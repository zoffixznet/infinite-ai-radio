package player

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"iar/internal/engine"
	"iar/internal/export"
	"iar/internal/prompting"
	"iar/internal/snippets"
)

// SaveSnippet captures a generated track as a high-quality MP3 in the
// snippets folder, under the directory for tag (empty means untagged).
// which selects the track: empty for the currently playing one, "prev"
// for the one before it, or a track id (a buffered phone saves what its
// driver is hearing, not what the speakers play). The track's PCM is
// already in memory or on disk, so saving is instant for playback: the
// encode runs in the background and completion is reported through
// Events. Saving an already-saved track is a success no-op.
func (o *Orchestrator) SaveSnippet(which, tag string) string {
	var track *engine.Track
	if isTrackID(which) {
		// An already-saved id is a no-op even after its audio has been
		// evicted from memory.
		o.mu.Lock()
		already := o.saved[which] != ""
		o.mu.Unlock()
		if already {
			return "already saved: that track is in your snippets"
		}
		t, ok := o.TrackData(which)
		if !ok {
			return "that track is no longer available to save"
		}
		if t.ID == "" {
			t.ID = which // library tracks load without an id of their own
		}
		track = t
	}
	o.mu.Lock()
	if track == nil {
		switch which {
		case "prev", "previous", "last":
			track = o.prevTrack
		default:
			track = o.curTrack
		}
	}
	already := track != nil && track.ID != "" && o.saved[track.ID] != ""
	busy := o.saving
	if track != nil && !already && !busy {
		o.saving = true
	}
	o.mu.Unlock()

	if track == nil {
		if which == "prev" {
			return "no previous track to save yet"
		}
		return "nothing to save yet: no generated track is playing"
	}
	if already {
		return "already saved: that track is in your snippets"
	}
	if busy {
		return "a snippet is already being saved; try again in a moment"
	}

	slug := snippets.Slug(tag)
	// Copy the display fields under the lock: the retitle loop may
	// still be replacing a provisional name on this very track. The
	// save then uses one consistent name throughout, whichever side of
	// the rename it caught.
	o.mu.Lock()
	title, subtitle := track.Title, track.Subtitle
	o.mu.Unlock()
	if title == "" {
		title, subtitle = prompting.TrackTitle(track.Prompt)
	}
	// The file is named after what the interface shows - the title, in
	// its own language's script - plus the sung language's tag, so a
	// track heard on the remote is findable on disk by the same name.
	path := snippets.Path(o.SnippetsDir, tag, title, track.Spec.VocalLanguage, time.Now())
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
			Quality:  o.cfg.MP3Quality,
			Title:    title,
			Subtitle: subtitle,
			Artist:   "Infinite AI Radio",
			Album:    slug,
			Comment:  snippetComment(track),
		})
		if err != nil {
			o.log.Error("snippet save failed", "event", "snippet_failed", "error", err.Error())
			o.emit("saving the track failed: " + err.Error())
			return
		}
		// The full lyric sheet rides along as a text file with the same
		// base name (the ID3 comment only holds a truncated copy).
		if track.Lyrics != "" && track.Lyrics != engine.InstrumentalLyrics {
			if err := os.WriteFile(snippets.LyricsSidecar(path), []byte(track.Lyrics), 0o644); err != nil {
				o.log.Warn("lyrics sidecar not written", "event", "snippet_lyrics_failed", "error", err.Error())
			}
		}
		o.markSaved(track.ID, path)
		o.log.Info("snippet saved", "event", "snippet_saved", "path", path, "prompt", prompt, "tag", slug)
		o.emit("track saved: " + shown)
		// The encode takes seconds and a rename takes none: a listener
		// who renamed the song while it was being written would
		// otherwise find the old name on disk forever.
		o.mu.Lock()
		latest := track.Title
		o.mu.Unlock()
		if latest != "" && latest != title {
			if moved, ok := o.retitleSaved(track.ID, latest); ok {
				o.emit("saved as: " + moved)
			}
		}
	}()
	return "saving this track to " + shown
}

// isTrackID reports whether a save selector is a track id rather than a
// which keyword.
func isTrackID(which string) bool {
	return strings.HasPrefix(which, "t-") || strings.HasPrefix(which, libFillerPrefix) ||
		strings.HasPrefix(which, bufTrackPrefix)
}

// maxSavedIDs bounds the remembered saved-track set.
const maxSavedIDs = 64

// markSaved records that a track id was saved this run, and where the
// file landed so a later rename can move it.
func (o *Orchestrator) markSaved(id, path string) {
	if id == "" {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.saved == nil {
		o.saved = map[string]string{}
	}
	if o.saved[id] != "" {
		return
	}
	o.saved[id] = path
	o.savedOrder = append(o.savedOrder, id)
	for len(o.savedOrder) > maxSavedIDs {
		delete(o.saved, o.savedOrder[0])
		o.savedOrder = o.savedOrder[1:]
	}
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
