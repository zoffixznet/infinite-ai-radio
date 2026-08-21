package player

import (
	"fmt"
	"time"

	"bgm/internal/audio"
	"bgm/internal/prompting"
	"bgm/internal/session"
)

// Steer applies one free-text steering input. The interpretation is
// acknowledged in the returned string, the session is persisted, queued
// tracks from the old context are dropped, and the next generation uses the
// new context.
func (o *Orchestrator) Steer(text string) string {
	o.mu.Lock()
	ack := prompting.Steer(o.sess, text)
	if ack.ContextChanged {
		o.epoch++
		dropped := len(o.queue)
		o.queue = nil
		if dropped > 0 {
			o.log.Info("steering dropped queued tracks", "event", "queue_dropped", "count", dropped)
		}
		if o.sess.Mode == session.ModeNoise {
			o.switchReq = true
		}
	}
	o.mu.Unlock()
	o.saveSession()
	o.kickGen()
	o.log.Info("steering accepted", "event", "steering", "input", text, "ack", ack.Text)
	return ack.Text
}

// Clear wipes the accumulated steering context.
func (o *Orchestrator) Clear() string {
	o.mu.Lock()
	o.sess.Clear()
	o.epoch++
	o.queue = nil
	o.mu.Unlock()
	o.saveSession()
	o.kickGen()
	o.log.Info("steering cleared", "event", "steering_cleared")
	return "steering context cleared; back to the session's base sound"
}

// Skip jumps to the next source at the following mix iteration.
func (o *Orchestrator) Skip() string {
	o.mu.Lock()
	o.switchReq = true
	o.mu.Unlock()
	o.log.Info("skip requested", "event", "skip")
	return "skipping to the next track"
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

// NameSession renames the current session so it can be resumed later.
func (o *Orchestrator) NameSession(name string) string {
	o.mu.Lock()
	sess := o.sess
	o.mu.Unlock()
	if err := o.store.Rename(sess, name); err != nil {
		return "could not save session: " + err.Error()
	}
	o.log.Info("session named", "event", "session_named", "name", sess.Name)
	return "session saved as " + sess.Name + " (resume with: bgm --session " + sess.Name + ")"
}

// LoadPreset switches the stream to a built-in preset, seeding a fresh
// session from it.
func (o *Orchestrator) LoadPreset(name string) string {
	p, err := session.LookupPreset(name)
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
	o.mu.Unlock()
	o.saveSession()
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
	o.mu.Unlock()
	o.kickGen()
	o.log.Info("session loaded", "event", "session_loaded", "session", s.Name)
	return "session " + s.Name + " loaded: " + s.Describe()
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

// Status returns a snapshot for status displays.
func (o *Orchestrator) Status() Status {
	o.mu.Lock()
	defer o.mu.Unlock()
	st := Status{
		Queued:      len(o.queue),
		Generating:  o.genBusy,
		Session:     o.sess.Name,
		SessionDesc: o.sess.Describe(),
		Volume:      int(o.volume.Load()),
		Paused:      o.paused,
		Underruns:   o.ring.Underruns(),
		GenCount:    o.genCount,
		LastGenTime: o.lastGen,
		Exporting:   o.exporting,
	}
	if o.eng != nil {
		st.EngineName = o.eng.Name()
		st.EngineReady = o.eng.Ready()
		st.EngineStarting = !st.EngineReady
	}
	if o.cur != nil {
		st.Source = o.cur.label()
		if ts, ok := o.cur.(*trackSource); ok {
			st.Elapsed = time.Duration(float64(ts.elapsedFrames()) / audio.SampleRate * float64(time.Second))
			st.Duration = ts.track.Duration()
		}
	}
	st.State = o.stateLocked()
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
		if _, isNoise := o.cur.(*noiseSource); isNoise {
			if o.eng == nil || !o.eng.Ready() {
				return "waiting for engine (noise bed)"
			}
			return "noise bed (first track generating)"
		}
		return "playing"
	}
}
