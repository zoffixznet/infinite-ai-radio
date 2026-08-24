package player

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"iar/internal/engine"
	"iar/internal/export"
	"iar/internal/prompting"
	"iar/internal/snippets"
)

// SaveSnippet captures the currently playing generated track (or the one
// before it, when which is "prev") as a high-quality MP3 in the snippets
// folder, under the directory for tag (empty means untagged). The track's
// PCM is already in memory, so saving is instant for playback: the encode
// runs in the background and completion is reported through Events.
func (o *Orchestrator) SaveSnippet(which, tag string) string {
	o.mu.Lock()
	var track *engine.Track
	switch which {
	case "prev", "previous", "last":
		track = o.prevTrack
	default:
		track = o.curTrack
	}
	busy := o.saving
	if track != nil && !busy {
		o.saving = true
	}
	o.mu.Unlock()

	if track == nil {
		if which == "prev" {
			return "no previous track to save yet"
		}
		return "nothing to save yet: no generated track is playing"
	}
	if busy {
		return "a snippet is already being saved; try again in a moment"
	}

	slug := snippets.Slug(tag)
	path := snippets.Path(o.SnippetsDir, tag, track.Prompt, time.Now())
	// User-facing acknowledgments show only the tag directory and file
	// name; the absolute path stays in the log.
	shown := filepath.Join(slug, filepath.Base(path))
	prompt := track.Prompt
	ctx := o.runCtx
	if ctx == nil {
		ctx = context.Background()
	}
	go func() {
		defer func() {
			o.mu.Lock()
			o.saving = false
			o.mu.Unlock()
		}()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			o.log.Error("snippet save failed", "event", "snippet_failed", "error", err.Error())
			o.emit("saving the track failed: " + err.Error())
			return
		}
		err := export.EncodeMP3(ctx, track.Samples, path, export.MP3Options{
			Quality: o.cfg.MP3Quality,
			Title:   prompt,
			Artist:  "Infinite AI Radio",
			Album:   slug,
			Comment: snippetComment(track),
		})
		if err != nil {
			o.log.Error("snippet save failed", "event", "snippet_failed", "error", err.Error())
			o.emit("saving the track failed: " + err.Error())
			return
		}
		o.log.Info("snippet saved", "event", "snippet_saved", "path", path, "prompt", prompt, "tag", slug)
		o.emit("track saved: " + shown)
	}()
	return "saving this track to " + shown
}

// snippetComment carries the lyrics (when real) into the file's tags.
func snippetComment(t *engine.Track) string {
	if t.Lyrics == "" || t.Lyrics == engine.InstrumentalLyrics {
		return "instrumental"
	}
	if len(t.Lyrics) > 500 {
		return t.Lyrics[:500]
	}
	return t.Lyrics
}

// NewSession clears everything and starts a fresh auto-persisted session
// seeded from a free-text prompt (the in-app `new` command).
func (o *Orchestrator) NewSession(prompt string) string {
	fresh := prompting.SessionFromPrompt(prompt)
	o.saveSession()
	o.mu.Lock()
	o.sess = fresh
	o.epoch++
	o.queue = nil
	o.lastGood = nil
	o.switchReq = true
	o.steerPending = false
	o.mu.Unlock()
	o.saveSession()
	o.recordCurrent()
	o.seedFromLibrary()
	o.kickGen()
	o.expandSeedAsync(fresh)
	o.log.Info("new session from prompt", "event", "session_new_prompt", "prompt", prompt, "vocal", fresh.Vocal)
	ack := "new session: " + fresh.Describe()
	if fresh.Vocal {
		ack += " (with vocals)"
	}
	return ack + o.steerContextNote("")
}
