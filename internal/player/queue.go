package player

import (
	"context"
	"strings"

	"iar/internal/engine"
	"iar/internal/prompting"
	"iar/internal/trackbuffer"
)

// QueueTrack describes one song in the store, as a client sees it.
type QueueTrack struct {
	// ID resolves the song's audio via TrackData and TrackFile.
	ID string
	// Prompt describes the song.
	Prompt string
	// Title and Subtitle are the short display names.
	Title    string
	Subtitle string
	// Seconds is the song's play time.
	Seconds float64
	// Taken reports that some player has taken the song already: it is
	// kept for the players that have not caught up.
	Taken bool
	// Hash is the SHA-256 of the song's MP3 as served, when the radio
	// recorded one at render; a client sends it back to save the song
	// from its own copy.
	Hash string
}

// bufTrackPrefix marks queue-listing ids that live in the phased disk
// buffer rather than the in-memory queue.
const bufTrackPrefix = "buf:"

// storeRow is the listing row for one song in the store.
func storeRow(e trackbuffer.Entry) QueueTrack {
	// The name was written with the song's words and stored beside
	// its audio; only a song the engine worded itself needs the
	// deterministic stand-in.
	title, subtitle := e.Title, e.Subtitle
	if title == "" {
		title, subtitle = prompting.TrackTitle(e.Prompt)
	}
	id := e.ID
	if id == "" {
		// Rendered before songs carried their own id.
		id = bufTrackPrefix + e.Base
	}
	return QueueTrack{
		ID: id, Prompt: e.Prompt, Title: title, Subtitle: subtitle,
		Seconds: e.Seconds, Taken: e.Taken, Hash: e.Hash,
	}
}

// QueueTracks returns the steering epoch and every song in the store,
// in the order they were made: the taken ones first, since they are
// older, then the ones nobody has taken. A client takes what it lacks,
// and a client filling up meets the taken songs first. Reading the
// listing never touches the audio path.
func (o *Orchestrator) QueueTracks() (int, []QueueTrack) {
	o.mu.Lock()
	epoch := o.epoch
	o.mu.Unlock()
	if o.Buffer == nil {
		return epoch, nil
	}
	entries := o.Buffer.List(epoch)
	out := make([]QueueTrack, 0, len(entries))
	for _, e := range entries {
		out = append(out, storeRow(e))
	}
	return epoch, out
}

// Song describes one song a client holds or is offered, with its
// lyrics: from the store while the song is there, from the songbook
// once it has gone. ok is false for a song the radio never made.
func (o *Orchestrator) Song(id string) (row QueueTrack, lyrics string, ok bool) {
	if base, found := o.bufferedBase(id); found {
		o.mu.Lock()
		epoch := o.epoch
		o.mu.Unlock()
		for _, e := range o.Buffer.List(epoch) {
			if e.Base == base {
				lyr := e.Lyrics
				if lyr == engine.InstrumentalLyrics {
					lyr = ""
				}
				return storeRow(e), lyr, true
			}
		}
	}
	rec, known := o.Songbook.ByID(id)
	if !known || rec.Hash == "" {
		return QueueTrack{}, "", false
	}
	title, subtitle := rec.Title, rec.Subtitle
	if title == "" {
		title, subtitle = prompting.TrackTitle(rec.Prompt)
	}
	lyr := rec.Lyrics
	if lyr == engine.InstrumentalLyrics {
		lyr = ""
	}
	return QueueTrack{
		ID: rec.ID, Prompt: rec.Prompt, Title: title, Subtitle: subtitle,
		Seconds: rec.Seconds, Taken: true, Hash: rec.Hash,
	}, lyr, true
}

// Take marks a song as taken by a client that has downloaded it: the
// level drops, and the generator may have a rung due. Taking a song
// nobody holds any more is nothing.
func (o *Orchestrator) Take(id string) {
	if o.Buffer == nil {
		return
	}
	base, ok := o.bufferedBase(id)
	if !ok {
		return
	}
	o.mu.Lock()
	epoch := o.epoch
	o.mu.Unlock()
	if o.Buffer.Take(epoch, base) {
		if dropped := o.Buffer.Trim(epoch, o.cfg.Buffer.Songs); dropped > 0 {
			o.log.Info("oldest taken songs trimmed from the store", "event", "buffer_trimmed", "songs", dropped)
		}
		o.kickGen()
	}
}

// TrackFile returns the MP3 of a song in the store, to be served
// exactly as written. ok is false for a song that is only in memory,
// or unknown.
func (o *Orchestrator) TrackFile(id string) (string, bool) {
	if o.Buffer == nil {
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
