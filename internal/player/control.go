package player

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"iar/internal/audio"
	"iar/internal/engine"
	"iar/internal/prompting"
	"iar/internal/session"
	"iar/internal/state"
)

// Steer applies one free-text steering input. The interpretation is
// acknowledged in the returned string, the session is persisted, queued
// tracks from the old context are dropped, and the next generation uses the
// new context.
func (o *Orchestrator) Steer(text string) string {
	o.mu.Lock()
	ack := prompting.Steer(o.sess, text)
	musicMode := o.sess.Mode == session.ModeMusic
	if ack.ContextChanged {
		o.epoch++
		dropped := len(o.queue)
		o.queue = nil
		if dropped > 0 {
			o.log.Info("steering dropped queued tracks", "event", "queue_dropped", "count", dropped)
		}
		switch o.sess.Mode {
		case session.ModeNoise:
			o.switchReq = true
		default:
			// Interrupt the current track: the mixer switches to the
			// first post-steer track as soon as one is queued instead
			// of letting the pre-steer track play to its end.
			o.steerPending = true
		}
	}
	epochAt := o.epoch
	sessPtr := o.sess
	snap := o.sess.Snapshot()
	o.mu.Unlock()
	o.saveSession()
	o.kickGen()
	if ack.ContextChanged && musicMode {
		// The helper model may refine the deterministic interpretation
		// in the background; the result lands only if no further
		// steering happened meanwhile and affects later tracks.
		o.builder.RefineAsync(snap, text, func(u prompting.SpecUpdate) {
			o.mu.Lock()
			changed := false
			if o.sess == sessPtr && o.epoch == epochAt {
				changed = prompting.MergeUpdate(o.sess, u)
			}
			o.mu.Unlock()
			if changed {
				o.saveSession()
				o.log.Info("steering refined by the helper model", "event", "steering_refined", "input", text)
			}
		})
	}
	response := ack.Text
	if musicMode {
		response += o.steerContextNote(text)
		if ack.ContextChanged {
			response += o.switchEstimateNote("sound")
		}
	}
	o.log.Info("steering accepted", "event", "steering", "input", text, "ack", response)
	return response
}

// switchEstimateNote says when a context change will be audible. what
// names what is changing ("sound", "language").
func (o *Orchestrator) switchEstimateNote(what string) string {
	if o.eng == nil || !o.eng.Ready() {
		return ""
	}
	o.mu.Lock()
	est := o.lastGen
	o.mu.Unlock()
	if est <= 0 {
		est = o.Timings.Expected(state.PhaseFirstTrack)
	}
	if est <= 0 {
		return "; switching as soon as the new track is generated"
	}
	return fmt.Sprintf("; switching to the new %s in about %s", what, est.Round(5*time.Second))
}

// steerContextNote appends honesty about when a steering input can be
// heard, plus a hint when the input names a preset.
func (o *Orchestrator) steerContextNote(text string) string {
	note := ""
	if p, err := o.store.LookupPreset(text); err == nil {
		note += " (tip: type 'preset " + p.Name + "' to switch to that preset)"
	}
	switch {
	case o.eng == nil:
		note += "; note: the music engine is unavailable (run 'iar setup')"
	case !o.eng.Ready():
		phase, elapsed, expected, _ := o.PhaseInfo()
		remaining := expected - elapsed
		if remaining < 5*time.Second {
			remaining = 5 * time.Second
		}
		note += fmt.Sprintf("; queued - engine %s, about %s left", phase, remaining.Round(time.Second))
	}
	return note
}

// Clear wipes the accumulated steering context.
func (o *Orchestrator) Clear() string {
	o.mu.Lock()
	o.sess.Clear()
	o.epoch++
	o.queue = nil
	if o.sess.Mode == session.ModeMusic {
		o.steerPending = true
	}
	o.mu.Unlock()
	o.saveSession()
	o.kickGen()
	o.log.Info("steering cleared", "event", "steering_cleared")
	return "steering context cleared; back to the session's base sound"
}

