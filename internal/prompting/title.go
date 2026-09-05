package prompting

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"iar/internal/engine"
)

// This file names tracks for display on lock screens and car displays:
// a deterministic name derived from the track's own description, and
// the helper model's better one, asked for where the song's words are
// written and waited for there. Whichever a song gets, it gets once and
// keeps. The deterministic one must look finished on its own, because
// the helper is optional and can fail.

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

// titleSongSystem names one specific song from its own words, which is
// what keeps two songs from the same station from sharing a name.
const titleSongSystem = `You name songs. Given a song's music style and
its lyrics, reply with JSON {"title", "subtitle"}. title: a distinctive
2-4 word name drawn from this song's own words and imagery - its hook
line or sharpest image, in the language the lyrics are written in;
never a genre label, never generic. subtitle: at most 6 words naming
the genre and mood, in English. Never mention prompts or AI.`

// titleInstrumentalSystem names a piece with no words, from the
// description written for it - the only thing that tells it apart from
// every other piece in the same style.
const titleInstrumentalSystem = `You name instrumental pieces. Given a
DESCRIPTION of one piece, reply with JSON {"title", "subtitle"}. title:
a distinctive 2-4 word name drawn from that description's own imagery -
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

// titleSong names one song from its own words, blocking until the
// helper answers. It runs where the lyrics are written - on the free
// graphics card, moments after the words exist - because a name that
// arrives later is a name that has to be applied to a song someone is
// already listening to. An empty title means the helper had nothing;
// callers fall back to the deterministic name and keep it.
func (b *Builder) titleSong(ctx context.Context, caption, lyrics string) (title, subtitle string) {
	if lyrics == "" || lyrics == engine.InstrumentalLyrics || b.ollama == nil {
		return "", ""
	}
	return b.nameFrom(ctx, titleSongSystem,
		"MUSIC STYLE: "+caption+"\nLYRICS:\n"+lyricExcerpt(lyrics, 14))
}

// titleInstrumental names a piece that has no words, from the
// description written for it in the same round.
func (b *Builder) titleInstrumental(ctx context.Context, caption string) (title, subtitle string) {
	if caption == "" || b.ollama == nil {
		return "", ""
	}
	return b.nameFrom(ctx, titleInstrumentalSystem, "DESCRIPTION: "+caption)
}

// nameFrom asks for one name and waits for it. There is exactly one
// retry, immediately, while the model is still loaded: this is the only
// window in which the name can be asked for at all, and a name that
// fails here is a song that plays under a generic one for the rest of
// its life. A rest earned by unrelated failures is deliberately not
// consulted - the lyrics for this very song just came back, so the
// helper is answering.
func (b *Builder) nameFrom(ctx context.Context, system, user string) (title, subtitle string) {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		raw, err := b.ollama.ChatWith(ctx, system, user, ChatOpts{
			Format: titleSchema, KeepAliveSeconds: scribeKeepAlive,
		})
		if err != nil {
			lastErr = err
			if ctx.Err() != nil {
				break
			}
			continue
		}
		var v struct {
			Title    string `json:"title"`
			Subtitle string `json:"subtitle"`
		}
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			lastErr = fmt.Errorf("unreadable answer %q: %w", firstChars(raw, 60), err)
			continue
		}
		title = sanitizeLine(v.Title, 48)
		subtitle = sanitizeLine(v.Subtitle, 64)
		if title != "" {
			return title, subtitle
		}
		lastErr = errors.New("the helper answered with no name")
	}
	// Said out loud: a whole batch coming out under prompt-derived
	// names is otherwise a mystery with nothing in the log.
	reason := "unknown"
	if lastErr != nil {
		reason = lastErr.Error()
	}
	b.log.Warn("song not named; it keeps the name from its description",
		"event", "song_name_failed", "error", reason)
	return "", ""
}

// firstChars trims a helper reply for a log line.
func firstChars(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
