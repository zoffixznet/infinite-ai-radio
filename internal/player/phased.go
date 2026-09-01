package player

import (
	"context"
	"time"

	"iar/internal/audio"
	"iar/internal/engine"
	"iar/internal/library"
	"iar/internal/session"
)

// Phased generation runs the two models in separate windows so they
// never share the graphics card: a cycle plans a batch of songs (the
// planner LM alone on the card, the diffusion model dropped), then
// renders the batch (the diffusion model alone, the planner untouched),
// stores everything on disk, and puts the engine to sleep. Playback
// feeds from the disk buffer; the engine wakes only when the buffer
// runs low or the listener steers.
//
// Batch sizes ramp with confidence in the steering context: the first
// track of a fresh context goes through alone (music as fast as the
// fused path), the next batch is small, and only after the listener has
// let a handful of songs play uninterrupted does the cycle commit to
// the full plan-ahead depth - so an evening of prompt-fiddling never
// wastes an hour of planned songs.
const (
	// phasedPrefetch is how many decoded tracks sit in memory ahead of
	// playback (the one playing rides the mixer; these cover decode
	// latency and the crossfade).
	phasedPrefetch = 2
	// rampSmallBatch is the batch size after the first track lands.
	rampSmallBatch = 10
	// rampStableTracks is how many tracks must play in a context before
	// the cycle goes to the full configured depth.
	rampStableTracks = 5
	// engineWakeBudget bounds one engine wake (cold start plus model
	// load); beyond it the cycle backs off and retries.
	engineWakeBudget = 4 * time.Minute
)

// phasedEngine is what phased generation needs from the engine.
type phasedEngine interface {
	Ready() bool
	Plan(ctx context.Context, spec engine.Spec) (*engine.Plan, error)
	Render(ctx context.Context, plan *engine.Plan) (*engine.Track, error)
}

// phasedEnabled reports whether this player runs the phased pipeline.
func (o *Orchestrator) phasedEnabled() bool {
	if o.Buffer == nil || !o.cfg.Buffer.Phased {
		return false
	}
	_, ok := o.eng.(phasedEngine)
	return ok
}

// syncPhasedEpoch aligns phased state with the steering epoch: a steer
// makes every stored plan and rendered song stale, so they are dropped
// and the ramp starts over.
func (o *Orchestrator) syncPhasedEpoch() int {
	o.mu.Lock()
	epoch := o.epoch
	changed := epoch != o.phasedEpoch
	if changed {
		o.phasedEpoch = epoch
		o.playedInEpoch = 0
		o.phasedSeq = 0
	}
	o.mu.Unlock()
	if changed {
		if dropped := o.Buffer.DropOtherEpochs(epoch); dropped > 0 {
			o.log.Info("stale buffered work dropped", "event", "buffer_epoch_dropped",
				"epoch", epoch, "files", dropped)
		}
	}
	return epoch
}

// nextPhasedSeq hands out sequence numbers, resuming after a restart.
func (o *Orchestrator) nextPhasedSeq(epoch int) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.phasedSeq == 0 {
		o.phasedSeq = o.Buffer.MaxSeq(epoch)
	}
	o.phasedSeq++
	return o.phasedSeq
}

// bufferedSeconds is the audio already secured for the epoch: rendered
// songs on disk plus the decoded prefetch in memory.
func (o *Orchestrator) bufferedSeconds(epoch int) float64 {
	_, secs := o.Buffer.TrackStats(epoch)
	o.mu.Lock()
	for _, t := range o.queue {
		secs += t.Duration().Seconds()
	}
	o.mu.Unlock()
	return secs
}

// cycleTargets returns the plan-ahead and render-ahead targets in
// seconds for the current ramp stage, and the stage's batch cap.
func (o *Orchestrator) cycleTargets(epoch int) (planSecs, renderSecs float64, batchCap int) {
	o.mu.Lock()
	played := o.playedInEpoch
	o.mu.Unlock()
	full := float64(o.cfg.Buffer.PlanAheadMinutes) * 60
	render := float64(o.cfg.Buffer.RenderAheadMinutes) * 60
	switch {
	case played == 0:
		// A fresh context: one song through both phases, playing as
		// soon as possible; the listener may be about to steer again.
		return 0, 0, 1
	case played < rampStableTracks:
		return 0, 0, rampSmallBatch
	default:
		return full, render, 0
	}
}

// wantCycle reports whether the engine should wake and produce.
func (o *Orchestrator) wantCycle(epoch int) bool {
	o.mu.Lock()
	mode := o.sess.Mode
	o.mu.Unlock()
	if mode != session.ModeMusic {
		return false
	}
	low := float64(o.cfg.Buffer.RenderLowMinutes) * 60
	planned, plannedSecs := o.Buffer.PlanStats(epoch)
	buffered := o.bufferedSeconds(epoch)
	if buffered == 0 && planned == 0 {
		// Nothing anywhere: the first track of a context always comes.
		return true
	}
	if buffered < low {
		return true
	}
	// Plans below target with the render buffer healthy: only worth a
	// wake at full depth (small ramp batches plan and render together).
	o.mu.Lock()
	played := o.playedInEpoch
	o.mu.Unlock()
	if played >= rampStableTracks {
		full := float64(o.cfg.Buffer.PlanAheadMinutes) * 60
		if plannedSecs+buffered < full && planned == 0 {
			return true
		}
	}
	return false
}