// LyricsGen shows or switches the lyric writer for vocal tracks. An
// empty name reports the current one and the options.
func (o *Orchestrator) LyricsGen(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		o.mu.Lock()
		current := o.builder.GeneratorName(o.sess)
		o.mu.Unlock()
		var b strings.Builder
		fmt.Fprintf(&b, "lyric writer: %s\navailable:\n", current)
		for _, g := range prompting.Generators() {
			marker := " "
			if g.Name() == current {
				marker = "*"
			}
			fmt.Fprintf(&b, " %s %-12s %s\n", marker, g.Name(), g.Blurb())
		}
		b.WriteString("use: lyrics <name>")
		return b.String()
	}
	gen, ok := prompting.GeneratorByName(name)
	if !ok {
		names := make([]string, 0, len(prompting.Generators()))
		for _, g := range prompting.Generators() {
			names = append(names, g.Name())
		}
		return "unknown lyric writer " + name + " (available: " + strings.Join(names, ", ") + ")"
	}
	o.mu.Lock()
	if o.builder.GeneratorName(o.sess) == gen.Name() {
		o.mu.Unlock()
		return gen.Name() + " is already writing the lyrics"
	}
	o.sess.LyricsGenerator = gen.Name()
	ack := "lyric writer: " + gen.Name()
	o.sess.RecordOnly("lyrics "+gen.Name(), ack)
	// Only vocal music tracks sound different under a new writer;
	// drop the queue then so the change is heard soon.
	refresh := o.sess.Vocal && o.sess.Mode == session.ModeMusic
	if refresh {
		o.epoch++
		o.queue = nil
		o.steerPending = true
	}
	o.mu.Unlock()
	o.saveSession()
	o.kickGen()
	if refresh {
		ack += " (takes effect on the next track)"
	} else {
		ack += " (applies when vocals are on)"
	}
	o.log.Info("lyric writer switched", "event", "lyrics_generator", "name", gen.Name())
	return ack
}

// Skip jumps to the next source at the following mix iteration. The
// acknowledgment is honest about what will actually play when the queue
// is empty.
func (o *Orchestrator) Skip() string {
	o.mu.Lock()
	noise := o.sess.Mode == session.ModeNoise
	queued := len(o.queue)
	stale := o.lastGoodStaleLocked()
	looping := o.lastGood != nil && !stale
	// A switch is only armed when there is somewhere to go. With an
	// empty queue the mixer's next stop is the last good track, so
	// skipping would restart the recording already playing and crossfade
	// it against itself - which is what "skipping" looked like from the
	// outside.
	if noise || queued > 0 {
		o.switchReq = true
	}
	o.mu.Unlock()
	o.log.Info("skip requested", "event", "skip", "queued", queued)
	switch {
	case noise:
		return "noise mode: nothing to skip"
	case queued > 0:
		return "skipping to the next track"
	case stale:
		return "nothing new to skip to yet - still on the previous sound while the first track in the new setting generates" + o.switchEstimateNote("sound")
	case looping:
		return "nothing new to skip to yet - the next track is still generating, so this one keeps looping" + o.switchEstimateNote("sound")
	default:
		return "nothing new to skip to yet - the next track is still generating" + o.switchEstimateNote("sound")
	}
}

// Pause silences output without stopping generation.
func (o *Orchestrator) Pause() string {
	o.mu.Lock()
	o.paused = true
	o.mu.Unlock()
	o.log.Info("paused", "event", "paused")
	return "paused (type 'pause' again or 'resume' to continue)"
}

// Resume continues after Pause.
func (o *Orchestrator) Resume() string {
	o.mu.Lock()
	o.paused = false
	o.mu.Unlock()
	o.log.Info("resumed", "event", "resumed")
	return "resumed"
}

// TogglePause flips the pause state and returns an acknowledgment.
func (o *Orchestrator) TogglePause() string {
	o.mu.Lock()
	paused := !o.paused
	o.paused = paused
	o.mu.Unlock()
	if paused {
		o.log.Info("paused", "event", "paused")
		return "paused (type 'pause' again or 'resume' to continue)"
	}
	o.log.Info("resumed", "event", "resumed")
	return "resumed"
}

// SetVolume sets output volume in percent.
func (o *Orchestrator) SetVolume(v int) string {
	if v < 0 {
		v = 0
	}
	if v > 100 {
		v = 100
	}
	o.volume.Store(int32(v))
	o.log.Info("volume changed", "event", "volume", "volume", v)
	return fmt.Sprintf("volume %d%%", v)
}

