// Package player runs the continuous stream: a generate-ahead worker that
// keeps tracks buffered, a mixer that joins them with equal-power
// crossfades (degrading gracefully to looping or a noise bed), and a pump
// that feeds the audio backend. It also carries the user-facing controls:
// steering, sessions, presets, skip, pause, volume and export priority.
package player

import (
	"sync/atomic"

	"iar/internal/audio"
	"iar/internal/engine"
)

// source produces interleaved stereo samples for the mixer.
type source interface {
	// read returns exactly frames*Channels samples, short only when the
	// source is exhausted.
	read(frames int) []int16
	// remaining reports frames left, or -1 for endless sources.
	remaining() int
	// label describes the source for status displays.
	label() string
}

// trackSource plays one generated track from memory. The position is
// atomic because status snapshots read it from another goroutine.
type trackSource struct {
	track *engine.Track
	pos   atomic.Int64 // frames consumed
	name  string
	// loop marks a source the mixer installed for want of anything
	// newer, so status surfaces can say so instead of presenting it as
	// a fresh track.
	loop bool
}

func newTrackSource(t *engine.Track, name string) *trackSource {
	return &trackSource{track: t, name: name}
}

func (s *trackSource) read(frames int) []int16 {
	pos := int(s.pos.Load())
	total := len(s.track.Samples) / audio.Channels
	if pos >= total {
		return nil
	}
	if pos+frames > total {
		frames = total - pos
	}
	out := s.track.Samples[pos*audio.Channels : (pos+frames)*audio.Channels]
	s.pos.Store(int64(pos + frames))
	return out
}

func (s *trackSource) remaining() int {
	total := len(s.track.Samples) / audio.Channels
	pos := int(s.pos.Load())
	if pos >= total {
		return 0
	}
	return total - pos
}

func (s *trackSource) label() string { return s.name }

// elapsedFrames reports how far playback is into the track.
func (s *trackSource) elapsedFrames() int { return int(s.pos.Load()) }

// noiseSource synthesizes endless noise.
type noiseSource struct {
	gen  *audio.NoiseGenerator
	name string
}

func newNoiseSource(color audio.NoiseColor, amp float64, name string) *noiseSource {
	return &noiseSource{gen: audio.NewNoiseGenerator(color, amp), name: name}
}

func (s *noiseSource) read(frames int) []int16 { return s.gen.Generate(frames) }
func (s *noiseSource) remaining() int          { return -1 }
func (s *noiseSource) label() string           { return s.name }

// silenceSource produces endless silence: the default startup sound while
// the first track is prepared (progress is shown instead of audio).
type silenceSource struct{}

func (silenceSource) read(frames int) []int16 { return make([]int16, frames*audio.Channels) }
func (silenceSource) remaining() int          { return -1 }
func (silenceSource) label() string           { return "silence (preparing music)" }
