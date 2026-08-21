// Package audio implements the PCM plumbing shared by the whole
// application: the internal sample format, a non-blocking ring buffer,
// crossfading, noise synthesis, WAV decoding and the playback backends.
//
// Everything in the pipeline uses one format: signed 16-bit little-endian
// samples, 48 kHz, stereo. Anything else is converted at the boundary.
package audio

import "time"

// The internal PCM format.
const (
	// SampleRate is the internal sample rate in Hz.
	SampleRate = 48000
	// Channels is the internal channel count.
	Channels = 2
	// BytesPerSample is the size of one sample of one channel.
	BytesPerSample = 2
	// FrameBytes is the size of one frame (one sample per channel).
	FrameBytes = Channels * BytesPerSample
	// BytesPerSecond is the byte rate of the internal format.
	BytesPerSecond = SampleRate * FrameBytes
)

// FramesToBytes converts a frame count to a byte count.
func FramesToBytes(frames int) int { return frames * FrameBytes }

// BytesToFrames converts a byte count to a whole frame count.
func BytesToFrames(bytes int) int { return bytes / FrameBytes }

// DurationToBytes returns the byte count for d of audio, frame-aligned.
func DurationToBytes(d time.Duration) int {
	frames := int(d.Seconds() * SampleRate)
	return FramesToBytes(frames)
}

// BytesToDuration returns the play time of a byte count.
func BytesToDuration(n int) time.Duration {
	return time.Duration(float64(n) / BytesPerSecond * float64(time.Second))
}

// BytesToSamples reinterprets little-endian PCM bytes as int16 samples.
// Odd trailing bytes are ignored.
func BytesToSamples(b []byte) []int16 {
	s := make([]int16, len(b)/2)
	for i := range s {
		s[i] = int16(uint16(b[2*i]) | uint16(b[2*i+1])<<8)
	}
	return s
}

// SamplesToBytes serializes int16 samples as little-endian PCM bytes.
func SamplesToBytes(s []int16) []byte {
	b := make([]byte, len(s)*2)
	for i, v := range s {
		b[2*i] = byte(uint16(v))
		b[2*i+1] = byte(uint16(v) >> 8)
	}
	return b
}

// clampSample converts a float sample in [-1, 1] (soft-clamped) to int16.
func clampSample(v float64) int16 {
	switch {
	case v > 1:
		return 32767
	case v < -1:
		return -32768
	default:
		return int16(v * 32767)
	}
}

// clampInt clamps a 32-bit accumulation back into int16 range.
func clampInt(v int32) int16 {
	switch {
	case v > 32767:
		return 32767
	case v < -32768:
		return -32768
	default:
		return int16(v)
	}
}
