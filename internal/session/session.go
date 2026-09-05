// Package session persists what the user asked for: the steering context of
// the running stream, saved sessions on disk, and the built-in presets.
package session

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"iar/internal/audio"
)

// Mode says what kind of audio the session produces.
type Mode string

// Session modes.
const (
	// ModeMusic streams AI-generated music.
	ModeMusic Mode = "music"
	// ModeNoise streams synthesized noise (pink/white/brown).
	ModeNoise Mode = "noise"
)

// Entry is one steering input and how it was interpreted.
type Entry struct {
	// Time is when the input arrived.
	Time time.Time `json:"time"`
	// Raw is exactly what the user typed.
	Raw string `json:"raw"`
	// Interpreted is the acknowledgment shown to the user.
	Interpreted string `json:"interpreted"`
}

// Session is the full steering context of a stream. Every field the user
// influences is persisted so a session can be resumed with the same vibe.
type Session struct {
	// Name identifies the session in the store.
	Name string `json:"name"`
	// Preset names the built-in preset this session was seeded from.
	Preset  string    `json:"preset,omitempty"`
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`

	// Mode selects music or noise output.
	Mode Mode `json:"mode"`
	// BasePrompt is the starting music description.
	BasePrompt string `json:"base_prompt"`
	// NoiseColor is the active color while Mode is ModeNoise.
	NoiseColor string `json:"noise_color,omitempty"`
	// NoiseBed is the noise color used for instant startup audio and as
	// the graceful-degradation bed.
	NoiseBed string `json:"noise_bed"`
	// Vocal asks for sung vocals; LyricsTheme steers what they are about.
	Vocal       bool   `json:"vocal"`
	LyricsTheme string `json:"lyrics_theme,omitempty"`
	// LyricsGenerator names the lyric writer for vocal tracks
	// ("scribe", "smoothbrain"); empty uses the configured default.
	LyricsGenerator string `json:"lyrics_generator,omitempty"`
	// Languages says which of the configured vocal languages this
	// session sings in. A language the map says nothing about counts as
	// on, so adding one to the configuration starts using it right
	// away; switching them all off hands the choice back to the engine.
	Languages map[string]bool `json:"languages,omitempty"`

	// Spec is the structured steering state derived from the tweaks.
	// Sessions saved before it existed load with a nil Spec; it is then
	// rebuilt by replaying the recorded tweaks.
	Spec *PromptSpec `json:"spec,omitempty"`

	// Tweaks is the accumulated steering context (wiped by clear).
	Tweaks []Entry `json:"tweaks"`
	// History records every input ever typed, including cleared ones.
	History []Entry `json:"history"`

	// Named is set once a user gave the session a name. Sessions that
	// only ever had a generated name are the ones the bulk delete
	// offers to clear out.
	Named bool `json:"named,omitempty"`
	// ForkedFrom names the session this one branched off when something
	// about the sound was changed, so the state before that change is
	// still on disk to go back to.
	ForkedFrom string `json:"forked_from,omitempty"`
	// SeedExpanded records that the helper model has already fleshed
	// out a prompt-seeded session's description. Without it every
	// restart of that session asks again and quietly re-steers it.
	SeedExpanded bool `json:"seed_expanded,omitempty"`
	// LastPlayed is when the session was last the one playing (zero for
	// files written by older versions; Updated stands in then).
	LastPlayed time.Time `json:"last_played"`
}

// autoNameRe matches the generated names: "session-<date>-<time>",
// "prompt-<slug>-<time>" and "<preset>-<date>-<time>". It decides the
// auto-named status of sessions saved before Named existed.
var autoNameRe = regexp.MustCompile(`^(session-\d{8}-\d{6}|prompt-.+-\d{6}|[a-z0-9._-]+-\d{8}-\d{6})$`)

// AutoNamed reports whether the session still carries a generated name
// (never named by a user).
func (s *Session) AutoNamed() bool {
	return !s.Named && autoNameRe.MatchString(s.Name)
}

// Played is the best "last played" time: LastPlayed when recorded,
// otherwise the last update.
func (s *Session) Played() time.Time {
	if !s.LastPlayed.IsZero() {
		return s.LastPlayed
	}
	return s.Updated
}

// stampRe matches the generated time stamp at the end of an auto name,
// in both shapes the generators produce.
var stampRe = regexp.MustCompile(`-\d{8}-\d{6}$|-\d{6}$`)

// ForkName is the auto name for a session branched off base: base's own
// name with its old time stamp replaced by now's, so the lineage reads
// off the name and repeated forks do not grow it without end.
func ForkName(base string, now time.Time) string {
	stem := stampRe.ReplaceAllString(SanitizeName(base), "")
	if stem == "" || stem == "unnamed" {
		stem = "session"
	}
	return stem + "-" + now.Format("20060102-150405")
}

// New returns a fresh unnamed session with a pleasant default vibe.
func New() *Session {
	now := time.Now()
	return &Session{
		Name:       "session-" + now.Format("20060102-150405"),
		Created:    now,
		Updated:    now,
		Mode:       ModeMusic,
		BasePrompt: "lofi chill beats, mellow, warm analog, relaxed, soft piano, calm background music",
		NoiseBed:   string(audio.NoisePink),
	}
}

// FromPreset seeds a fresh session from a preset, keeping the preset
// read-only.
func FromPreset(p *Preset) *Session {
	s := New()
	s.Name = p.Name + "-" + s.Created.Format("20060102-150405")
	s.Preset = p.Name
	s.Mode = p.Mode
	s.BasePrompt = p.Prompt
	s.NoiseColor = p.NoiseColor
	s.NoiseBed = p.NoiseBed
	s.Vocal = p.Vocal
	s.LyricsTheme = p.LyricsTheme
	s.Spec = p.Spec.Clone()
	if s.NoiseBed == "" {
		s.NoiseBed = string(audio.NoisePink)
	}
	return s
}

// AddTweak records a steering input in both the active context and the
// permanent history.
func (s *Session) AddTweak(raw, interpreted string) {
	e := Entry{Time: time.Now(), Raw: raw, Interpreted: interpreted}
	s.Tweaks = append(s.Tweaks, e)
	s.History = append(s.History, e)
	s.Updated = e.Time
}

// RecordOnly records an input in the history without changing the steering
// context (used for mode switches and clear itself).
func (s *Session) RecordOnly(raw, interpreted string) {
	e := Entry{Time: time.Now(), Raw: raw, Interpreted: interpreted}
	s.History = append(s.History, e)
	s.Updated = e.Time
}

// Clear wipes the accumulated steering context (and vocal steering) while
// keeping the base prompt and history.
func (s *Session) Clear() {
	s.Tweaks = nil
	s.Spec = nil
	s.RecordOnly("clear", "steering context cleared")
}

// TweakPhrases returns the interpreted steering phrases in order.
func (s *Session) TweakPhrases() []string {
	out := make([]string, 0, len(s.Tweaks))
	for _, t := range s.Tweaks {
		out = append(out, t.Interpreted)
	}
	return out
}

// Describe returns a one-line summary for status displays.
func (s *Session) Describe() string {
	switch s.Mode {
	case ModeNoise:
		return s.NoiseColor + " noise"
	default:
		parts := []string{s.BasePrompt}
		if n := len(s.Tweaks); n > 0 {
			parts = append(parts, fmt.Sprintf("+%d tweaks", n))
		}
		if s.Vocal {
			parts = append(parts, "vocals")
		}
		return strings.Join(parts, " ")
	}
}

// Snapshot returns a copy safe to read from another goroutine.
func (s *Session) Snapshot() *Session {
	cp := *s
	cp.Spec = s.Spec.Clone()
	cp.Tweaks = append([]Entry(nil), s.Tweaks...)
	cp.History = append([]Entry(nil), s.History...)
	if s.Languages != nil {
		cp.Languages = make(map[string]bool, len(s.Languages))
		for k, v := range s.Languages {
			cp.Languages[k] = v
		}
	}
	return &cp
}
