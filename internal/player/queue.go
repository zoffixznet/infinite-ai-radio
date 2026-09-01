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
			title, subtitle := prompting.TrackTitle(e.Prompt)
			if t2, s2, ok := o.builder.TitleFor(specPromptForLog(e.Spec)); ok {
				title = t2
				if s2 != "" {
					subtitle = s2
				}
			}
			lyr := e.Lyrics
			if lyr == engine.InstrumentalLyrics {
				lyr = ""
			}
			out = append(out, QueueTrack{
				ID: bufTrackPrefix + e.Base, Prompt: e.Prompt, Title: title, Subtitle: subtitle,
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
		out = append(out, QueueTrack{
			ID: libFillerPrefix + key + "/" + e.ID, Prompt: prompt, Title: title, Subtitle: subtitle,
			Seconds: e.Seconds, Kind: "library", Lyrics: e.Lyrics,
		})
	}
	return epoch, out
}

// enabledLanguageNamesLocked is the set of language names a vocal
// session currently sings in, or nil when the listener has not narrowed
// it down and anything banked is fair game. Callers hold o.mu.
func (o *Orchestrator) enabledLanguageNamesLocked() map[string]bool {
	if !o.sess.Vocal {
		return nil
	}
	states := o.languageStatesLocked()
	if len(states) == 0 {
		return nil
	}
	on := map[string]bool{}
	for _, l := range states {
		if l.On {
			on[l.Name] = true
		}
	}
	if len(on) == 0 {
		return nil // every language off: the engine chooses, so anything goes
	}
	return on
}

// TrackData resolves a track id to its audio: queued tracks, the
// playing and previous tracks, the loop fallback, and library filler.
// ok is false when the id is unknown or already evicted.
func (o *Orchestrator) TrackData(id string) (*engine.Track, bool) {
	if base, isBuf := strings.CutPrefix(id, bufTrackPrefix); isBuf {
		if o.Buffer == nil {
			return nil, false
		}
		o.mu.Lock()
		epoch := o.epoch
		o.mu.Unlock()
		ctx := o.runCtx
		if ctx == nil {
			ctx = context.Background()
		}
		t, ok := o.Buffer.Peek(ctx, epoch, base)
		if ok {
			o.fillTitle(t, specPromptForLog(t.Spec))
		}
		return t, ok
	}
	if rest, isLib := strings.CutPrefix(id, libFillerPrefix); isLib {
		key, fileID, found := strings.Cut(rest, "/")
		if !found {
			return nil, false
		}
		t, ok := o.Library.Load(key, fileID)
		if ok && t.Title == "" {
			t.Title, t.Subtitle = prompting.TrackTitle(t.Prompt)
		}
		return t, ok
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