// NameSession renames the current session so it can be resumed later
// and marks it user-named, which exempts it from the automatic sweep.
func (o *Orchestrator) NameSession(name string) string {
	newName := session.SanitizeName(name)
	if _, err := session.LookupPreset(newName); err == nil {
		return "'" + newName + "' is a built-in preset; pick another name"
	}
	o.mu.Lock()
	oldName := o.sess.Name
	o.sess.Name = newName
	o.sess.Named = true
	o.mu.Unlock()
	o.saveSession()
	if oldName != newName {
		o.store.Delete(oldName)
		if o.StateDir != nil {
			o.StateDir.ForgetSession(oldName)
		}
	}
	o.recordCurrent()
	o.log.Info("session named", "event", "session_named", "name", newName)
	return "session saved as " + newName + " (resume with: iar --session " + newName + ")"
}

// LoadPreset switches the stream to a built-in preset, seeding a fresh
// session from it.
func (o *Orchestrator) LoadPreset(name string) string {
	p, err := o.store.LookupPreset(name)
	if err != nil {
		return err.Error()
	}
	o.saveSession()
	fresh := session.FromPreset(p)
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
	o.kickGen()
	o.log.Info("preset loaded", "event", "preset_loaded", "preset", p.Name)
	return "preset " + p.Name + ": " + p.Description
}

// LoadSession resumes a saved session by name.
func (o *Orchestrator) LoadSession(name string) string {
	s, err := o.store.Load(name)
	if err != nil {
		return err.Error()
	}
	o.saveSession()
	o.mu.Lock()
	o.sess = s
	o.epoch++
	o.queue = nil
	o.lastGood = nil
	o.switchReq = true
	o.steerPending = false
	o.mu.Unlock()
	o.saveSession()
	o.recordCurrent()
	o.kickGen()
	o.log.Info("session loaded", "event", "session_loaded", "session", s.Name)
	return "session " + s.Name + " loaded: " + s.Describe()
}

// LoadByName switches to a preset or a saved session, whichever the
// name denotes (presets win).
func (o *Orchestrator) LoadByName(name string) string {
	if _, err := o.store.LookupPreset(name); err == nil {
		return o.LoadPreset(name)
	}
	return o.LoadSession(name)
}

// SessionNames lists saved sessions, newest first.
func (o *Orchestrator) SessionNames() []string {
	sessions, err := o.store.List()
	if err != nil {
		o.log.Error("session list failed", "event", "session_list_failed", "error", err.Error())
		return nil
	}
	names := make([]string, 0, len(sessions))
	for _, s := range sessions {
		names = append(names, s.Name)
	}
	return names
}

// Listing returns every session and preset grouped for display.
func (o *Orchestrator) Listing() session.Listing {
	l, err := o.store.Listing()
	if err != nil {
		o.log.Error("session list failed", "event", "session_list_failed", "error", err.Error())
	}
	return l
}

// Presets returns the built-in presets that have not been deleted.
func (o *Orchestrator) Presets() []*session.Preset { return o.store.Presets() }

// DeleteSession removes a saved session, or tombstones a preset so it
// disappears until restored. The playing session is never deleted.
func (o *Orchestrator) DeleteSession(name string) string {
	name = session.SanitizeName(name)
	if name == o.CurrentName() {
		return "cannot delete " + name + ": it is playing right now (switch to something else first)"
	}
	if p, err := o.store.LookupPreset(name); err == nil {
		if err := o.store.HidePreset(p.Name); err != nil {
			return "could not delete preset " + p.Name + ": " + err.Error()
		}
		o.log.Info("preset deleted", "event", "preset_deleted", "preset", p.Name)
		return "preset " + p.Name + " deleted (bring presets back with: iar sessions restore-presets)"
	}
	if err := o.store.Delete(name); err != nil {
		if errors.Is(err, session.ErrNotFound) {
			return "no session or preset named " + name
		}
		return "could not delete " + name + ": " + err.Error()
	}
	if o.StateDir != nil {
		o.StateDir.ForgetSession(name)
	}
	o.log.Info("session deleted", "event", "session_deleted", "session", name)
	return "session " + name + " deleted"
}

// EngineTail returns recent engine output lines, newest last, for the
// diagnostic command that asks for them. It is deliberately not part of
// Status: reading it costs a pass over the engine daemon's log, which
// grows to hundreds of megabytes over a run, and Status is called on
// every repaint.
func (o *Orchestrator) EngineTail() []string {
	if o.eng == nil {
		return nil
	}
	t, ok := o.eng.(interface{ Tail() []string })
	if !ok {
		return nil
	}
	return t.Tail()
}

