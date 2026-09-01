package prompting

import (
	"context"
	"encoding/json"
	"strings"
	"unicode"
)

// This file names tracks for display on lock screens and car displays:
// a deterministic fallback computed for every track, plus an optional
// helper-model call that proposes an evocative short name in the
// background. The fallback must look finished on its own, because the
// helper is optional and self-disables.

// TrackTitle derives a short display title and subtitle from a track's
// generation prompt, deterministically. The title is the title-cased
// first comma segment (word-aware shortened); the subtitle names the
// genre and mood from the following segments, at most six words.
func TrackTitle(prompt string) (title, subtitle string) {
	var segs []string
	for _, seg := range strings.Split(prompt, ",") {
		if seg = strings.TrimSpace(seg); seg != "" {
			segs = append(segs, seg)
		}
	}
	if len(segs) == 0 {
		return "Generated Track", ""
	}
	title = titleCase(shorten(segs[0], 32))
	words := 0
	var parts []string
	for _, seg := range segs[1:] {
		n := len(strings.Fields(seg))
		if words+n > 6 {
			break
		}
		parts = append(parts, seg)
		words += n
	}
	return title, strings.Join(parts, ", ")
}

// titleCase capitalizes the first letter of every word.
func titleCase(s string) string {
	words := strings.Fields(s)
	for i, w := range words {
		r := []rune(w)
		r[0] = unicode.ToUpper(r[0])
		words[i] = string(r)
	}
	return strings.Join(words, " ")
}

// titleSystem instructs the helper model to name a track.
const titleSystem = `You name radio tracks. Given a music-generation
prompt, reply with JSON {"title", "subtitle"}. title: an evocative 2-4
word song name, no quotes, not a list of genres. subtitle: at most 6
words naming the genre and mood. Never mention prompts or AI.`

// titleSongSystem names one specific song from its own words, which is
// what keeps two songs from the same station from sharing a name.
const titleSongSystem = `You name songs. Given a song's music style and
its lyrics, reply with JSON {"title", "subtitle"}. title: a distinctive
2-4 word name drawn from this song's own words and imagery - its hook
line or sharpest image, in the language the lyrics are written in;
never a genre label, never generic. subtitle: at most 6 words naming
the genre and mood, in English. Never mention prompts or AI.`

// titleSchema constrains the helper's reply.
var titleSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"title":    map[string]any{"type": "string"},
		"subtitle": map[string]any{"type": "string"},
	},
	"required": []any{"title", "subtitle"},
}

// TitleAsync asks the helper model for a short track name in the
// background, cached per prompt. Never blocks; does nothing when the
// helper is unusable.
func (b *Builder) TitleAsync(prompt string) {
	if prompt == "" || !b.helperUsable() {
		return
	}
	key := "n|" + prompt
	if _, ok := b.lookup(key); ok {
		return
	}
	b.fillAsync(key, func(ctx context.Context) (string, error) {
		// Keep-alive so this call and the lyric write clustered around
		// the same track share one model load.
		return b.ollama.ChatWith(ctx, titleSystem, "Music prompt: "+prompt, ChatOpts{
			Format: titleSchema, KeepAliveSeconds: scribeKeepAlive,
		})
	})
}

// TitleSongAsync names one specific song in the background, keyed by
// the caller's per-song key and fed the song's actual words - so every
// song gets its own name instead of sharing its steering context's.
// Never blocks; does nothing when the helper is unusable.
func (b *Builder) TitleSongAsync(key, caption, lyrics string) {
	if key == "" || lyrics == "" || !b.helperUsable() {
		return
	}
	ck := "n|" + key
	if _, ok := b.lookup(ck); ok {
		return
	}
	user := "MUSIC STYLE: " + caption + "\nLYRICS:\n" + lyricExcerpt(lyrics, 14)
	b.fillAsync(ck, func(ctx context.Context) (string, error) {
		return b.ollama.ChatWith(ctx, titleSongSystem, user, ChatOpts{
			Format: titleSchema, KeepAliveSeconds: scribeKeepAlive,
		})
	})
}

// lyricExcerpt returns up to n sung lines (tags skipped) for a prompt.
func lyricExcerpt(lyrics string, n int) string {
	var out []string
	for _, line := range strings.Split(lyrics, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "[") {
			continue
		}
		out = append(out, line)
		if len(out) >= n {
			break
		}
	}
	return strings.Join(out, "\n")
}

// TitleForKey returns the song-keyed name when ready (see
// TitleSongAsync); ok is false otherwise.
func (b *Builder) TitleForKey(key string) (title, subtitle string, ok bool) {
	return b.TitleFor(key)
}

// TitleFor returns the helper's short name for a prompt when it is
// ready and valid; ok is false otherwise (callers keep the fallback).
func (b *Builder) TitleFor(prompt string) (title, subtitle string, ok bool) {
	raw, ok := b.lookup("n|" + prompt)
	if !ok {
		return "", "", false
	}
	var v struct {
		Title    string `json:"title"`
		Subtitle string `json:"subtitle"`
	}
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return "", "", false
	}
	title = sanitizeLine(v.Title, 48)
	subtitle = sanitizeLine(v.Subtitle, 64)
	if title == "" {
		return "", "", false
	}
	return title, subtitle, true
}
