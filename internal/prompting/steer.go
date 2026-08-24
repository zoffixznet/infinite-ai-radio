// Package prompting turns the session's steering context into concrete
// generation requests: it interprets free-text tweaks, merges them into
// prompts (optionally polished by a local Ollama model) and produces lyrics
// for vocal tracks.
package prompting

import (
	"fmt"
	"regexp"
	"strings"

	"iar/internal/session"
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

// Steer interprets one free-text steering input, updates the session's
// structured spec accordingly and returns an acknowledgment. Inputs
// that change nothing ("less guitars" when guitars are already avoided)
// report ContextChanged false so the queue survives. The caller
// persists the session afterwards.
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
		if s.Mode == session.ModeNoise && s.NoiseColor == color {
			return Ack{Text: color + " noise is already playing"}
		}
		s.Mode = session.ModeNoise
		s.NoiseColor = color
		ack := fmt.Sprintf("switching to %s noise", color)
		s.RecordOnly(raw, ack)
		return Ack{Text: ack, ContextChanged: true}
	}

	// Any other input while in noise mode returns to music.
	prefix := ""
	modeChanged := false
	if s.Mode == session.ModeNoise {
		s.Mode = session.ModeMusic
		prefix = "back to music; "
		modeChanged = true
	}

	// Vocal routing.
	if vocalOffRe.MatchString(text) {
		if !s.Vocal && !modeChanged {
			return Ack{Text: "already instrumental"}
		}
		s.Vocal = false
		s.LyricsTheme = ""
		ack := prefix + "vocals off, instrumental from the next track"
		s.RecordOnly(raw, ack)
		return Ack{Text: ack, ContextChanged: true}
	}
	if vocalOnRe.MatchString(text) && !vocalOffRe.MatchString(text) {
		changed := !s.Vocal || modeChanged
		s.Vocal = true
		if m := aboutRe.FindStringSubmatch(text); m != nil {
			theme := strings.TrimSpace(m[1])
			if theme != s.LyricsTheme {
				s.LyricsTheme = theme
				changed = true
			}
		}
		// Language requests inside a vocal input still land in the spec.
		EnsureSpec(s)
		before := s.Spec.Clone()
		applyText(s, text)
		if !before.Equal(s.Spec) {
			changed = true
		}
		if !changed {
			return Ack{Text: "vocals are already on"}
		}
		ack := prefix + "vocals on"
		if s.LyricsTheme != "" {
			ack += " (theme: " + s.LyricsTheme + ")"
		}
		s.RecordOnly(raw, ack)
		return Ack{Text: ack, ContextChanged: true}
	}

	// Structured steering: mutate the spec with real semantics.
	EnsureSpec(s)
	before := s.Spec.Clone()
	descr := applyText(s, text)
	if before.Equal(s.Spec) && !modeChanged {
		return Ack{Text: "already in effect; nothing changed"}
	}
	if descr == "" {
		descr = text
	}
	s.AddTweak(raw, descr)
	return Ack{Text: prefix + "steering: " + descr, ContextChanged: true}
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