// Status returns a snapshot for status displays.
func (o *Orchestrator) Status() Status {
	o.mu.Lock()
	defer o.mu.Unlock()
	st := Status{
		BufferTarget:    o.cfg.BufferTracks,
		FailStreak:      o.failStreak,
		LastFailure:     o.lastFailure,
		Queued:          len(o.queue),
		Generating:      o.genBusy,
		Session:         o.sess.Name,
		SessionDesc:     o.sess.Describe(),
		Epoch:           o.epoch,
		BasePrompt:      o.sess.BasePrompt,
		Tweaks:          append([]session.Entry(nil), o.sess.Tweaks...),
		Vocal:           o.sess.Vocal,
		LyricsGenerator: o.builder.GeneratorName(o.sess),
		Volume:          int(o.volume.Load()),
		Paused:          o.paused,
		Underruns:       o.ring.Underruns(),
		GenCount:        o.genCount,
		LastGenTime:     o.lastGen,
		Exporting:       o.exporting,
	}
	if o.eng != nil {
		st.EngineName = o.eng.Name()
		st.EngineReady = o.eng.Ready()
		st.EngineStarting = !st.EngineReady
	}
	if o.phasedEnabled() {
		st.Phased = true
		st.BufferedTracks = o.bufTracks
		st.WordsmithWant = o.wordsmithWantNow
		st.WordsmithWrote = o.wordsmithWroteNow
		// Mirror cycleTargets' ramp tiers (inlined: it takes o.mu).
		switch {
		case o.playedInEpoch == 0:
			st.RampBatch = 1
		case o.properPlayedInEpoch < rampStableTracks:
			st.RampBatch = rampSmallBatch
		}
		st.BufferedSeconds = o.bufSeconds
		st.PlannedTracks = o.bufPlans
		st.PlannedSeconds = o.bufPlanSeconds
		st.BufferTargetSeconds = float64(o.cfg.Buffer.RenderAheadMinutes) * 60
		st.BufferLowSeconds = float64(o.cfg.Buffer.RenderLowMinutes) * 60
	}
	if o.Telemetry != nil {
		// The sampler measures on its own timer; this is a copy of the
		// last snapshot, cheap enough for every repaint.
		s := o.Telemetry.Sample()
		st.Telemetry = &s
	}
	if o.cur != nil {
		st.Source = o.cur.label()
		if ts, ok := o.cur.(*trackSource); ok {
			st.Elapsed = time.Duration(float64(ts.elapsedFrames()) / audio.SampleRate * float64(time.Second))
			st.Duration = ts.track.Duration()
			st.TrackID = ts.track.ID
			st.TrackPrompt = ts.track.Prompt
			st.TrackTitle = ts.track.Title
			st.TrackSubtitle = ts.track.Subtitle
			st.TrackNum = o.curTrackNum
			st.TrackSaved = o.saved[ts.track.ID]
			st.Looping = ts.loop
			st.TrackLanguage = trackLanguage(ts.track)
			st.TrackLyrics = trackLyrics(ts.track)
		}
	}
	if o.prevTrack != nil {
		st.PrevTrackID = o.prevTrack.ID
		st.PrevTrackPrompt = o.prevTrack.Prompt
		st.PrevTrackTitle = o.prevTrack.Title
		st.PrevTrackSaved = o.saved[o.prevTrack.ID]
	}
	st.SavedTrackIDs = append([]string(nil), o.savedOrder...)
	st.Languages = o.languageStatesLocked()
	st.Switching = o.steerPending
	st.State = o.stateLocked()
	o.mu.Unlock()
	st.Phase, st.PhaseElapsed, st.PhaseExpected, st.PhaseSlow = o.PhaseInfo()
	o.mu.Lock()
	return st
}

// stateLocked derives the display state; callers hold o.mu.
func (o *Orchestrator) stateLocked() string {
	switch {
	case o.paused:
		return "paused"
	case o.cur == nil:
		return "starting"
	case o.sess.Mode == session.ModeNoise:
		return "noise"
	default:
		switch o.cur.(type) {
		case silenceSource:
			return "preparing"
		case *noiseSource:
			if o.eng == nil || !o.eng.Ready() {
				return "waiting for engine (noise bed)"
			}
			return "noise bed (first track generating)"
		}
		return "playing"
	}
}

