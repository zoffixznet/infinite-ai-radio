package player

import (
	"context"
	"time"

	"iar/internal/audio"
	"iar/internal/engine"
	"iar/internal/session"
)

// Noise amplitudes: the fallback bed sits lower than deliberate noise mode.
const (
	bedAmp   = 0.12
	noiseAmp = 0.30
)

// mixLoop writes the audible stream into the ring buffer: queued tracks
// joined with equal-power crossfades, degrading to looping the last good
// track or the session's noise bed when generation cannot keep up.
func (o *Orchestrator) mixLoop(ctx context.Context) {
	const chunkFrames = audio.SampleRate / 10 // 100 ms
	fadeFrames := int(o.cfg.CrossfadeSeconds * audio.SampleRate)

	// Instant startup audio: the session's noise bed (or noise mode's
	// deliberate noise).
	o.setCurrent(o.startupSource())

	// Held, the mixer feeds the output silence instead of songs. It
	// deliberately does NOT advance the source: the whole point of the
	// hold is that the buffer is still there, unspent, when the
	// listener comes back.
	hold := make([]byte, audio.FramesToBytes(chunkFrames))
	for ctx.Err() == nil {
		if o.standbyNow() {
			if _, err := o.ring.Write(hold); err != nil {
				return // ring closed: shutting down
			}
			continue
		}
		o.mu.Lock()
		cur := o.cur
		o.mu.Unlock()
		wantSwitch := o.takeSwitch()

		// A finite source about to end always needs a successor.
		if !wantSwitch {
			if rem := cur.remaining(); rem >= 0 && rem <= fadeFrames {
				wantSwitch = true
			}
			// An endless fallback source yields as soon as real
			// content is available.
			if o.fallbackShouldYield(cur) {
				wantSwitch = true
			}
		}

		if wantSwitch {
			next := o.chooseNext(cur)
			if next != nil {
				o.crossfade(ctx, cur, next, fadeFrames, chunkFrames)
				continue
			}
		}

		buf := cur.read(chunkFrames)
		if len(buf) == 0 {
			// Source exhausted with no successor chosen above
			// (e.g. degenerate track shorter than the fade).
			next := o.chooseNext(cur)
			if next == nil {
				next = o.bedSource()
			}
			o.setCurrent(next)
			continue
		}
		if _, err := o.ring.Write(audio.SamplesToBytes(buf)); err != nil {
			return // ring closed: shutting down
		}
	}
}

// takeSwitch consumes pending switch requests, reporting whether the
// mixer should change sources now. A steer interrupts the current track
// as soon as post-steer content exists: steering empties the queue and
// stale-epoch results are discarded, so anything queued while
// steerPending is set is post-steer by construction. Each request is
// consumed exactly once.
func (o *Orchestrator) takeSwitch() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	want := o.switchReq
	o.switchReq = false
	if o.steerPending && len(o.queue) > 0 {
		want = true
		o.steerPending = false
	}
	return want
}

// startupSource picks the very first audio source for the session. The
// default for music sessions is silence with visible progress; the noise
// bed plays only for noise sessions, on explicit opt-in, or when the
// engine is unavailable (with a prominent explanation).
func (o *Orchestrator) startupSource() source {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.sess.Mode == session.ModeNoise {
		color := audio.ParseNoiseColor(o.sess.NoiseColor)
		return newNoiseSource(color, noiseAmp, string(color)+" noise")
	}
	if o.eng == nil {
		o.emit("MUSIC ENGINE UNAVAILABLE: run 'iar setup' to install it (details: 'iar doctor'). Playing the session's noise bed instead.")
		color := audio.ParseNoiseColor(o.sess.NoiseBed)
		return newNoiseSource(color, bedAmp, string(color)+" noise bed")
	}
	if o.cfg.BedWhileWaiting {
		o.emit("noise bed while the first track is prepared (bed_while_waiting is on)")
		color := audio.ParseNoiseColor(o.sess.NoiseBed)
		return newNoiseSource(color, bedAmp, string(color)+" noise bed")
	}
	return silenceSource{}
}

