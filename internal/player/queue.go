package player

import (
	"context"
	"strings"

	"iar/internal/engine"
	"iar/internal/library"
	"iar/internal/prompting"
	"iar/internal/session"
)

// QueueTrack describes one track a remote client may prefetch.
type QueueTrack struct {
	// ID resolves the track's audio via TrackData.
	ID string
	// Prompt describes the track.
	Prompt string
	// Title and Subtitle are the short display names.
	Title    string
	Subtitle string
	// Seconds is the track's play time.
	Seconds float64
	// Lyrics is what the track sings, empty for instrumentals. A client
	// playing its own downloaded copy has no other way to show the
	// words in step with what it is hearing.
	Lyrics string
	// Kind is "queue" for freshly generated upcoming tracks and
	// "library" for same-vibe banked filler.
	Kind string
}

// libFillerPrefix marks track ids that resolve to the on-disk library.
const libFillerPrefix = "lib:"

// bufTrackPrefix marks queue-listing ids that live in the phased disk
// buffer rather than the in-memory queue.
const bufTrackPrefix = "buf:"

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
		out = append(out, QueueTrack{
			ID: t.ID, Prompt: t.Prompt, Title: t.Title, Subtitle: t.Subtitle,
			Seconds: t.Duration().Seconds(), Kind: "queue", Lyrics: trackLyrics(t),
		})
	}
	buffered := o.Buffer != nil && o.cfg.Buffer.Phased
	o.mu.Unlock()
	if buffered {
		// Phased mode: the deep queue lives on disk. Remote listeners
		// prefetch these exactly like the in-memory queue; the feeder
		// consumes them in the same order.
		for _, e := range o.Buffer.List(epoch) {
			// The name was written with the song's words and stored
			// beside its audio; only a song the engine worded itself
			// needs the deterministic stand-in.
			title, subtitle := e.Title, e.Subtitle
			if title == "" {
				title, subtitle = prompting.TrackTitle(e.Prompt)
			}
			lyr := e.Lyrics
			if lyr == engine.InstrumentalLyrics {
				lyr = ""
			}
			id := e.ID
			if id == "" {
				// Rendered before songs carried their own id.
				id = bufTrackPrefix + e.Base
			}
			out = append(out, QueueTrack{
				ID: id, Prompt: e.Prompt, Title: title, Subtitle: subtitle,
				Seconds: e.Seconds, Kind: "queue", Lyrics: lyr,
			})
		}
	}
	o.mu.Lock()
	// Mid-switchover the queue is empty by design, so filler would be
	// the whole listing - and a remote client that starts playing it is
	// switching into audio older than what it is already playing, quite
	// possibly in the language just switched off. Offer nothing until
	// the first track of the new context exists, the same rule the
	// local mixer follows.
	switching := o.steerPending
	langs := o.enabledLanguageNamesLocked()
	seeded := o.seededLib
	o.mu.Unlock()
	if switching {
		return epoch, out
	}
	listed := make(map[string]bool, len(out))
	for _, row := range out {
		listed[row.ID] = true
	}
	for i, e := range o.Library.Entries(key) {
		if i >= maxLibraryFiller {
			break
		}
		if seeded[e.ID] {
			continue // already queued above as an instant start
		}
		if langs != nil && !langs[e.Language] {
			// A configured list means the listener said which languages
			// they want. A banked track from before languages were
			// recorded has an empty one, which is unknown rather than
			// acceptable.
			continue
		}
		prompt := e.Prompt
		if prompt == "" {
			prompt = "banked track"
		}
		title, subtitle := e.Title, e.Subtitle
		if title == "" {
			title, subtitle = prompting.TrackTitle(prompt)
		}
		id := libFillerPrefix + key + "/" + e.ID
		if e.TrackID != "" {
			// A banked song is still the song it was, so it goes out
			// under its own id. Every song is banked the moment it is
			// fed, and filler is taken newest first - so under a
			// library id of its own, the filler was always the last few
			// songs the radio had fed, and a phone that had them
			// already took each one again. One in the play queue right
			// now is already listed above.
			if listed[e.TrackID] {
				continue
			}
			id = e.TrackID
		}
		out = append(out, QueueTrack{
			ID: id, Prompt: prompt, Title: title, Subtitle: subtitle,
			Seconds: e.Seconds, Kind: "library", Lyrics: e.Lyrics,
		})
	}
	return epoch, out
}

