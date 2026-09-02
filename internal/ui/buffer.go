package ui

import (
	"fmt"
	"time"

	"iar/internal/player"
)

// Phased generation buffers songs to disk, so the number worth showing
// is how much music is secured - not how many tracks happen to be
// decoded in memory. The in-memory prefetch is two tracks by design and
// says nothing about whether the radio is keeping up.

// bufferReady is the short sentence for the now-playing panel.
func bufferReady(st player.Status) string {
	if !st.Phased {
		return fmt.Sprintf("%d track(s) ready", st.Queued)
	}
	if st.BufferedTracks == 0 {
		if st.PlannedTracks > 0 {
			return fmt.Sprintf("nothing rendered yet, %d planned", st.PlannedTracks)
		}
		return "nothing buffered yet"
	}
	return fmt.Sprintf("%d song(s) ready, %s of music",
		st.BufferedTracks, fmtSpan(st.BufferedSeconds))
}

// nowLine names what is playing. A track carries its own name, derived
// from its lyrics; the source label is only the first sixty characters
// of the steering prompt, which reads as "An Aggressive And
// High-Energy...". The name is the interesting part; the genre line
// follows it.
func nowLine(st player.Status) string {
	if st.TrackTitle == "" {
		return st.Source
	}
	if st.TrackSubtitle == "" {
		return st.TrackTitle
	}
	return st.TrackTitle + " · " + st.TrackSubtitle
}

// genIdle is what the generation row says when nothing is in flight.
// The row is drawn either way: it flips several times a minute as the
// cycle moves between plan and render jobs, and a row that comes and
// goes drags every line under it up and down the screen.
func genIdle(st player.Status) string {
	switch {
	case st.Exporting != "":
		return "idle · the engine is busy with an export"
	case st.Phased && !st.EngineReady:
		// Phased generation puts the engine to sleep on purpose once
		// the buffer is deep enough; that is the pipeline working.
		return "idle · engine asleep"
	case st.EngineName != "" && !st.EngineReady:
		return "idle · engine not ready"
	}
	return "idle"
}

// bufferGauge describes the buffer for the status bar: how full it is
// against the depth the cycle aims for, and the line beside the bar.
// ok is false when there is nothing meaningful to draw.
func bufferGauge(st player.Status) (frac float64, text string, ok bool) {
	if !st.Phased {
		if st.BufferTarget <= 0 {
			return 0, "", false
		}
		return float64(st.Queued) / float64(st.BufferTarget),
			fmt.Sprintf("%d/%d buffered", st.Queued, st.BufferTarget), true
	}
	// Songs are the exact number; spans of time are how long those
	// songs happen to run. The bar tracks whichever target the current
	// ramp stage is filling: a batch of N songs early on, the
	// configured depth of audio once steering settles.
	if st.RampBatch > 0 {
		done := st.BufferedTracks
		if done > st.RampBatch {
			done = st.RampBatch
		}
		frac = float64(done) / float64(st.RampBatch)
		text = fmt.Sprintf("%d of %d songs this batch", done, st.RampBatch)
		if st.PlannedTracks > 0 {
			text += fmt.Sprintf(" · %d planned", st.PlannedTracks)
		}
		return frac, text, true
	}
	if st.BufferTargetSeconds > 0 {
		frac = st.BufferedSeconds / st.BufferTargetSeconds
	}
	text = fmt.Sprintf("%d songs (%s) rendered", st.BufferedTracks, fmtSpan(st.BufferedSeconds))
	if st.BufferTargetSeconds > 0 {
		if st.BufferedSeconds >= st.BufferTargetSeconds {
			text += fmt.Sprintf(" · %s target met", fmtSpan(st.BufferTargetSeconds))
		} else {
			text += fmt.Sprintf(" of the %s target", fmtSpan(st.BufferTargetSeconds))
		}
	}
	if st.PlannedTracks > 0 {
		text += fmt.Sprintf(" · %d planned (%s)", st.PlannedTracks, fmtSpan(st.PlannedSeconds))
	}
	if st.BufferLowSeconds > 0 {
		text += fmt.Sprintf(" · next batch when %s left", fmtSpan(st.BufferLowSeconds))
	}
	return frac, text, true
}

// fmtSpan writes a stretch of audio the way a listener thinks about it:
// hours and minutes for a deep buffer, minutes and seconds for a
// shallow one.
func fmtSpan(seconds float64) string {
	if seconds <= 0 {
		return "0m"
	}
	d := time.Duration(seconds * float64(time.Second))
	if d >= time.Hour {
		return fmt.Sprintf("%dh%02dm", int(d/time.Hour), int(d/time.Minute)%60)
	}
	if d >= time.Minute {
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
	return fmt.Sprintf("%ds", int(d/time.Second))
}