// bedSource returns the session's fallback noise bed.
func (o *Orchestrator) bedSource() source {
	o.mu.Lock()
	color := audio.ParseNoiseColor(o.sess.NoiseBed)
	o.mu.Unlock()
	return newNoiseSource(color, bedAmp, string(color)+" noise bed")
}

// isStopgap reports whether a source is a placeholder (silence or a noise
// bed) rather than real content.
func isStopgap(s source) bool {
	switch s.(type) {
	case silenceSource, *noiseSource:
		return true
	default:
		return false
	}
}

// fallbackShouldYield reports whether cur is a stopgap that should hand
// over to real queued content.
func (o *Orchestrator) fallbackShouldYield(cur source) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.sess.Mode == session.ModeNoise {
		// Deliberate noise never yields automatically.
		if ns, ok := cur.(*noiseSource); ok && ns.gen.Color() == audio.ParseNoiseColor(o.sess.NoiseColor) {
			return false
		}
		return true // wrong source for noise mode; switch
	}
	if isStopgap(cur) {
		return len(o.queue) > 0
	}
	// A track the listener flagged for looping is wanted as-is: it
	// neither yields to the queue nor gets treated as a warm-up.
	if o.loopOn && o.loopEpoch == o.epoch {
		if _, ok := cur.(*trackSource); ok {
			return false
		}
	}
	// A library track is a warm-up: hand over as soon as a freshly
	// generated track is waiting.
	if ts, ok := cur.(*trackSource); ok && ts.track.FromLibrary {
		for _, q := range o.queue {
			if !q.FromLibrary {
				return true
			}
		}
	}
	return false
}

// chooseNext picks the successor source, applying the degradation ladder:
// queued track, then looping the last good track, then the noise bed. It
// returns nil when cur should simply continue (endless source, nothing
// better available).
func (o *Orchestrator) chooseNext(cur source) source {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.sess.Mode == session.ModeNoise {
		color := audio.ParseNoiseColor(o.sess.NoiseColor)
		name := string(color) + " noise"
		if ns, ok := cur.(*noiseSource); ok && ns.gen.Color() == color && ns.name == name {
			return nil
		}
		return newNoiseSource(color, noiseAmp, name)
	}

	// A requested loop replays the track the listener flagged: the
	// same recording comes back (with the usual crossfade) until the
	// loop is toggled off, skipped past, or steering changes the
	// context. The queue is untouched and nothing counts as played.
	if o.loopOn && o.loopEpoch == o.epoch {
		if ts, ok := cur.(*trackSource); ok {
			return newTrackSource(ts.track, summarize(ts.track)+" (looping on request)")
		}
	}

	if len(o.queue) > 0 {
		t := o.queue[0]
		o.queue = o.queue[1:]
		// The ramp's stability count means songs the listener actually
		// started hearing, not songs staged into the prefetch - and
		// the deep batch unlocks on songs with the writer's own words
		// (or instrumentals, which have none to write): the audition
		// must be of the songs the deep batch will actually sound
		// like, not of the quick engine-worded openers.
		o.playedInEpoch++
		if t.Spec.Lyrics != "" {
			o.properPlayedInEpoch++
		}
		defer o.kickGen()
		return newTrackSource(t, summarize(t))
	}

	// Nothing queued. Keep an endless source running rather than
	// restarting it.
	if isStopgap(cur) {
		return nil
	}

	if o.lastGood != nil {
		// After a context change the last good track is the sound the
		// listener has just moved away from: worth saying so, and worth
		// saying only once per change so a burst of skips does not
		// flush every other event out of the channel.
		stale := o.lastGoodStaleLocked()
		label := " (looping)"
		if stale {
			label = " (previous sound)"
		}
		if o.loopNoticeEpoch != o.epoch || !stale {
			if stale {
				o.loopNoticeEpoch = o.epoch
				o.log.Warn("queue empty after a context change, replaying the previous sound", "event", "stale_loop_fallback")
				o.emit("still on the previous sound while the first track in the new setting generates")
			} else {
				o.log.Warn("queue empty, looping last track", "event", "loop_fallback")
				o.emit("generator is behind; looping the last track")
			}
		}
		src := newTrackSource(o.lastGood, summarize(o.lastGood)+label)
		src.loop = true
		return src
	}

	// A finite source ended with nothing to play and no last track. With
	// a working engine this is a brief wait: stay silent with progress.
	// Only an unavailable/failed engine gets the audible noise bed, and
	// it is announced prominently.
	o.mu.Unlock()
	failed := o.engineFailed()
	o.mu.Lock()
	if !failed {
		o.log.Info("queue empty with no last track, waiting in silence", "event", "silence_wait")
		return silenceSource{}
	}
	o.log.Warn("engine unavailable with nothing to play, noise bed fallback", "event", "bed_fallback")
	o.emit("ENGINE UNAVAILABLE: no music can be generated (see 'iar doctor' and the log). Playing the noise bed instead.")
	color := audio.ParseNoiseColor(o.sess.NoiseBed)
	return newNoiseSource(color, bedAmp, string(color)+" noise bed")
}

