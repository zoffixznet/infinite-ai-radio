package player

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	"iar/internal/engine"
	"iar/internal/export"
	"iar/internal/snippets"
)

// A song's name is the only handle a listener has on it. The helper
// writes most of them, but the one it wrote is sometimes wrong, dull or
// simply not what the song turned out to be - and the song saying so is
// playing right now, which is the worst possible moment to be sent to
// another screen to fix it. Retitle renames whatever is playing from
// wherever the listener is, and then chases that name into every place
// the song is remembered: the playing copy, the song in the store, the
// songbook's record of it, and - if this song was already saved - the
// file and its ID3 tag in the snippets folder.

// Retitle renames a song. which selects it exactly as saving does:
// empty for what the speakers are playing, "prev" for the one before
// it, or a track id - a phone playing its own downloaded copy renames
// what it is hearing, not what the machine is. The ack names the song
// as the listener will now see it.
func (o *Orchestrator) Retitle(which, title string) string {
	title = strings.TrimSpace(title)
	if title == "" {
		return "a new name is needed"
	}
	// A song keeps one id from the render on, so there is nothing to
	// translate here: the song is looked for in memory, then in the
	// store, then in the book, whatever stage it has reached. That
	// includes a song rendered before songs had ids of their own, whose
	// file name is its id for good.
	o.mu.Lock()
	var (
		hits []*engine.Track
		seen = map[*engine.Track]bool{}
	)
	take := func(t *engine.Track) {
		if t == nil || seen[t] {
			return
		}
		// An empty selector means the playing song, so the caller's id
		// matching only applies when they named one.
		if which != "" && which != "prev" && which != "previous" && which != "last" && t.ID != which {
			return
		}
		seen[t] = true
		hits = append(hits, t)
	}
	switch which {
	case "prev", "previous", "last":
		take(o.prevTrack)
	case "":
		// Only what is actually coming out of the speakers: with
		// nothing but silence there is no song on screen to rename.
		if ts, ok := o.cur.(*trackSource); ok {
			take(ts.track)
		}
	default:
		for _, t := range o.queue {
			take(t)
		}
		if ts, ok := o.cur.(*trackSource); ok {
			take(ts.track)
		}
		take(o.incoming)
		take(o.curTrack)
		take(o.prevTrack)
		take(o.lastGood)
	}
	if len(hits) == 0 {
		o.mu.Unlock()
		if which == "" {
			return "nothing is playing to rename yet"
		}
		// Not in memory here. A phone ahead of the speakers, or a long
		// way behind them, is playing a song that is still a file in the
		// store, listed under the song's own id.
		if base, ok := o.bufferedBase(which); ok {
			return o.retitleOnDisk(base, which, title)
		}
		// Gone from the store too, but made here: the book keeps the
		// name, for the copy the phone still holds and for the saved
		// file.
		if _, known := o.Songbook.ByID(which); known {
			o.log.Info("song renamed", "event", "track_renamed", "id", which, "title", title, "gone", true)
			return o.renameAck(which, title)
		}
		o.log.Info("rename found no such song", "event", "track_rename_missed", "id", which)
		return "that song is no longer here to rename"
	}
	id := hits[0].ID
	for _, t := range hits {
		t.Title = title
	}
	o.mu.Unlock()
	// The copy in the store, while the song is still there: a player
	// that has not caught up reads its name from that.
	if base, ok := o.bufferedBase(id); ok {
		o.setStoredTitle(base, title)
	}
	o.log.Info("song renamed", "event", "track_renamed", "id", id, "title", title)
	return o.renameAck(id, title)
}

// retitleOnDisk renames a song that is only in the store: nothing here
// is playing it, so the sidecar is the whole job. id is the one the
// listener named it by, and the one the answer uses.
func (o *Orchestrator) retitleOnDisk(base, id, title string) string {
	if !o.setStoredTitle(base, title) {
		return "that song is no longer here to rename"
	}
	o.log.Info("song renamed", "event", "track_renamed", "id", id, "title", title)
	return o.renameAck(id, title)
}

// setStoredTitle writes a new title into a stored song's sidecar,
// keeping its genre line. Reports whether the song was still there.
func (o *Orchestrator) setStoredTitle(base, title string) bool {
	if o.Buffer == nil {
		return false
	}
	o.mu.Lock()
	epoch := o.epoch
	o.mu.Unlock()
	for _, e := range o.Buffer.List(epoch) {
		if e.Base == base {
			return o.Buffer.SetTitle(epoch, base, title, e.Subtitle)
		}
	}
	return false
}

// renameAck finishes a rename: the book records the name, and a song
// already saved to disk is renamed there too, because a listener who
// renames what they are hearing means the copy they kept as well.
func (o *Orchestrator) renameAck(id, title string) string {
	// The book keeps the name past the audio: a copy that comes back
	// from a phone to be saved is saved under the name it was given.
	o.Songbook.Retitle(id, title)
	if moved, ok := o.retitleSaved(id, title); ok {
		return "renamed to " + title + ", saved copy included: " + moved
	}
	return "renamed to " + title
}

// retitleSaved renames the snippet a track was saved as, rewriting the
// MP3's own title tag and the file names. Reports the tag/file it now
// goes by. A track that was never saved is not an error.
func (o *Orchestrator) retitleSaved(id, title string) (string, bool) {
	if id == "" || o.SnippetsDir == "" {
		return "", false
	}
	path := o.Songbook.SavedPath(id)
	if path == "" {
		return "", false
	}
	tag, file := filepath.Base(filepath.Dir(path)), filepath.Base(path)
	ctx := o.runCtx
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if err := export.RetitleMP3(ctx, path, title); err != nil {
		o.log.Warn("saved copy not renamed", "event", "snippet_retitle_failed",
			"path", path, "error", err.Error())
		return "", false
	}
	newFile, err := snippets.NewCatalog(o.SnippetsDir).Retitle(tag, file, title)
	if err != nil {
		// The tag inside the file is already the new one, which is
		// what a music player shows; only the file name lags.
		o.log.Warn("saved copy kept its file name", "event", "snippet_rename_failed",
			"path", path, "error", err.Error())
		return filepath.Join(tag, file), true
	}
	newPath := filepath.Join(filepath.Dir(path), newFile)
	o.Songbook.MarkSaved(id, newPath)
	o.log.Info("saved copy renamed", "event", "snippet_renamed", "path", newPath)
	return filepath.Join(tag, newFile), true
}