// enabledLanguageNamesLocked is the set of language names a vocal
// session currently sings in, or nil when the listener has not narrowed
// it down and anything banked is fair game. Callers hold o.mu.
func (o *Orchestrator) enabledLanguageNamesLocked() map[string]bool {
	if !o.sess.Vocal || len(o.sess.SungLanguages) == 0 {
		// No list means the engine chooses, so anything banked goes.
		return nil
	}
	on := map[string]bool{}
	for _, name := range o.sess.SungLanguages {
		on[name] = true
	}
	return on
}

// TrackData resolves a track id to its audio, wherever the song is in
// its life: waiting on disk, in the play queue, playing or just played,
// or banked in the library after the radio has moved past it. The track
// comes back labelled with the id it was asked for. ok is false when the
// id is unknown or its audio has been evicted.
func (o *Orchestrator) TrackData(id string) (*engine.Track, bool) {
	if id == "" {
		return nil, false
	}
	ctx := o.runCtx
	if ctx == nil {
		ctx = context.Background()
	}
	if rest, isLib := strings.CutPrefix(id, libFillerPrefix); isLib {
		key, fileID, found := strings.Cut(rest, "/")
		if !found {
			return nil, false
		}
		t, ok := o.Library.Load(ctx, key, fileID)
		if !ok {
			return nil, false
		}
		if t.Title == "" {
			t.Title, t.Subtitle = prompting.TrackTitle(t.Prompt)
		}
		t.ID = id
		return t, true
	}
	// The live copy first: a song keeps its id from disk into the play
	// queue, and feeding it deletes it from disk.
	if t, ok := o.liveTrack(id); ok {
		return t, true
	}
	if base, ok := o.bufferedBase(id); ok {
		o.mu.Lock()
		epoch := o.epoch
		o.mu.Unlock()
		if t, ok := o.Buffer.Peek(ctx, epoch, base); ok {
			// Only a nameless song gets a stand-in: the sidecar's own
			// title is the answer, whether it was written with the words
			// or typed by a listener.
			if t.Title == "" {
				o.fillTitle(t)
			}
			t.ID = id
			return t, true
		}
		// Fed while we were looking: the file is gone because the song
		// just moved into the play queue.
		if t, ok := o.liveTrack(id); ok {
			return t, true
		}
	}
	return o.bankedTrack(ctx, id)
}

// liveTrack finds a song in memory: queued, playing, just played, or
// the loop fallback. Tracks are handed out as shallow copies - the
// caller reads Title and Subtitle without the lock, often during a
// seconds-long encode, while a listener may rename the live track.
// Samples are shared and immutable.
func (o *Orchestrator) liveTrack(id string) (*engine.Track, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, t := range o.queue {
		if t.ID == id {
			cp := *t
			return &cp, true
		}
	}
	for _, t := range []*engine.Track{o.curTrack, o.prevTrack, o.lastGood} {
		if t != nil && t.ID == id {
			cp := *t
			return &cp, true
		}
	}
	return nil, false
}

// bufferedBase names the disk file a song is waiting in. A song rendered
// before songs carried their own id is listed under its file name, so
// that one is read straight off the id; anything else is looked up.
func (o *Orchestrator) bufferedBase(id string) (string, bool) {
	if o.Buffer == nil {
		return "", false
	}
	base, isBuf := strings.CutPrefix(id, bufTrackPrefix)
	o.mu.Lock()
	epoch := o.epoch
	o.mu.Unlock()
	for _, e := range o.Buffer.List(epoch) {
		// A file name is only the name of the song with no id of its
		// own. File names are reused - the sequence starts over once a
		// buffer has emptied - so one that now holds a song with an id
		// is a different song from the one a phone may still hold under
		// the file name, and handing that one back would save or rename
		// the wrong music.
		if isBuf && e.Base == base && e.ID == "" {
			return base, true
		}
		if !isBuf && e.ID == id {
			return e.Base, true
		}
	}
	return "", false
}

// bankedTrack finds a song the radio has already played past. Every
// song is banked in the library as it is fed, so its audio outlives its
// place in the stream by as much as the library holds - which is what a
// phone playing its own copy long after the speakers needs, the moment
// its listener presses save. The remembered location is the quick way;
// reading the library's sidecars is the one that survives a restart.
func (o *Orchestrator) bankedTrack(ctx context.Context, id string) (*engine.Track, bool) {
	o.mu.Lock()
	ref, known := o.bankRefs[id]
	o.mu.Unlock()
	var (
		t  *engine.Track
		ok bool
	)
	if known {
		t, ok = o.Library.Load(ctx, ref.key, ref.id)
	}
	if !ok {
		t, ok = o.Library.Find(ctx, id)
	}
	if !ok {
		return nil, false
	}
	if t.Title == "" {
		t.Title, t.Subtitle = prompting.TrackTitle(t.Prompt)
	}
	t.ID = id
	return t, true
}
