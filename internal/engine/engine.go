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

	// Structured constraints the engine honours directly (all optional;
	// zero values leave the choice to the model).
	BPM           int
	KeyScale      string // "C major", "F# minor", ...
	TimeSignature string // "2", "3", "4" or "6"
	VocalLanguage string // ISO code for sung vocals
	// VocalLanguageName is the listener's own wording for that
	// language, kept for display: languages the engine has no tag for
	// are still sung, and only this field remembers which one it was.
	VocalLanguageName string
	// NegativePrompt lists what the music must avoid; it drives the
	// engine's planner-side negative conditioning.
	NegativePrompt string
	// LMCfgScale raises planner guidance when negatives are present
	// (0 keeps the engine default).
	LMCfgScale float64
}

// Vocal reports whether the spec asks for sung vocals.
func (s Spec) Vocal() bool {
	return s.SampleQuery != "" || (s.Lyrics != "" && s.Lyrics != InstrumentalLyrics)
}

// Track is one generated track in the internal PCM format.
type Track struct {
	// ID identifies the track for remote serving; the player assigns it
	// when the track enters the stream.
	ID string
	// Samples are interleaved s16le samples at 48 kHz stereo.
	Samples []int16
	// Spec is the request that produced the track.
	Spec Spec
	// Prompt and Lyrics are the values the engine actually used (it may
	// enhance or invent them).
	Prompt string
	Lyrics string
	// Title and Subtitle are the short display names shown on lock
	// screens and car displays (a 2-4 word name and a genre/mood line).
	// The player fills them before the track enters the stream.
	Title    string
	Subtitle string
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
