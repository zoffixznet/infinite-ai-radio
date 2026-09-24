package player

import (
	"context"
	"strings"

	"iar/internal/engine"
	"iar/internal/prompting"
	"iar/internal/trackbuffer"
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
	// Kind is "queue" for the songs ahead - this machine's prefetch and
	// the songs in the store nobody has taken - and "library" for the
	// taken songs the store keeps as spares.
	Kind string
	// Hash is the SHA-256 of the track's MP3 as served, when the radio
	// recorded one at render; a client sends it back to save the song
	// from its own copy.
	Hash string
}

// bufTrackPrefix marks queue-listing ids that live in the phased disk
// buffer rather than the in-memory queue.
const bufTrackPrefix = "buf:"

// maxSpares bounds how many kept songs pad the queue listing.
const maxSpares = 6

// QueueTracks returns the steering epoch and the tracks a remote client
// may prefetch: this machine's in-memory prefetch first, then the songs
// in the store nobody has taken, then a few of the taken songs the
// store keeps as spares. Reading the queue never touches the audio
// path.
func (o *Orchestrator) QueueTracks() (int, []QueueTrack) {
	o.mu.Lock()
	epoch := o.epoch
	out := make([]QueueTrack, 0, len(o.queue)+maxSpares)
	for _, t := range o.queue {
		out = append(out, QueueTrack{
			ID: t.ID, Prompt: t.Prompt, Title: t.Title, Subtitle: t.Subtitle,
			Seconds: t.Duration().Seconds(), Kind: "queue", Lyrics: trackLyrics(t),
		})
	}
	buffered := o.Buffer != nil && o.cfg.Buffer.Phased
	o.mu.Unlock()
	if buffered {
		// The deep queue lives on disk. Remote listeners prefetch the
		// songs nobody has taken exactly like the in-memory queue; the
		// feeder consumes them in the same order.
		row := func(e trackbuffer.Entry, kind string) QueueTrack {
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
			return QueueTrack{
				ID: id, Prompt: e.Prompt, Title: title, Subtitle: subtitle,
				Seconds: e.Seconds, Kind: kind, Lyrics: lyr,
			}
		}
		var kept []trackbuffer.Entry
		for _, e := range o.Buffer.List(epoch) {
			if e.Taken {
				kept = append(kept, e)
				continue
			}
			out = append(out, row(e, "queue"))
		}
		// Taken songs stay in the store for players that have not
		// caught up. To a phone they are songs the radio has moved past,
		// worth holding as spares. The newest few, the ones in the
		// in-memory queue excepted - those are listed above.
		queued := make(map[string]bool, len(out))
		for _, r := range out {
			queued[r.ID] = true
		}
		spares := 0
		for i := len(kept) - 1; i >= 0 && spares < maxSpares; i-- {
			r := row(kept[i], "library")
			if queued[r.ID] {
				continue
			}
			out = append(out, r)
			spares++
		}
	}
	for i := range out {
		if s, ok := o.Songbook.ByID(out[i].ID); ok {
			out[i].Hash = s.Hash
		}
	}
	return epoch, out
}

// TrackFile returns the MP3 of a song in the store, to be served
// exactly as written. ok is false for a song that is only in memory,
// or unknown.
func (o *Orchestrator) TrackFile(id string) (string, bool) {
	if o.Buffer == nil || !o.cfg.Buffer.Phased {
		return "", false
	}
	base, ok := o.bufferedBase(id)
	if !ok {
		return "", false
	}
	o.mu.Lock()
	epoch := o.epoch
	o.mu.Unlock()
	return o.Buffer.TrackPath(epoch, base)
}

// TrackData resolves a track id to its audio, wherever the song is in
// its life: in the store, in the play queue, playing or just played.
// The track comes back labelled with the id it was asked for. ok is
// false when the id is unknown or its audio is gone.
func (o *Orchestrator) TrackData(id string) (*engine.Track, bool) {
	if id == "" {
		return nil, false
	}
	ctx := o.runCtx
	if ctx == nil {
		ctx = context.Background()
	}
	// The live copy first: a song keeps its id from the store into the
	// play queue.
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
		// Trimmed while we were looking; the play queue may still
		// hold it.
		if t, ok := o.liveTrack(id); ok {
			return t, true
		}
	}
	return nil, false
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
	for _, t := range []*engine.Track{o.incoming, o.curTrack, o.prevTrack, o.lastGood} {
		if t != nil && t.ID == id {
			cp := *t
			return &cp, true
		}
	}
	return nil, false
}

// bufferedBase names the file a song sits in. A song rendered before
// songs carried their own id is listed under its file name, so that
// one is read straight off the id; anything else is looked up.
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
