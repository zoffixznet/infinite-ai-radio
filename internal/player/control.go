package player

import (
	"errors"
	"fmt"
	"time"

	"iar/internal/audio"
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
	o.mu.Unlock()
	o.saveSession()
	o.kickGen()
	response := ack.Text
	if musicMode {
		response += o.steerContextNote(text)
		if ack.ContextChanged {
			response += o.switchEstimateNote()
		}
	}
	o.log.Info("steering accepted", "event", "steering", "input", text, "ack", response)
	return response
}

// switchEstimateNote says when a context change will be audible.
func (o *Orchestrator) switchEstimateNote() string {
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
	return fmt.Sprintf("; switching to the new sound in about %s", est.Round(5*time.Second))
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

// Skip jumps to the next source at the following mix iteration. The
// acknowledgment is honest about what will actually play when the queue
// is empty.
func (o *Orchestrator) Skip() string {
	o.mu.Lock()
	o.switchReq = true
	noise := o.sess.Mode == session.ModeNoise
	queued := len(o.queue)
	looping := o.lastGood != nil
	o.mu.Unlock()
	o.log.Info("skip requested", "event", "skip", "queued", queued)
	switch {
	case noise:
		return "noise mode: nothing to skip"
	case queued > 0:
		return "skipping to the next track"
	case looping:
		return "skipping - next track still generating, looping the last one meanwhile"
	default:
		return "skipping - next track still generating"
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

// Status returns a snapshot for status displays.
func (o *Orchestrator) Status() Status {
	o.mu.Lock()
	defer o.mu.Unlock()
	st := Status{
		BufferTarget: o.cfg.BufferTracks,
		FailStreak:   o.failStreak,
		LastFailure:  o.lastFailure,
		Queued:       len(o.queue),
		Generating:   o.genBusy,
		Session:      o.sess.Name,
		SessionDesc:  o.sess.Describe(),
		Volume:       int(o.volume.Load()),
		Paused:       o.paused,
		Underruns:    o.ring.Underruns(),
		GenCount:     o.genCount,
		LastGenTime:  o.lastGen,
		Exporting:    o.exporting,
	}
	if o.eng != nil {
		st.EngineName = o.eng.Name()
		st.EngineReady = o.eng.Ready()
		st.EngineStarting = !st.EngineReady
		if t, ok := o.eng.(interface{ Tail() []string }); ok {
			st.EngineTail = t.Tail()
		}
	}
	if o.cur != nil {
		st.Source = o.cur.label()
		if ts, ok := o.cur.(*trackSource); ok {
			st.Elapsed = time.Duration(float64(ts.elapsedFrames()) / audio.SampleRate * float64(time.Second))
			st.Duration = ts.track.Duration()
		}
	}
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
		title = summarize(o.curTrack)
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
