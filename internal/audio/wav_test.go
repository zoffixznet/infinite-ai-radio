package audio

import (
	"encoding/binary"
	"math"
	"testing"
)

func TestWAVRoundTrip(t *testing.T) {
	samples := make([]int16, 4800*Channels)
	for i := range samples {
		samples[i] = int16(1000 * math.Sin(float64(i)/50))
	}
	data := EncodeWAV(samples)
	w, err := DecodeWAV(data)
	if err != nil {
		t.Fatal(err)
	}
	if w.Rate != SampleRate || w.Channels != Channels {
		t.Fatalf("decoded rate/channels = %d/%d", w.Rate, w.Channels)
	}
	got, err := w.ToInternal()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(samples) {
		t.Fatalf("length %d != %d", len(got), len(samples))
	}
	for i := range got {
		if got[i] != samples[i] {
			t.Fatalf("sample %d: %d != %d", i, got[i], samples[i])
		}
	}
}

func TestDecodeWAVFloat32(t *testing.T) {
	// Hand-build a float32 WAV: 4 frames of 0.5.
	const frames = 4
	dataLen := frames * Channels * 4
	buf := make([]byte, 44+dataLen)
	copy(buf[0:4], "RIFF")
	binary.LittleEndian.PutUint32(buf[4:8], uint32(36+dataLen))
	copy(buf[8:12], "WAVE")
	copy(buf[12:16], "fmt ")
	binary.LittleEndian.PutUint32(buf[16:20], 16)
	binary.LittleEndian.PutUint16(buf[20:22], 3) // IEEE float
	binary.LittleEndian.PutUint16(buf[22:24], Channels)
	binary.LittleEndian.PutUint32(buf[24:28], SampleRate)
	binary.LittleEndian.PutUint32(buf[28:32], SampleRate*Channels*4)
	binary.LittleEndian.PutUint16(buf[32:34], Channels*4)
	binary.LittleEndian.PutUint16(buf[34:36], 32)
	copy(buf[36:40], "data")
	binary.LittleEndian.PutUint32(buf[40:44], uint32(dataLen))
	for i := 0; i < frames*Channels; i++ {
		binary.LittleEndian.PutUint32(buf[44+4*i:], math.Float32bits(0.5))
	}
	w, err := DecodeWAV(buf)
	if err != nil {
		t.Fatal(err)
	}
	got, err := w.ToInternal()
	if err != nil {
		t.Fatal(err)
	}
	want := int16(16383) // 0.5 * 32767, truncated
	if got[0] != want {
		t.Fatalf("float sample decoded to %d; want %d", got[0], want)
	}
}

func TestDecodeWAVMonoUpmix(t *testing.T) {
	// 16-bit mono WAV.
	samples := []int16{100, -100, 3000}
	dataLen := len(samples) * 2
	buf := make([]byte, 44+dataLen)
	copy(buf[0:4], "RIFF")
	binary.LittleEndian.PutUint32(buf[4:8], uint32(36+dataLen))
	copy(buf[8:12], "WAVE")
	copy(buf[12:16], "fmt ")
	binary.LittleEndian.PutUint32(buf[16:20], 16)
	binary.LittleEndian.PutUint16(buf[20:22], 1)
	binary.LittleEndian.PutUint16(buf[22:24], 1) // mono
	binary.LittleEndian.PutUint32(buf[24:28], SampleRate)
	binary.LittleEndian.PutUint32(buf[28:32], SampleRate*2)
	binary.LittleEndian.PutUint16(buf[32:34], 2)
	binary.LittleEndian.PutUint16(buf[34:36], 16)
	copy(buf[36:40], "data")
	binary.LittleEndian.PutUint32(buf[40:44], uint32(dataLen))
	copy(buf[44:], SamplesToBytes(samples))

	w, err := DecodeWAV(buf)
	if err != nil {
		t.Fatal(err)
	}
	got, err := w.ToInternal()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(samples)*2 || got[0] != 100 || got[1] != 100 || got[4] != 3000 {
		t.Fatalf("mono upmix wrong: %v", got)
	}
}

func TestDecodeWAVRejectsGarbage(t *testing.T) {
	if _, err := DecodeWAV([]byte("definitely not a wav file")); err == nil {
		t.Fatal("garbage accepted")
	}
}
