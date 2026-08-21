// Package engine defines the music generation interface implemented by the
// concrete engines (the ACE-Step sidecar and test doubles).
package engine

import (
	"context"
	"time"

	"iar/internal/audio"
)

// InstrumentalLyrics is the lyric value that forces an instrumental track.
const InstrumentalLyrics = "[Instrumental]"

// Spec describes one track to generate.
type Spec struct {
	// Prompt is the music description (genre, mood, instrumentation).
	Prompt string
	// Lyrics holds real lyrics for vocal tracks, or InstrumentalLyrics.
	Lyrics string
	// SampleQuery, when non-empty, asks the engine to invent both the
	// detailed caption and lyrics from this natural-language description.
	// Used for vocal tracks when no lyric writer is available locally.
	SampleQuery string
	// Seconds is the requested track duration.
	Seconds int
	// Seed pins the random seed; -1 means random.
	Seed int64
}

// Vocal reports whether the spec asks for sung vocals.
func (s Spec) Vocal() bool {
	return s.SampleQuery != "" || (s.Lyrics != "" && s.Lyrics != InstrumentalLyrics)
}

// Track is one generated track in the internal PCM format.
type Track struct {
	// Samples are interleaved s16le samples at 48 kHz stereo.
	Samples []int16
	// Spec is the request that produced the track.
	Spec Spec
	// Prompt and Lyrics are the values the engine actually used (it may
	// enhance or invent them).
	Prompt string
	Lyrics string
	// Seed reports the seed(s) used, when known.
	Seed string
	// GenTime is how long generation took.
	GenTime time.Duration
	// FromLibrary marks a track loaded from the on-disk library rather
	// than freshly generated.
	FromLibrary bool
}

// Duration reports the track's play time.
func (t *Track) Duration() time.Duration {
	frames := len(t.Samples) / audio.Channels
	return time.Duration(float64(frames) / audio.SampleRate * float64(time.Second))
}

// Engine produces tracks. Implementations must be safe for use from one
// goroutine at a time; callers serialize.
type Engine interface {
	// Name identifies the engine in logs and status displays.
	Name() string
	// Generate renders one track, blocking until done or ctx ends.
	Generate(ctx context.Context, spec Spec) (*Track, error)
	// Ready reports whether the engine can currently generate.
	Ready() bool
}
