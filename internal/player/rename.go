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
// the song is remembered: the playing copy, the rendered song still on
// disk, the banked copy kept for instant starts, and - if this song was
// already saved - the file and its ID3 tag in the snippets folder.

// bankRef locates a track's banked library copy so a rename can be
// written into its sidecar as well.
type bankRef struct {
	key string
	id  string
}

// maxRetired bounds how many finished songs stay renameable. A phone
// plays its own copies at its own pace and can be a long way behind the
// speakers, so "rename what I am hearing" often names a song this
// machine finished with; its audio is gone, but the copies that outlive
// it - the banked one, a saved file - are not.
const maxRetired = 64

// maxBankRefs bounds the remembered banked-copy locations; the oldest
// are pruned when the map outgrows it and their tracks are gone.
const maxBankRefs = 96

// maxBufFed bounds how many fed-from-disk songs are remembered. A
// listener's own copy of a song outlives the disk original by however
// far ahead they are buffered; a couple of hours' worth is plenty.
const maxBufFed = 128

// rememberFed records that a disk-buffer file became this in-memory
// track, so a rename aimed at the file finds the song.
func (o *Orchestrator) rememberFed(base, id string) {
	if base == "" || id == "" {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.bufFed == nil {
		o.bufFed = map[string]string{}
	}
	if _, seen := o.bufFed[base]; !seen {
		o.bufFedOrder = append(o.bufFedOrder, base)
	}
	o.bufFed[base] = id
	for len(o.bufFedOrder) > maxBufFed {
		delete(o.bufFed, o.bufFedOrder[0])
		o.bufFedOrder = o.bufFedOrder[1:]
	}
}

// retireTrack remembers a song that has finished playing, by name only:
// enough to rename it afterwards, nothing that pins its audio.
func (o *Orchestrator) retireTrack(t *engine.Track) {
	if t == nil || t.ID == "" {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.retired == nil {
		o.retired = map[string]string{}
	}
	if _, seen := o.retired[t.ID]; !seen {
		o.retiredOrder = append(o.retiredOrder, t.ID)
	}
	o.retired[t.ID] = t.Subtitle
	for len(o.retiredOrder) > maxRetired {
		delete(o.retired, o.retiredOrder[0])
		o.retiredOrder = o.retiredOrder[1:]
	}
}

// rememberBank records where a track's banked library copy lives.
func (o *Orchestrator) rememberBank(id string, ref bankRef) {
	if id == "" || ref.id == "" {
		return
	}
	o.mu.Lock()
	o.bankRefs[id] = ref
	over := len(o.bankRefs) > maxBankRefs
	o.mu.Unlock()
	if over {
		o.pruneBankRefs()
	}
}

// pruneBankRefs drops banked-copy locations whose tracks are no longer
// anywhere a rename could still find them.
func (o *Orchestrator) pruneBankRefs() {
	o.mu.Lock()
	defer o.mu.Unlock()
	live := map[string]bool{}
	for _, t := range o.queue {
		live[t.ID] = true
	}
	if ts, ok := o.cur.(*trackSource); ok {
		live[ts.track.ID] = true
	}
	for _, t := range []*engine.Track{o.prevTrack, o.lastGood, o.curTrack} {
		if t != nil {
			live[t.ID] = true
		}
	}
	for id := range o.bankRefs {
		// A finished song can still be renamed, so where it was banked
		// is still worth knowing.
		if _, retired := o.retired[id]; !live[id] && !retired {
			delete(o.bankRefs, id)
		}
	}
}

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
	if base, isBuf := strings.CutPrefix(which, bufTrackPrefix); isBuf {
		// The machine may have fed this song since the listener's
		// device took its copy, which deletes it from disk. Follow it
		// into memory before giving up on it.
		o.mu.Lock()
		fed := o.bufFed[base]
		o.mu.Unlock()
		if fed == "" {
			return o.retitleOnDisk(base, title)
		}
		which = fed
	}
	if rest, isLib := strings.CutPrefix(which, libFillerPrefix); isLib {
		key, fileID, found := strings.Cut(rest, "/")
		if !found {
			return "that song cannot be renamed"
		}
		return o.retitleBanked(key, fileID, title)
	}
	return o.retitleLive(which, title)
}

// retitleLive renames a song that is in memory - playing, played, or
// waiting in the queue - and every copy of it that outlives playback.
func (o *Orchestrator) retitleLive(which, title string) string {
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
		// Only what is actually coming out of the speakers: with the
		// noise bed playing there is no song on screen to rename.
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
		take(o.curTrack)
		take(o.prevTrack)
		take(o.lastGood)
	}
	if len(hits) == 0 {
		// Not here any more, but a listener whose device trails the
		// speakers is still hearing it. The audio is gone; the copies
		// that outlive it are not.
		subtitle, retired := o.retired[which]
		ref, banked := o.bankRefs[which]
		o.mu.Unlock()
		if !retired {
			if which == "" {
				return "nothing is playing to rename yet"
			}
			o.log.Info("rename found no such song", "event", "track_rename_missed", "id", which)
			return "that song is no longer here to rename"
		}
		if banked {
			o.Library.SetTitle(ref.key, ref.id, title, subtitle)
		}
		o.log.Info("song renamed", "event", "track_renamed", "id", which, "title", title, "retired", true)
		return o.renameAck(which, title)
	}
	var (
		id       = hits[0].ID
		subtitle = hits[0].Subtitle
	)
	for _, t := range hits {
		t.Title = title
	}
	ref, banked := o.bankRefs[id]
	o.mu.Unlock()

	if banked {
		o.Library.SetTitle(ref.key, ref.id, title, subtitle)
	}
	o.log.Info("song renamed", "event", "track_renamed", "id", id, "title", title)
	return o.renameAck(id, title)
}

