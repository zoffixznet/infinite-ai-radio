package player

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"bgm/internal/engine"
	"bgm/internal/export"
	"bgm/internal/prompting"
	"bgm/internal/session"
)

// SaveSnippet captures the currently playing generated track (or the one
// before it, when which is "prev") as a high-quality MP3 in the snippets
// folder. The track's PCM is already in memory, so saving is instant for
// playback: the encode runs in the background and completion is reported
// through Events.
func (o *Orchestrator) SaveSnippet(which string) string {
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

	path := filepath.Join(o.SnippetsDir, snippetName(track))
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
		if err := os.MkdirAll(o.SnippetsDir, 0o755); err != nil {
			o.log.Error("snippet save failed", "event", "snippet_failed", "error", err.Error())
			o.emit("saving the track failed: " + err.Error())
			return
		}
		err := export.EncodeMP3(ctx, track.Samples, path, export.MP3Options{
			Quality: o.cfg.MP3Quality,
			Title:   prompt,
			Artist:  "bgm",
			Comment: snippetComment(track),
		})
		if err != nil {
			o.log.Error("snippet save failed", "event", "snippet_failed", "error", err.Error())
			o.emit("saving the track failed: " + err.Error())
			return
		}
		o.log.Info("snippet saved", "event", "snippet_saved", "path", path, "prompt", prompt)
		o.emit("track saved: " + path)
	}()
	return "saving this track to " + path
}

// snippetName builds the snippet file name: timestamp plus prompt slug.
func snippetName(t *engine.Track) string {
	slug := session.SanitizeName(t.Prompt)
	if len(slug) > 60 {
		slug = slug[:60]
	}
	if slug == "" || slug == "unnamed" {
		slug = "track"
	}
	return fmt.Sprintf("%s-%s.mp3", time.Now().Format("20060102-150405"), slug)
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
	o.mu.Unlock()
	o.saveSession()
	o.seedFromLibrary()
	o.kickGen()
	o.log.Info("new session from prompt", "event", "session_new_prompt", "prompt", prompt, "vocal", fresh.Vocal)
	ack := "new session: " + fresh.Describe()
	if fresh.Vocal {
		ack += " (with vocals)"
	}
	return ack + o.steerContextNote("")
}