// Snapshot returns paused state, volume percent and a display title for
// desktop integration surfaces.
func (o *Orchestrator) Snapshot() (paused bool, volume int, title string) {
	o.mu.Lock()
	paused = o.paused
	title = o.sess.Describe()
	if o.curTrack != nil {
		if o.curTrack.Title != "" {
			title = o.curTrack.Title
			if o.curTrack.Subtitle != "" {
				title += " - " + o.curTrack.Subtitle
			}
		} else {
			title = summarize(o.curTrack)
		}
	}
	o.mu.Unlock()
	return paused, int(o.volume.Load()), title
}

// Announce surfaces a message from an external control surface (the phone
// remote) in the interactive UI and the log.
func (o *Orchestrator) Announce(text string) {
	o.log.Info("remote action", "event", "remote_action", "text", text)
	o.emit(text)
}

// SetLanguageStore installs the sink that persists the vocal-language
// catalogue and which of its languages are switched off. Without one
// both still work, they just do not survive a restart.
func (o *Orchestrator) SetLanguageStore(save func(names, off []string) error) {
	o.mu.Lock()
	o.saveLanguages = save
	o.mu.Unlock()
}

// languageStatesLocked pairs the configured catalogue with the
// session's choices; callers hold o.mu.
func (o *Orchestrator) languageStatesLocked() []LanguageState {
	cat := o.builder.Languages()
	out := make([]LanguageState, 0, len(cat))
	for _, l := range cat {
		on, ok := o.sess.Languages[l.Name]
		out = append(out, LanguageState{Name: l.Name, Engine: l.Engine(), On: !ok || on})
	}
	return out
}

// Languages lists the configured vocal languages and which of them the
// session sings in.
func (o *Orchestrator) Languages() []LanguageState {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.languageStatesLocked()
}

// SetLanguage switches one configured language on or off for the
// playing session. Turning them all off hands the choice back to the
// music engine.
func (o *Orchestrator) SetLanguage(name string, on bool) string {
	name = strings.Join(strings.Fields(name), " ")
	if name == "" {
		return "name a language to switch"
	}
	o.mu.Lock()
	var match string
	for _, l := range o.builder.Languages() {
		if strings.EqualFold(l.Name, name) {
			match = l.Name
			break
		}
	}
	if match == "" {
		o.mu.Unlock()
		return name + " is not one of the configured languages"
	}
	if o.sess.Languages == nil {
		o.sess.Languages = map[string]bool{}
	}
	o.sess.Languages[match] = on
	states := o.languageStatesLocked()
	vocal := o.sess.Vocal
	pinned := o.releasePinLocked()
	refresh := vocal && o.sess.Mode == session.ModeMusic
	if refresh {
		o.epoch++
		o.queue = nil
		o.steerPending = true
	}
	save := o.saveLanguages
	o.mu.Unlock()
	o.saveSession()
	o.kickGen()
	ack := describeLanguages(states) + o.languageAckNote(vocal, pinned, refresh)
	if save != nil {
		if err := save(languageNames(states), switchedOff(states)); err != nil {
			o.log.Error("saving the vocal languages failed", "event", "vocal_languages_failed", "error", err.Error())
			ack += " (this run only: " + err.Error() + ")"
		}
	}
	o.log.Info("vocal language switched", "event", "vocal_language", "name", match, "on", on)
	return ack
}

// languageNames lists the configured languages in order.
func languageNames(states []LanguageState) []string {
	out := make([]string, 0, len(states))
	for _, l := range states {
		out = append(out, l.Name)
	}
	return out
}

// switchedOff lists the configured languages not currently being sung.
func switchedOff(states []LanguageState) []string {
	var out []string
	for _, l := range states {
		if !l.On {
			out = append(out, l.Name)
		}
	}
	return out
}

// releasePinLocked drops a language pinned earlier by hand ("sing in
// french") and reports whether there was one. Touching the language
// controls is the newest and most explicit instruction about language a
// listener can give, and on the phone it is the only one they can give,
// so it wins over the older steer. Callers hold o.mu.
func (o *Orchestrator) releasePinLocked() bool {
	if o.sess.Spec == nil || !o.sess.Spec.LanguagePinned {
		return false
	}
	o.sess.Spec.LanguagePinned = false
	o.sess.Spec.VocalLanguage = ""
	return true
}

