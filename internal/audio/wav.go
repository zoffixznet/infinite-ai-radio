package audio

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// WAV holds a decoded WAV file, converted to interleaved int16 samples but
// still at its original rate and channel count.
type WAV struct {
	// Samples are interleaved int16 samples.
	Samples []int16
	// Rate is the sample rate in Hz.
	Rate int
	// Channels is the channel count.
	Channels int
}

// DecodeWAV parses a WAV file, accepting 16/24/32-bit integer and 32-bit
// float PCM.
func DecodeWAV(data []byte) (*WAV, error) {
	if len(data) < 12 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return nil, errors.New("not a RIFF/WAVE file")
	}
	var (
		format, channels, bits int
		rate                   int
		raw                    []byte
		haveFmt                bool
	)
	for off := 12; off+8 <= len(data); {
		id := string(data[off : off+4])
		size := int(binary.LittleEndian.Uint32(data[off+4 : off+8]))
		body := off + 8
		if body+size > len(data) {
			size = len(data) - body // tolerate truncated final chunk
		}
		switch id {
		case "fmt ":
			if size < 16 {
				return nil, errors.New("wav: short fmt chunk")
			}
			format = int(binary.LittleEndian.Uint16(data[body : body+2]))
			channels = int(binary.LittleEndian.Uint16(data[body+2 : body+4]))
			rate = int(binary.LittleEndian.Uint32(data[body+4 : body+8]))
			bits = int(binary.LittleEndian.Uint16(data[body+14 : body+16]))
			if format == 0xFFFE && size >= 40 { // WAVE_FORMAT_EXTENSIBLE
				format = int(binary.LittleEndian.Uint16(data[body+24 : body+26]))
			}
			haveFmt = true
		case "data":
			raw = data[body : body+size]
		}
		if size%2 == 1 {
			size++ // chunks are word-aligned
		}
		off = body + size
	}
	if !haveFmt || raw == nil {
		return nil, errors.New("wav: missing fmt or data chunk")
	}
	if channels < 1 {
		return nil, errors.New("wav: no channels")
	}
	samples, err := convertToInt16(raw, format, bits)
	if err != nil {
		return nil, err
	}
	return &WAV{Samples: samples, Rate: rate, Channels: channels}, nil
}

// convertToInt16 converts raw sample bytes to int16.
func convertToInt16(raw []byte, format, bits int) ([]int16, error) {
	const (
		fmtPCM   = 1
		fmtFloat = 3
	)
	switch {
	case format == fmtPCM && bits == 16:
		return BytesToSamples(raw), nil
	case format == fmtPCM && bits == 24:
		n := len(raw) / 3
		out := make([]int16, n)
		for i := 0; i < n; i++ {
			v := int32(raw[3*i]) | int32(raw[3*i+1])<<8 | int32(raw[3*i+2])<<16
			v = v << 8 >> 8 // sign-extend 24 -> 32
			out[i] = int16(v >> 8)
		}
		return out, nil
	case format == fmtPCM && bits == 32:
		n := len(raw) / 4
		out := make([]int16, n)
		for i := 0; i < n; i++ {
			v := int32(binary.LittleEndian.Uint32(raw[4*i : 4*i+4]))
			out[i] = int16(v >> 16)
		}
		return out, nil
	case format == fmtFloat && bits == 32:
		n := len(raw) / 4
		out := make([]int16, n)
		for i := 0; i < n; i++ {
			f := math.Float32frombits(binary.LittleEndian.Uint32(raw[4*i : 4*i+4]))
			out[i] = clampSample(float64(f))
		}
		return out, nil
	default:
		return nil, fmt.Errorf("wav: unsupported format %d with %d bits", format, bits)
	}
}

// ToInternal converts the decoded WAV to the internal format's channel
// layout. The sample rate must already match; callers resample beforehand
// when it does not.
func (w *WAV) ToInternal() ([]int16, error) {
	if w.Rate != SampleRate {
		return nil, fmt.Errorf("wav: sample rate %d needs resampling to %d", w.Rate, SampleRate)
	}
	switch w.Channels {
	case Channels:
		return w.Samples, nil
	case 1:
		out := make([]int16, len(w.Samples)*Channels)
		for i, v := range w.Samples {
			out[2*i] = v
			out[2*i+1] = v
		}
		return out, nil
	default:
		return nil, fmt.Errorf("wav: unsupported channel count %d", w.Channels)
	}
}

// EncodeWAV serializes interleaved int16 samples in the internal format as a
// 16-bit PCM WAV file.
func EncodeWAV(samples []int16) []byte {
	dataLen := len(samples) * 2
	buf := make([]byte, 44+dataLen)
	copy(buf[0:4], "RIFF")
	binary.LittleEndian.PutUint32(buf[4:8], uint32(36+dataLen))
	copy(buf[8:12], "WAVE")
	copy(buf[12:16], "fmt ")
	binary.LittleEndian.PutUint32(buf[16:20], 16)
	binary.LittleEndian.PutUint16(buf[20:22], 1) // PCM
	binary.LittleEndian.PutUint16(buf[22:24], uint16(Channels))
	binary.LittleEndian.PutUint32(buf[24:28], uint32(SampleRate))
	binary.LittleEndian.PutUint32(buf[28:32], uint32(BytesPerSecond))
	binary.LittleEndian.PutUint16(buf[32:34], uint16(FrameBytes))
	binary.LittleEndian.PutUint16(buf[34:36], 16)
	copy(buf[36:40], "data")
	binary.LittleEndian.PutUint32(buf[40:44], uint32(dataLen))
	copy(buf[44:], SamplesToBytes(samples))
	return buf
}
