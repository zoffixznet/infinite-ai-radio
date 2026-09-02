package prompting

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"unicode"

	"iar/internal/engine"
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
	title = titleCase(nameFromPhrase(segs[0], 32))
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

// A deterministic title has to look finished on its own, and a prompt
// written as prose ("An aggressive and high-energy nu-metal track
// driven by...") otherwise became "An Aggressive And High-Energy": an
// article at the front and whatever word the cut happened to land on at
// the back. Only articles are dropped from the front - anything more
// eats the words that carry the meaning.
var (
	titleLeading  = map[string]bool{"a": true, "an": true, "the": true}
	titleTrailing = map[string]bool{
		"a": true, "an": true, "the": true, "and": true, "or": true,
		"with": true, "of": true, "in": true, "on": true, "for": true,
		"to": true, "by": true, "from": true, "that": true, "which": true,
		"driven": true, "featuring": true, "backed": true, "built": true,
	}
)

// nameFromPhrase shortens a prompt segment into something that reads as
// a name: no leading article, and no conjunction or preposition left
// dangling by the cut.
func nameFromPhrase(seg string, n int) string {
	words := strings.Fields(shorten(seg, n))
	for len(words) > 1 && titleLeading[strings.ToLower(words[0])] {
		words = words[1:]
	}
	for len(words) > 1 && titleTrailing[strings.ToLower(words[len(words)-1])] {
		words = words[:len(words)-1]
	}
	return strings.Join(words, " ")
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
// It reports whether a new helper call actually started, so callers
// that ration requests do not spend a slot on a no-op (cached answer,
// call already in flight, helper unusable).
func (b *Builder) TitleSongAsync(key, caption, lyrics string) bool {
	if key == "" || lyrics == "" || !b.helperUsable() {
		return false
	}
	ck := "n|" + key
	if _, ok := b.lookup(ck); ok {
		return false
	}
	user := "MUSIC STYLE: " + caption + "\nLYRICS:\n" + lyricExcerpt(lyrics, 14)
	return b.fillAsync(ck, func(ctx context.Context) (string, error) {
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

// SongKey names the helper's title slot for one song by its own words:
// the words exist before the song is planned, rendered, or even
// sequenced, so the name can be asked for the moment the lyrics are
// written and found again by anyone who holds the lyrics - the plan,
// the rendered file, the feeder, or a listing, across restarts and
// replans alike. Instrumentals have no words and no key.
func SongKey(lyrics string) string {
	if lyrics == "" || lyrics == engine.InstrumentalLyrics {
		return ""
	}
	sum := sha256.Sum256([]byte(lyrics))
	return "song:" + hex.EncodeToString(sum[:8])
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