// retitleOnDisk renames a rendered song still waiting in the disk
// buffer: nothing is playing it yet, so the sidecar is the whole job.
func (o *Orchestrator) retitleOnDisk(base, title string) string {
	if o.Buffer == nil {
		return "that song is no longer here to rename"
	}
	o.mu.Lock()
	epoch := o.epoch
	o.mu.Unlock()
	var subtitle string
	var found bool
	for _, e := range o.Buffer.List(epoch) {
		if e.Base != base {
			continue
		}
		subtitle, found = e.Subtitle, true
		break
	}
	if !found || !o.Buffer.SetTitle(epoch, base, title, subtitle) {
		return "that song is no longer here to rename"
	}
	o.log.Info("song renamed", "event", "track_renamed", "id", bufTrackPrefix+base, "title", title)
	return o.renameAck(bufTrackPrefix+base, title)
}

// retitleBanked renames a library track a listener is playing as
// filler; the banked file is the only copy there is.
func (o *Orchestrator) retitleBanked(key, fileID, title string) string {
	var subtitle string
	for _, e := range o.Library.Entries(key) {
		if e.ID == fileID {
			subtitle = e.Subtitle
			break
		}
	}
	if !o.Library.SetTitle(key, fileID, title, subtitle) {
		return "that song is no longer here to rename"
	}
	id := libFillerPrefix + key + "/" + fileID
	o.log.Info("song renamed", "event", "track_renamed", "id", id, "title", title)
	return o.renameAck(id, title)
}

// renameAck finishes a rename: a song already saved to disk is renamed
// there too, because a listener who renames what they are hearing means
// the copy they kept as well.
func (o *Orchestrator) renameAck(id, title string) string {
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
	o.mu.Lock()
	path := o.saved[id]
	o.mu.Unlock()
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
	o.mu.Lock()
	if o.saved[id] == path {
		o.saved[id] = newPath
	}
	o.mu.Unlock()
	o.log.Info("saved copy renamed", "event", "snippet_renamed", "path", newPath)
	return filepath.Join(tag, newFile), true
}
