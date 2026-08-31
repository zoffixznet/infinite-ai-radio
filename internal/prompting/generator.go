package prompting

import (
	"context"
	"strings"
	"time"
)

// This file defines the pluggable lyric generators. Each generator
// turns one track's steering context into structure-tagged lyrics for
// the engine to sing; the builder runs them strictly in the background
// and falls back to the engine's own planner when none has finished in
// time. Generators are selected per session (the `lyrics` command and
// the phone remote switch them) with a configurable default.

// LLM is the chat surface lyric generators write through. *Ollama
// implements it; tests substitute fakes.
type LLM interface {
	Chat(ctx context.Context, system, user string) (string, error)
	ChatJSON(ctx context.Context, system, user string, schema any) (string, error)
	ChatWith(ctx context.Context, system, user string, opts ChatOpts) (string, error)
}

// The Ollama client is the production LLM.
var _ LLM = (*Ollama)(nil)

// LyricsRequest is everything a lyric generator may draw on for one
// track.
type LyricsRequest struct {
	// Style is the rendered music description (genre, mood,
	// instruments).
	Style string
	// Theme is what the song should be about; empty means "match the
	// mood of the music".
	Theme string
	// Seconds is the track duration the lyrics must fill.
	Seconds int
	// Language is the engine's language tag for sung vocals; empty
	// means the engine was given no tag.
	Language string
	// LanguageName is the language to write in, in words ("Russian",
	// "Bisaya (Cebuano)"). Empty means English. It is set even for
	// languages the engine has no tag for: the words are still written
	// in that language, the engine just sings them untagged.
	LanguageName string
	// AvoidHooks lists hook lines from recent tracks in the same
	// steering context, so consecutive songs don't share a chorus.
	AvoidHooks []string
}

// English reports whether the lyrics should be written in English,
// which is the default and the only language the rhyme and syllable
// machinery understands.
func (r LyricsRequest) English() bool {
	if r.LanguageName != "" {
		return strings.EqualFold(r.LanguageName, "English")
	}
	return r.Language == "" || r.Language == "en"
}

// WriteIn names the language to write in, always in words, so the
// prompt can say "write in Bisaya (Cebuano)" rather than quoting a tag
// the engine has never heard of.
func (r LyricsRequest) WriteIn() string {
	if r.LanguageName != "" {
		return r.LanguageName
	}
	if n := LanguageName(r.Language); n != "" {
		return n
	}
	return "English"
}

// LyricsGenerator writes the lyrics for one track.
type LyricsGenerator interface {
	// Name identifies the generator in commands and session files
	// (lowercase, one word).
	Name() string
	// Blurb is a one-line description for listings.
	Blurb() string
	// Timeout bounds one Generate call.
	Timeout() time.Duration
	// Generate returns structure-tagged lyrics for the request.
	Generate(ctx context.Context, llm LLM, req LyricsRequest) (string, error)
}

// DefaultGeneratorName is used when neither the session nor the
// configuration picks a lyric generator.
const DefaultGeneratorName = "scribe"

// registry is the ordered list of lyric generators, default first.
var registry = []LyricsGenerator{&Scribe{}, &Smoothbrain{}}

// Generators lists the available lyric generators.
func Generators() []LyricsGenerator { return registry }

// GeneratorByName resolves a generator name, case-insensitively.
func GeneratorByName(name string) (LyricsGenerator, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, g := range registry {
		if g.Name() == name {
			return g, true
		}
	}
	return nil, false
}
