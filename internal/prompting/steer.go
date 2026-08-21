// Package prompting turns the session's steering context into concrete
// generation requests: it interprets free-text tweaks, merges them into
// prompts (optionally polished by a local Ollama model) and produces lyrics
// for vocal tracks.
package prompting

import (
	"fmt"
	"regexp"
	"strings"

	"bgm/internal/session"
)

// Ack describes how a steering input was understood.
type Ack struct {
	// Text is the acknowledgment shown to the user.
	Text string
	// ContextChanged reports whether the generation context changed (the
	// queue of not-yet-played tracks should be refreshed).
	ContextChanged bool
}

var noiseRe = regexp.MustCompile(`\b(pink|white|brown)?[ -]?noise\b`)

// aboutRe captures a lyric theme from phrases like "vocals about winning".
var aboutRe = regexp.MustCompile(`\babout\s+(.+)$`)

// vocalOffRe matches requests to drop vocals.
var vocalOffRe = regexp.MustCompile(`\b(no|without|stop|remove|drop)\b.*\b(vocals?|singing|lyrics|voice)\b|\binstrumental\b`)

// vocalOnRe matches requests for vocals.
var vocalOnRe = regexp.MustCompile(`\b(vocals?|singing|sing|lyrics|voice)\b`)

// tweakTable maps recognized steering words to prompt phrases. First match
// wins; unmatched input is passed through as-is, which the music model
// usually understands fine.
var tweakTable = []struct {
	re     *regexp.Regexp
	phrase string
}{
	{regexp.MustCompile(`\b(more )?(energetic|energy|upbeat|intense|harder|pumped?)\b`), "more energetic, driving, higher energy"},
	{regexp.MustCompile(`\b(calmer|calm|chill(er)?|relax(ed|ing)?|softer|gentler|mellow)\b`), "calmer, softer, more gentle"},
	{regexp.MustCompile(`\b(faster|speed up|quicker)\b`), "faster tempo"},
	{regexp.MustCompile(`\b(slower|slow(ed)? down)\b`), "slower tempo"},
	{regexp.MustCompile(`\b(happier|happy|brighter|cheerful)\b`), "brighter, uplifting, major key"},
	{regexp.MustCompile(`\b(darker|sadder|sad|moody|melancholi[ck])\b`), "darker, melancholic, minor key"},
	{regexp.MustCompile(`\b(dreamy|spacey|atmospheric)\b`), "dreamy, atmospheric, spacious"},
	{regexp.MustCompile(`\b(heavier|aggressive|distorted)\b`), "heavier, aggressive, distorted"},
	{regexp.MustCompile(`\bmore (bass|low end)\b`), "deep bass, strong low end"},
	{regexp.MustCompile(`\b(no|less|fewer|drop the) drums?\b`), "beatless, no drums, no percussion"},
}

// Steer interprets one free-text steering input, updates the session
// accordingly and returns an acknowledgment. The caller persists the
// session afterwards.
func Steer(s *session.Session, raw string) Ack {
	text := strings.ToLower(strings.TrimSpace(raw))
	if text == "" {
		return Ack{Text: "nothing to do"}
	}

	// Noise routing: "generate pink noise", "white noise please", ...
	if m := noiseRe.FindStringSubmatch(text); m != nil {
		color := m[1]
		if color == "" {
			color = "pink"
		}
		s.Mode = session.ModeNoise
		s.NoiseColor = color
		ack := fmt.Sprintf("switching to %s noise", color)
		s.RecordOnly(raw, ack)
		return Ack{Text: ack, ContextChanged: true}
	}

	// Any other input while in noise mode returns to music.
	prefix := ""
	if s.Mode == session.ModeNoise {
		s.Mode = session.ModeMusic
		prefix = "back to music; "
	}

	// Vocal routing.
	if vocalOffRe.MatchString(text) {
		s.Vocal = false
		s.LyricsTheme = ""
		ack := prefix + "vocals off, instrumental from the next track"
		s.RecordOnly(raw, ack)
		return Ack{Text: ack, ContextChanged: true}
	}
	if vocalOnRe.MatchString(text) {
		s.Vocal = true
		if m := aboutRe.FindStringSubmatch(text); m != nil {
			s.LyricsTheme = strings.TrimSpace(m[1])
		}
		ack := prefix + "vocals on"
		if s.LyricsTheme != "" {
			ack += " (theme: " + s.LyricsTheme + ")"
		}
		s.RecordOnly(raw, ack)
		return Ack{Text: ack, ContextChanged: true}
	}

	// Plain tweak: map to a known phrase or pass through.
	phrase := text
	for _, t := range tweakTable {
		if t.re.MatchString(text) {
			phrase = t.phrase
			break
		}
	}
	s.AddTweak(raw, phrase)
	ack := prefix + "steering with: " + phrase
	return Ack{Text: ack, ContextChanged: true}
}

// SessionFromPrompt creates a fresh session seeded from a free-text
// prompt (prompt-first startup and the in-app `new` command). Vocal
// requests inside the prompt ("... with vocals about winning") are
// honored.
func SessionFromPrompt(prompt string) *session.Session {
	s := session.New()
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return s
	}
	text := strings.ToLower(prompt)
	if m := noiseRe.FindStringSubmatch(text); m != nil {
		color := m[1]
		if color == "" {
			color = "pink"
		}
		s.Mode = session.ModeNoise
		s.NoiseColor = color
		return s
	}
	if vocalOnRe.MatchString(text) && !vocalOffRe.MatchString(text) {
		s.Vocal = true
		if m := aboutRe.FindStringSubmatch(text); m != nil {
			s.LyricsTheme = strings.TrimSpace(m[1])
		}
	}
	s.BasePrompt = prompt
	s.Name = "prompt-" + session.SanitizeName(shorten(prompt, 32)) + "-" + s.Created.Format("150405")
	return s
}

// shorten trims s to at most n bytes without cutting mid-word when
// possible.
func shorten(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := s[:n]
	if i := strings.LastIndex(cut, " "); i > n/2 {
		cut = cut[:i]
	}
	return cut
}
