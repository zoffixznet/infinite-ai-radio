package player

import (
	"strings"

	"iar/internal/engine"
	"iar/internal/library"
	"iar/internal/session"
)

// QueueTrack describes one track a remote client may prefetch.
type QueueTrack struct {
	// ID resolves the track's audio via TrackData.
	ID string
	// Prompt describes the track.
	Prompt string
	// Seconds is the track's play time.
	Seconds float64
	// Kind is "queue" for freshly generated upcoming tracks and
	// "library" for same-vibe banked filler.
	Kind string
}

// libFillerPrefix marks track ids that resolve to the on-disk library.
const libFillerPrefix = "lib:"

// maxLibraryFiller bounds how many banked tracks pad the queue listing.
const maxLibraryFiller = 6

// QueueTracks returns the steering epoch and the tracks a remote client
// may prefetch: the in-memory queue first, then same-vibe library
// tracks as lower-priority filler. Reading the queue never touches the
// audio path.
func (o *Orchestrator) QueueTracks() (int, []QueueTrack) {
	o.mu.Lock()
	epoch := o.epoch
	if o.sess.Mode != session.ModeMusic {
		o.mu.Unlock()
		return epoch, nil
	}
	key := library.Key(o.sess)
	out := make([]QueueTrack, 0, len(o.queue)+maxLibraryFiller)
	for _, t := range o.queue {
		out = append(out, QueueTrack{ID: t.ID, Prompt: t.Prompt, Seconds: t.Duration().Seconds(), Kind: "queue"})
	}
	o.mu.Unlock()
	for i, e := range o.Library.Entries(key) {
		if i >= maxLibraryFiller {
			break
		}
		prompt := e.Prompt
		if prompt == "" {
			prompt = "banked track"
		}
		out = append(out, QueueTrack{
			ID: libFillerPrefix + key + "/" + e.ID, Prompt: prompt, Seconds: e.Seconds, Kind: "library",
		})
	}
	return epoch, out
}

// TrackData resolves a track id to its audio: queued tracks, the
// playing and previous tracks, the loop fallback, and library filler.
// ok is false when the id is unknown or already evicted.
func (o *Orchestrator) TrackData(id string) (*engine.Track, bool) {
	if rest, isLib := strings.CutPrefix(id, libFillerPrefix); isLib {
		key, fileID, found := strings.Cut(rest, "/")
		if !found {
			return nil, false
		}
		return o.Library.Load(key, fileID)
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, t := range o.queue {
		if t.ID == id {
			return t, true
		}
	}
	for _, t := range []*engine.Track{o.curTrack, o.prevTrack, o.lastGood} {
		if t != nil && t.ID == id {
			return t, true
		}
	}
	return nil, false
}