// crossfade streams an equal-power transition from cur into next, then
// installs next as current. Chunked so shutdown stays responsive.
func (o *Orchestrator) crossfade(ctx context.Context, cur, next source, fadeFrames, chunkFrames int) {
	fade := fadeFrames
	if _, silent := cur.(silenceSource); silent {
		// Fading out of silence needs no long blend; a short fade-in
		// gets music to the ears sooner.
		if max := 3 * audio.SampleRate / 10; fade > max {
			fade = max
		}
	}
	if rem := cur.remaining(); rem >= 0 && rem < fade {
		fade = rem
	}
	if rem := next.remaining(); rem >= 0 && rem/2 < fade {
		fade = rem / 2
	}
	done := 0
	for done < fade && ctx.Err() == nil {
		n := chunkFrames
		if done+n > fade {
			n = fade - done
		}
		a := cur.read(n)
		b := next.read(n)
		mixed := mixSegment(a, b, n, done, fade)
		if _, err := o.ring.Write(audio.SamplesToBytes(mixed)); err != nil {
			return
		}
		done += n
	}
	o.log.Info("crossfade", "event", "crossfade", "from", cur.label(), "to", next.label(), "seconds", float64(fade)/audio.SampleRate)
	o.setCurrent(next)
}

// mixSegment mixes one chunk of a whole fade, with gains derived from the
// chunk's position in the fade.
func mixSegment(tail, head []int16, frames, offset, total int) []int16 {
	out := make([]int16, frames*audio.Channels)
	for f := 0; f < frames; f++ {
		gOut, gIn := audio.EqualPowerGains(float64(offset+f) / float64(total))
		for c := 0; c < audio.Channels; c++ {
			i := f*audio.Channels + c
			var a, b float64
			if i < len(tail) {
				a = float64(tail[i])
			}
			if i < len(head) {
				b = float64(head[i])
			}
			v := a*gOut + b*gIn
			switch {
			case v > 32767:
				v = 32767
			case v < -32768:
				v = -32768
			}
			out[i] = int16(v)
		}
	}
	return out
}

// setCurrent installs a source as the one playing now.
func (o *Orchestrator) setCurrent(s source) {
	o.mu.Lock()
	o.cur = s
	ts, isTrack := s.(*trackSource)
	if isTrack && (o.curTrack == nil || o.curTrack != ts.track) {
		o.prevTrack = o.curTrack
		o.curTrack = ts.track
		o.playCount++
		o.curTrackNum = o.playCount
	}
	firstMusic := isTrack && !o.firstMusic
	if firstMusic {
		o.firstMusic = true
	}
	started := o.started
	o.mu.Unlock()
	o.log.Info("now playing", "event", "now_playing", "source", s.label())
	if firstMusic {
		seconds := time.Since(started).Seconds()
		o.log.Info("first music playing", "event", "first_music", "seconds_since_start", seconds)
		o.emit("music started")
	}
}

// summarize renders a short now-playing description of a track.
func summarize(t *engine.Track) string {
	p := t.Prompt
	if len(p) > 60 {
		p = p[:57] + "..."
	}
	if t.FromLibrary {
		p += " [library]"
	}
	if t.Lyrics != "" && t.Lyrics != engine.InstrumentalLyrics {
		p += " [vocals]"
	}
	return p
}
