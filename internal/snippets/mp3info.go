package snippets

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf16"
)

// Info is what the catalog reads back from a saved MP3.
type Info struct {
	// Title and Album are the ID3 tags the save command wrote (prompt
	// and tag).
	Title string
	Album string
	// Duration is the play time, from the encoder's frame-count header
	// when present, otherwise estimated from the bitrate.
	Duration time.Duration
}

// ReadInfo parses the ID3v2 tag and first audio frame of an MP3 file.
// It reads only the tag and a few KB of audio, so listing a large
// library stays cheap.
func ReadInfo(path string) (Info, error) {
	f, err := os.Open(path)
	if err != nil {
		return Info{}, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return Info{}, err
	}
	var info Info
	audioStart := int64(0)
	header := make([]byte, 10)
	if _, err := io.ReadFull(f, header); err == nil && bytes.Equal(header[:3], []byte("ID3")) {
		size := syncsafe(header[6:10])
		tag := make([]byte, size)
		if _, err := io.ReadFull(f, tag); err != nil {
			return Info{}, err
		}
		info.Title, info.Album = parseID3Frames(header[3], header[5], tag)
		audioStart = 10 + int64(size)
		if header[5]&0x10 != 0 { // footer present (v2.4)
			audioStart += 10
		}
	}
	audioBytes := fi.Size() - audioStart
	if audioBytes > 128 {
		// An ID3v1 trailer is not audio.
		trailer := make([]byte, 3)
		if _, err := f.ReadAt(trailer, fi.Size()-128); err == nil && string(trailer) == "TAG" {
			audioBytes -= 128
		}
	}
	probe := make([]byte, 8192)
	n, _ := f.ReadAt(probe, audioStart)
	dur, err := mp3Duration(probe[:n], audioBytes)
	if err != nil {
		return Info{}, err
	}
	info.Duration = dur
	return info, nil
}

func syncsafe(b []byte) int {
	return int(b[0]&0x7f)<<21 | int(b[1]&0x7f)<<14 | int(b[2]&0x7f)<<7 | int(b[3]&0x7f)
}

// parseID3Frames walks the frames of an ID3v2.3/2.4 tag body and returns
// the title (TIT2) and album (TALB) text.
func parseID3Frames(version, flags byte, body []byte) (title, album string) {
	pos := 0
	if flags&0x40 != 0 && len(body) >= 4 { // extended header
		if version == 4 {
			pos = syncsafe(body[:4])
		} else {
			pos = int(binary.BigEndian.Uint32(body[:4])) + 4
		}
	}
	for pos+10 <= len(body) {
		id := string(body[pos : pos+4])
		if id[0] == 0 {
			break // padding
		}
		var size int
		if version == 4 {
			size = syncsafe(body[pos+4 : pos+8])
		} else {
			size = int(binary.BigEndian.Uint32(body[pos+4 : pos+8]))
		}
		pos += 10
		if size < 0 || pos+size > len(body) {
			break
		}
		data := body[pos : pos+size]
		pos += size
		switch id {
		case "TIT2":
			title = decodeText(data)
		case "TALB":
			album = decodeText(data)
		}
	}
	return title, album
}

// decodeText decodes an ID3 text frame (encoding byte + text).
func decodeText(data []byte) string {
	if len(data) < 1 {
		return ""
	}
	enc, text := data[0], data[1:]
	var s string
	switch enc {
	case 1, 2: // UTF-16 with BOM, UTF-16BE
		bigEndian := true
		if enc == 1 && len(text) >= 2 {
			switch {
			case text[0] == 0xFF && text[1] == 0xFE:
				bigEndian = false
				text = text[2:]
			case text[0] == 0xFE && text[1] == 0xFF:
				text = text[2:]
			}
		}
		u := make([]uint16, 0, len(text)/2)
		for i := 0; i+1 < len(text); i += 2 {
			if bigEndian {
				u = append(u, uint16(text[i])<<8|uint16(text[i+1]))
			} else {
				u = append(u, uint16(text[i+1])<<8|uint16(text[i]))
			}
		}
		s = string(utf16.Decode(u))
	case 3: // UTF-8
		s = string(text)
	default: // ISO-8859-1
		r := make([]rune, len(text))
		for i, b := range text {
			r[i] = rune(b)
		}
		s = string(r)
	}
	return strings.TrimRight(s, "\x00")
}

var (
	bitratesV1 = [16]int{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 0}
	bitratesV2 = [16]int{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160, 0}
	ratesV1    = [4]int{44100, 48000, 32000, 0}
	ratesV2    = [4]int{22050, 24000, 16000, 0}
	ratesV25   = [4]int{11025, 12000, 8000, 0}
)

// mp3Duration finds the first MPEG Layer III frame in probe and derives
// the duration from a Xing/Info frame-count header, or from the bitrate
// and the audio byte count when no such header exists.
func mp3Duration(probe []byte, audioBytes int64) (time.Duration, error) {
	for i := 0; i+4 <= len(probe); i++ {
		if probe[i] != 0xFF || probe[i+1]&0xE0 != 0xE0 {
			continue
		}
		h := binary.BigEndian.Uint32(probe[i:])
		versionBits := (h >> 19) & 3
		layer := (h >> 17) & 3
		brIdx := (h >> 12) & 15
		srIdx := (h >> 10) & 3
		mono := (h>>6)&3 == 3
		if versionBits == 1 || layer != 1 || brIdx == 0 || brIdx == 15 || srIdx == 3 {
			continue
		}
		var bitrate, rate, spf, side int
		switch versionBits {
		case 3: // MPEG1
			bitrate, rate, spf = bitratesV1[brIdx], ratesV1[srIdx], 1152
			side = 32
			if mono {
				side = 17
			}
		case 2: // MPEG2
			bitrate, rate, spf = bitratesV2[brIdx], ratesV2[srIdx], 576
			side = 17
			if mono {
				side = 9
			}
		default: // MPEG2.5
			bitrate, rate, spf = bitratesV2[brIdx], ratesV25[srIdx], 576
			side = 17
			if mono {
				side = 9
			}
		}
		// A Xing/Info header (written by every VBR and most CBR
		// encoders) carries the exact frame count.
		x := i + 4 + side
		if x+16 <= len(probe) {
			tag := string(probe[x : x+4])
			if tag == "Xing" || tag == "Info" {
				flags := binary.BigEndian.Uint32(probe[x+4:])
				if flags&1 != 0 {
					frames := binary.BigEndian.Uint32(probe[x+8:])
					secs := float64(frames) * float64(spf) / float64(rate)
					return time.Duration(secs * float64(time.Second)), nil
				}
			}
		}
		secs := float64(audioBytes) * 8 / float64(bitrate*1000)
		return time.Duration(secs * float64(time.Second)), nil
	}
	return 0, errors.New("no MPEG audio frame found")
}