// cycleLoop is phased generation's producer: it sleeps until work is
// needed, wakes the engine, runs a plan batch then a render batch, and
// hibernates the engine again.
func (o *Orchestrator) cycleLoop(ctx context.Context) {
	failures := 0
	oomStreak := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-o.wake:
		case <-time.After(2 * time.Second):
		}
		epoch := o.syncPhasedEpoch()
		if !o.wantCycle(epoch) {
			continue
		}
		o.runCycle(ctx, &failures, &oomStreak)
		if ctx.Err() != nil {
			return
		}
	}
}

// runCycle wakes the engine, plans, renders, and hibernates.
func (o *Orchestrator) runCycle(ctx context.Context, failures, oomStreak *int) {
	pe := o.eng.(phasedEngine)
	o.setEngineActive(true)
	if !o.waitEngineReady(ctx) {
		o.log.Warn("engine did not become ready for a cycle", "event", "cycle_engine_unready")
		return
	}

	epoch := o.syncPhasedEpoch()
	planTarget, renderTarget, batchCap := o.cycleTargets(epoch)

	// Plan phase: the planner writes songs until the batch is full.
	// With the disk-backed DiT the first plan job clears the diffusion
	// model off the card, so the whole batch runs in the small window.
	plannedThisCycle := 0
	for {
		if ctx.Err() != nil {
			return
		}
		if cur := o.syncPhasedEpoch(); cur != epoch {
			return // steered mid-batch; everything planned so far is gone
		}
		_, plannedSecs := o.Buffer.PlanStats(epoch)
		have := plannedSecs + o.bufferedSeconds(epoch)
		if batchCap > 0 && plannedThisCycle >= batchCap {
			break
		}
		if batchCap == 0 && have >= planTarget {
			break
		}
		_, sess := o.snapshotSession()
		spec := o.builder.BuildSpec(ctx, sess, o.cfg.TrackSeconds)
		o.builder.TitleAsync(specPromptForLog(spec))
		o.setGenBusy(true)
		o.log.Info("plan started", "event", "plan_started",
			"prompt", specPromptForLog(spec), "lyric_mode", lyricMode(spec),
			"vocal", spec.Vocal(), "epoch", epoch)
		start := time.Now()
		o.genMu.Lock()
		plan, err := pe.Plan(ctx, spec)
		o.genMu.Unlock()
		o.setGenBusy(false)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			if !o.notePhasedFailure(ctx, "plan", err, failures, oomStreak) {
				return
			}
			continue
		}
		o.notePhasedSuccess(failures, oomStreak)
		seq := o.nextPhasedSeq(epoch)
		if err := o.Buffer.PutPlan(epoch, seq, plan); err != nil {
			o.log.Error("plan not stored", "event", "buffer_plan_failed", "error", err.Error())
			return
		}
		plannedThisCycle++
		o.log.Info("plan finished", "event", "plan_finished",
			"elapsed_seconds", time.Since(start).Seconds(), "plan_seconds", plan.Seconds,
			"epoch", epoch, "seq", seq)
	}

	// Render phase: the diffusion model mounts once (from disk) and
	// burns through the stored plans.
	for {
		if ctx.Err() != nil {
			return
		}
		if cur := o.syncPhasedEpoch(); cur != epoch {
			return
		}
		if renderTarget > 0 && o.bufferedSeconds(epoch) >= renderTarget {
			break
		}
		plan, seq, ok := o.Buffer.NextPlan(epoch)
		if !ok {
			break
		}
		o.setGenBusy(true)
		start := time.Now()
		o.genMu.Lock()
		track, err := pe.Render(ctx, plan)
		o.genMu.Unlock()
		o.setGenBusy(false)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			if !o.notePhasedFailure(ctx, "render", err, failures, oomStreak) {
				return
			}
			continue
		}
		o.notePhasedSuccess(failures, oomStreak)
		if o.cfg.NormalizeLoudness {
			audio.NormalizeLoudness(track.Samples, audio.DefaultTargetRMS)
		}
		if err := o.Buffer.PutTrack(ctx, epoch, seq, track); err != nil {
			o.log.Error("rendered track not stored", "event", "buffer_track_failed", "error", err.Error())
			return
		}
		o.Buffer.DropPlan(epoch, seq)
		elapsed := time.Since(start)
		o.mu.Lock()
		o.genCount++
		o.lastGen = elapsed
		o.mu.Unlock()
		o.log.Info("render finished", "event", "render_finished",
			"elapsed_seconds", elapsed.Seconds(), "track_seconds", track.Duration().Seconds(),
			"epoch", epoch, "seq", seq)
	}

	// The ramp may already owe more work (a stage-0 track lands and
	// immediately unlocks the small batch): keep the engine warm and
	// let the loop re-enter instead of paying a pointless cold start.
	if o.wantCycle(o.syncPhasedEpoch()) {
		return
	}
	// Work done: give the card and the memory back until the buffer
	// runs low again.
	o.hibernateEngine()
}

