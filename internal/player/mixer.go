package player

import (
	"context"

	"bgm/internal/audio"
	"bgm/internal/engine"
	"bgm/internal/session"
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

	for ctx.Err() == nil {
		o.mu.Lock()
		cur := o.cur
		wantSwitch := o.switchReq
		o.switchReq = false
		o.mu.Unlock()

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

// startupSource picks the very first audio source for the session.
func (o *Orchestrator) startupSource() source {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.sess.Mode == session.ModeNoise {
		color := audio.ParseNoiseColor(o.sess.NoiseColor)
		return newNoiseSource(color, noiseAmp, string(color)+" noise")
	}
	if o.eng != nil {
		o.emit("starting with a gentle noise bed while the first track generates")
	} else {
		o.emit("music engine unavailable; playing the session's noise bed (run 'bgm setup' or see 'bgm doctor')")
	}
	color := audio.ParseNoiseColor(o.sess.NoiseBed)
	return newNoiseSource(color, bedAmp, string(color)+" noise bed")
}

// bedSource returns the session's fallback noise bed.
func (o *Orchestrator) bedSource() source {
	o.mu.Lock()
	color := audio.ParseNoiseColor(o.sess.NoiseBed)
	o.mu.Unlock()
	return newNoiseSource(color, bedAmp, string(color)+" noise bed")
}

// fallbackShouldYield reports whether cur is a stopgap (bed or loop) that
// should hand over to real queued content.
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
	if _, isNoise := cur.(*noiseSource); isNoise {
		return len(o.queue) > 0
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

	if len(o.queue) > 0 {
		t := o.queue[0]
		o.queue = o.queue[1:]
		defer o.kickGen()
		return newTrackSource(t, summarize(t))
	}

	// Nothing queued. Keep an endless source running rather than
	// restarting it.
	if _, isNoise := cur.(*noiseSource); isNoise {
		return nil
	}

	if o.lastGood != nil {
		o.log.Warn("queue empty, looping last track", "event", "loop_fallback")
		o.emit("generator is behind; looping the last track")
		return newTrackSource(o.lastGood, summarize(o.lastGood)+" (looping)")
	}

	o.log.Warn("queue empty with no last track, noise bed fallback", "event", "bed_fallback")
	o.emit("no generated music available; playing the noise bed")
	color := audio.ParseNoiseColor(o.sess.NoiseBed)
	return newNoiseSource(color, bedAmp, string(color)+" noise bed")
}

// crossfade streams an equal-power transition from cur into next, then
// installs next as current. Chunked so shutdown stays responsive.
func (o *Orchestrator) crossfade(ctx context.Context, cur, next source, fadeFrames, chunkFrames int) {
	fade := fadeFrames
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
	o.mu.Unlock()
	o.log.Info("now playing", "event", "now_playing", "source", s.label())
}

// summarize renders a short now-playing description of a track.
func summarize(t *engine.Track) string {
	p := t.Prompt
	if len(p) > 60 {
		p = p[:57] + "..."
	}
	if t.Lyrics != "" && t.Lyrics != engine.InstrumentalLyrics {
		p += " [vocals]"
	}
	return p
}
