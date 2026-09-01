package player

import (
	"context"
	"time"

	"iar/internal/engine"
)

// The helper that names songs can run minutes behind the music on a
// busy machine: a song is often planned, rendered, fed and even playing
// before its name exists. A name applied only at feed time therefore
// misses almost always, and the song plays under its prompt-derived
// stand-in ("Nu-metal") forever. This loop closes that gap: it keeps
// re-requesting and re-checking names for every song that still carries
// a provisional one - in the prefetch queue, playing, just played, and
// rendered on disk - and applies answers wherever they land. Every
// interface polls Status, so a name arriving mid-song reaches the
// terminal, the phone and the lock screen on their next tick.

// retitleInterval is how often late names are re-checked. Each check is
// a few map lookups plus, in phased mode, a metadata listing.
const retitleInterval = 20 * time.Second

// retitleRequestsPerPass caps how many NEW naming calls one pass may
// start for songs on disk. A long-running station can hold a hundred
// unnamed songs, and asking for them all at once buries a slow helper
// in concurrent calls that then time out together; a few per pass, in
// play order, names what is about to play first and drains the backlog
// at a rate the helper can actually answer.
const retitleRequestsPerPass = 2

// bankRef locates a track's banked library copy so a late name can be
// written into its sidecar as well.
type bankRef struct {
	key string
	id  string
}

func (o *Orchestrator) retitleLoop(ctx context.Context) {
	t := time.NewTicker(retitleInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			o.retitlePass()
		}
	}
}

// retitlePass runs one round of late-name resolution. Split from the
// loop so tests can drive it without the timer.
func (o *Orchestrator) retitlePass() {
	o.mu.Lock()
	var cands []*engine.Track
	add := func(t *engine.Track) {
		if t != nil && t.TitleProvisional && t.TitleKey != "" {
			cands = append(cands, t)
		}
	}
	for _, t := range o.queue {
		add(t)
	}
	if ts, ok := o.cur.(*trackSource); ok {
		add(ts.track)
	}
	add(o.prevTrack)
	o.mu.Unlock()

	for _, t := range cands {
		// Ask again first: the original request may have been dropped
		// (helper still probing at startup, or resting after failures).
		// The call is cached and a no-op while an answer is pending.
		if t.Lyrics != "" && t.Lyrics != engine.InstrumentalLyrics {
			o.builder.TitleSongAsync(t.TitleKey, specPromptForLog(t.Spec), t.Lyrics)
		}
		title, subtitle, ok := o.builder.TitleForKey(t.TitleKey)
		if !ok {
			continue
		}
		o.mu.Lock()
		t.Title = title
		if subtitle != "" {
			t.Subtitle = subtitle
		}
		t.TitleProvisional = false
		ref, hasBank := o.bankRefs[t.ID]
		delete(o.bankRefs, t.ID)
		o.mu.Unlock()
		if hasBank {
			// The banked library copy was written under the stand-in
			// name; give it the real one so instant starts and filler
			// in later runs do not resurrect "Nu-metal".
			o.Library.SetTitle(ref.key, ref.id, title, subtitle)
		}
		o.log.Info("late title applied", "event", "title_applied",
			"key", t.TitleKey, "title", title)
	}
	o.pruneBankRefs()

	// Rendered songs still on disk get their names persisted, so the
	// queue listing shows them and a restart keeps them. Only the
	// buffer's existence matters here - without phased generation it
	// simply lists nothing.
	if o.Buffer == nil {
		return
	}
	o.mu.Lock()
	epoch := o.epoch
	o.mu.Unlock()
	requested := 0
	for _, e := range o.Buffer.List(epoch) {
		if e.Title != "" || e.TitleKey == "" {
			continue
		}
		// Answers are applied without limit; only NEW requests are
		// rationed.
		if title, subtitle, ok := o.builder.TitleForKey(e.TitleKey); ok {
			if o.Buffer.SetTitle(epoch, e.Base, title, subtitle) {
				o.log.Info("late title persisted", "event", "title_persisted",
					"key", e.TitleKey, "title", title)
			}
			continue
		}
		// Only calls that actually start consume a ration slot, so a
		// key whose cached answer failed validation cannot starve the
		// entries behind it.
		if requested < retitleRequestsPerPass &&
			e.Lyrics != "" && e.Lyrics != engine.InstrumentalLyrics &&
			o.builder.TitleSongAsync(e.TitleKey, e.Prompt, e.Lyrics) {
			requested++
		}
	}
}

// pruneBankRefs drops banked-copy locations whose tracks are no longer
// anywhere a rename could still find them.
func (o *Orchestrator) pruneBankRefs() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.bankRefs) == 0 {
		return
	}
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
		if !live[id] {
			delete(o.bankRefs, id)
		}
	}
}