// feedLoop keeps the in-memory prefetch filled from the disk buffer.
func (o *Orchestrator) feedLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-o.wake:
		case <-time.After(500 * time.Millisecond):
		}
		o.mu.Lock()
		mode := o.sess.Mode
		need := len(o.queue) < phasedPrefetch
		o.mu.Unlock()
		if mode != session.ModeMusic || !need {
			continue
		}
		epoch, sess := o.snapshotSession()
		track, ok := o.Buffer.NextTrack(ctx, epoch)
		if !ok {
			o.kickGen() // nothing on disk: the cycle loop should wake
			continue
		}
		track.ID = newTrackID()
		o.fillTitle(track, specPromptForLog(track.Spec))
		o.mu.Lock()
		kept := epoch == o.epoch
		if kept {
			o.queue = append(o.queue, track)
			o.lastGood = track
			o.lastGoodEpoch = epoch
			o.playedInEpoch++
		}
		o.mu.Unlock()
		if !kept {
			continue
		}
		// Bank on consumption: a track goes into the instant-start
		// library when it is actually about to be heard, so a dropped
		// buffer never churns the library.
		key := library.Key(sess)
		o.wg.Add(1)
		go func(t *engine.Track) {
			defer o.wg.Done()
			if err := o.Library.Put(key, t); err != nil {
				o.log.Debug("library banking failed", "event", "library_put_failed", "error", err.Error())
			}
		}(track)
	}
}

// setGenBusy flips the status flag around one engine job.
func (o *Orchestrator) setGenBusy(busy bool) {
	o.mu.Lock()
	o.genBusy = busy
	o.mu.Unlock()
}

// notePhasedFailure applies the shared failure policy (OOM backoff,
// restart streaks) to a failed plan or render job. It returns false
// when the cycle should be abandoned for now.
func (o *Orchestrator) notePhasedFailure(ctx context.Context, kind string, err error, failures, oomStreak *int) bool {
	reason := err.Error()
	deviceFault := isDeviceFault(reason)
	oom := isOutOfMemory(reason)
	if oom {
		*oomStreak++
	} else {
		*oomStreak = 0
		*failures++
	}
	o.mu.Lock()
	o.failStreak = *failures
	o.lastFailure = reason
	o.mu.Unlock()
	o.log.Error("generation failed", "event", "generation_failed",
		"kind", kind, "error", reason, "failures", *failures,
		"device_fault", deviceFault, "out_of_memory", oom)
	if oom {
		o.emit("the graphics card is full right now; waiting for room before generating again")
	}
	if !oom && (deviceFault || *failures >= restartStreak) {
		if rst, ok := o.eng.(interface{ RestartEngine(string) bool }); ok && rst.RestartEngine(reason) {
			why := "repeated generation failures"
			if deviceFault {
				why = "a device-placement fault (it never recovers on its own)"
			}
			o.emit("engine restarting after " + why + "; music keeps playing meanwhile")
			o.log.Warn("engine restart requested", "event", "engine_restart_requested",
				"streak", *failures, "device_fault", deviceFault)
			*failures = 0
		} else if *failures%5 == 0 || deviceFault {
			o.emit("generation keeps failing and the engine cannot be restarted from here (see log and 'iar doctor')")
		}
	} else if !oom && *failures == 1 {
		o.emit("generation failed; retrying (details in the log)")
	}
	wait := backoff(*failures)
	if oom {
		wait = oomBackoff(*oomStreak)
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(wait):
	}
	// Give up on the cycle after a few consecutive failures; the loop
	// re-enters when the buffer still needs work.
	return *failures < restartStreak && *oomStreak < 3
}

// notePhasedSuccess clears the failure bookkeeping.
func (o *Orchestrator) notePhasedSuccess(failures, oomStreak *int) {
	*failures = 0
	*oomStreak = 0
	o.mu.Lock()
	o.failStreak = 0
	o.lastFailure = ""
	o.mu.Unlock()
}

// waitEngineReady blocks until the engine answers with models loaded
// (waking it involves a cold start when it was hibernated).
func (o *Orchestrator) waitEngineReady(ctx context.Context) bool {
	deadline := time.Now().Add(engineWakeBudget)
	for time.Now().Before(deadline) {
		if o.eng.Ready() {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(time.Second):
		}
	}
	return false
}

// setEngineActive tells the backend whether this player currently needs
// the engine daemon alive (heartbeats and respawns follow it).
func (o *Orchestrator) setEngineActive(active bool) {
	if a, ok := o.eng.(interface{ SetEngineActive(bool) }); ok {
		a.SetEngineActive(active)
	}
}

// hibernateEngine stops heartbeating and shuts the engine daemon down,
// giving all of its graphics and system memory back until the next
// cycle. Playback continues from the disk buffer.
func (o *Orchestrator) hibernateEngine() {
	o.setEngineActive(false)
	if h, ok := o.eng.(interface{ HibernateEngine() bool }); ok && h.HibernateEngine() {
		o.log.Info("engine hibernated until the buffer runs low", "event", "engine_hibernated")
	}
}
