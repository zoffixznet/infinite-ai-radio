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

// bufferGauge describes the store for the status bar: while a batch
// runs, how much of it is rendered; between batches, how full the
// store is and when the tap next runs. ok is false when there is
// nothing meaningful to draw.
func bufferGauge(st player.Status) (frac, consumed float64, text string, ok bool) {
	if !st.Phased {
		if st.BufferTarget <= 0 {
			return 0, 0, "", false
		}
		return float64(st.Queued) / float64(st.BufferTarget), 0,
			fmt.Sprintf("%d/%d buffered", st.Queued, st.BufferTarget), true
	}
	toPlay := st.BufferedTracks
	if st.Generating && st.RampBatch > 0 {
		// The batch is live and its progress is the news.
		text = fmt.Sprintf("%d of %d songs rendered · %d to play",
			st.BatchRendered, st.RampBatch, toPlay)
		if st.PlannedTracks > 0 {
			text += fmt.Sprintf(" · %d planned", st.PlannedTracks)
		}
		return float64(st.BatchRendered) / float64(st.RampBatch), 0, text, true
	}
	// Between batches the batch's own count is finished business: a
	// cycle that stopped at 11 of 20 because the writer ran out of
	// words is not still working on the other nine, and saying "11 of
	// 20 rendered" beside an idle engine reads as a stall. What matters
	// then is how much music is banked and what starts the next batch.
	text = fmt.Sprintf("%d songs to play (%s)", toPlay, fmtSpan(st.BufferedSeconds))
	switch {
	case st.StoreTarget > 0 && st.StoreLevel >= st.StoreTarget:
		text += " · store full"
	case st.NextBatchIn > 0:
		text += " · next batch in " + fmtSpan(st.NextBatchIn.Seconds())
	case st.RampBatch > 0:
		text += fmt.Sprintf(" · next batch of %d due", st.RampBatch)
	}
	if st.StoreTarget > 0 {
		frac = float64(st.StoreLevel) / float64(st.StoreTarget)
	}
	return frac, 0, text, true
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