// languageAckNote says what a language edit will actually do, so a
// listener who changed the list is not left guessing whether it landed.
func (o *Orchestrator) languageAckNote(vocal, pinned, refresh bool) string {
	note := ""
	if pinned {
		note = " (this replaces the language you steered in earlier)"
	}
	switch {
	case !vocal:
		return note + " (applies when vocals are on - this session is instrumental)"
	case refresh:
		return note + " (the tracks queued ahead were dropped)" + o.switchEstimateNote("language")
	default:
		return note + " (takes effect on the next track)"
	}
}

// SetLanguages replaces the configured catalogue and persists it. An
// empty list hands every track's language back to the music engine.
func (o *Orchestrator) SetLanguages(names []string) string {
	langs := prompting.ParseLanguages(names)
	clean := make([]string, 0, len(langs))
	for _, l := range langs {
		clean = append(clean, l.Name)
	}
	// The swap and the epoch bump happen together: a generation that
	// starts between them is built from the new catalogue and then
	// thrown away by the epoch guard, which is a wasted minute of the
	// graphics card exactly when the listener is waiting.
	o.mu.Lock()
	before := enabledSignature(o.languageStatesLocked())
	o.builder.SetLanguages(clean)
	// Choices about languages that are gone would silently reappear if
	// the same name were configured again later.
	for name := range o.sess.Languages {
		keep := false
		for _, l := range langs {
			if l.Name == name {
				keep = true
				break
			}
		}
		if !keep {
			delete(o.sess.Languages, name)
		}
	}
	states := o.languageStatesLocked()
	save := o.saveLanguages
	vocal := o.sess.Vocal
	pinned := false
	// Saving the same list again must not cost the listener the audio
	// already generated ahead; only a real change to what is sung is
	// worth dropping the queue for.
	changed := enabledSignature(states) != before
	if changed {
		pinned = o.releasePinLocked()
	}
	refresh := vocal && changed && o.sess.Mode == session.ModeMusic
	if refresh {
		o.epoch++
		o.queue = nil
		o.steerPending = true
	}
	o.mu.Unlock()
	o.saveSession()
	o.kickGen()
	ack := describeLanguages(states)
	if changed {
		ack += o.languageAckNote(vocal, pinned, refresh)
	} else {
		ack += " (unchanged)"
	}
	if save != nil {
		if err := save(clean, switchedOff(states)); err != nil {
			o.log.Error("saving the vocal languages failed", "event", "vocal_languages_failed", "error", err.Error())
			return ack + " (this run only: " + err.Error() + ")"
		}
	}
	o.log.Info("vocal languages configured", "event", "vocal_languages", "languages", strings.Join(clean, ", "))
	return ack
}

// enabledSignature identifies the set of languages actually sung, so a
// language edit that changes nothing audible can be left alone.
func enabledSignature(states []LanguageState) string {
	var sb strings.Builder
	for _, l := range states {
		if l.On {
			sb.WriteString(l.Name)
			sb.WriteByte('|')
		}
	}
	return sb.String()
}

// describeLanguages says what the songs will be sung in.
func describeLanguages(states []LanguageState) string {
	var on []string
	for _, l := range states {
		if l.On {
			on = append(on, l.Name)
		}
	}
	switch len(on) {
	case 0:
		return "singing in whatever language the music engine picks"
	case 1:
		return "singing in " + on[0]
	default:
		return "each song picks one of " + strings.Join(on[:len(on)-1], ", ") + " or " + on[len(on)-1] + " at random"
	}
}

// trackLyrics returns the words a track is singing, or "" when there
// are none to show. The engine echoes back what it actually used, which
// for an engine-planned track is the only record of the words.
func trackLyrics(t *engine.Track) string {
	l := strings.TrimSpace(t.Lyrics)
	if l == engine.InstrumentalLyrics {
		return ""
	}
	return l
}

// trackLanguage names the language a generated track was sung in, in
// the listener's own wording. Tracks generated before the name was
// carried, and hand-pinned ones, still resolve through the engine tag.
func trackLanguage(t *engine.Track) string {
	if t.Spec.VocalLanguageName != "" {
		return t.Spec.VocalLanguageName
	}
	if !t.Spec.Vocal() {
		// The spec carries a language even for an instrumental, where
		// nothing is sung in it.
		return ""
	}
	return prompting.LanguageName(t.Spec.VocalLanguage)
}
