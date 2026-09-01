package player

import (
	"context"
	"fmt"
	"time"

	"iar/internal/audio"
	"iar/internal/engine"
	"iar/internal/library"
	"iar/internal/session"
	"iar/internal/trackbuffer"
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
	// starveMinutes is how little rendered audio counts as an
	// emergency: below it, rendering preempts planning mid-batch so
	// playback cannot run dry. This is deliberately NOT the refill
	// trigger (render_low_minutes, 45 minutes by default). Using the
	// refill trigger here made every batch degenerate into one plan
	// and one render, with a model swap between every single song,
	// until three quarters of an hour of audio had accumulated.
	starveMinutes = 8
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

// syncPhasedState aligns the on-disk buffer with what the radio is
// actually playing. Two identities are checked: the steering-context
// key (which survives restarts - a buffer written for another session
// or steering context is wiped wholesale) and the in-run epoch (a
// steer drops the old epoch's files and restarts the ramp).
func (o *Orchestrator) syncPhasedState() int {
	_, sess := o.snapshotSession()
	key := library.Key(sess)
	o.mu.Lock()
	epoch := o.epoch
	booted := o.phasedSynced
	o.phasedSynced = true
	changed := epoch != o.phasedEpoch
	if changed {
		o.phasedEpoch = epoch
		o.playedInEpoch = 0
		o.phasedSeq = 0
	}
	o.mu.Unlock()
	if !booted {
		// A crash, a kill or a power cut leaves half-written files and
		// songs missing one of their two halves. Nothing else collects
		// them, so they would sit on disk for good.
		if dropped, freed := o.Buffer.Sweep(); dropped > 0 {
			o.log.Info("buffer debris from an interrupted run removed",
				"event", "buffer_swept", "files", dropped, "bytes", freed)
		}
	}
	if was := o.Buffer.RenderVersionOf(); !booted && was != trackbuffer.RenderVersion {
		// Songs on disk came out of a render path that has since been
		// fixed. Their plans are still good, so only the audio goes.
		if dropped := o.Buffer.DropTracks(); dropped > 0 {
			o.log.Info("buffered songs from an older render path dropped",
				"event", "buffer_render_version_dropped", "songs", dropped,
				"was", was, "now", trackbuffer.RenderVersion)
			o.emit(fmt.Sprintf("dropped %d buffered song(s) made by an older render path; they are being rendered again", dropped))
		}
		o.Buffer.SetRenderVersion(trackbuffer.RenderVersion)
	}
	if o.Buffer.Context() != key {
		if dropped := o.Buffer.DropAll(); dropped > 0 {
			o.log.Info("buffered work from another context dropped", "event", "buffer_context_dropped",
				"files", dropped)
		}
		o.Buffer.SetContext(key)
		return epoch
	}
	if changed || !booted {
		// On the first sync of a run this also clears orphans from a
		// previous run's steered epochs (the counter restarts at zero).
		if dropped := o.Buffer.DropOtherEpochs(epoch); dropped > 0 {
			o.log.Info("stale buffered work dropped", "event", "buffer_epoch_dropped",
				"epoch", epoch, "files", dropped)
		}
		if changed {
			o.Buffer.SetContext(key)
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

// publishBufferStats records what the on-disk buffer holds, so status
// displays can report the real depth without scanning the buffer
// directory themselves several times a second.
func (o *Orchestrator) publishBufferStats(epoch int) {
	tracks, secs := o.Buffer.TrackStats(epoch)
	plans, planSecs := o.Buffer.PlanStats(epoch)
	o.mu.Lock()
	o.bufTracks = tracks
	o.bufSeconds = secs
	o.bufPlans = plans
	o.bufPlanSeconds = planSecs
	for _, t := range o.queue {
		o.bufTracks++
		o.bufSeconds += t.Duration().Seconds()
	}
	o.mu.Unlock()
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
	cooldown := o.cycleCooldown
	o.mu.Unlock()
	if mode != session.ModeMusic {
		return false
	}
	if time.Now().Before(cooldown) {
		// A recent cycle gave up on persistent failures; do not spin
		// the engine awake again until the cooldown passes.
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
		epoch := o.syncPhasedState()
		// The status display reads these; counting them here keeps the
		// directory scan on the producer's timer instead of on every
		// repaint.
		o.publishBufferStats(epoch)
		if !o.wantCycle(epoch) {
			continue
		}
		o.runCycle(ctx, &failures, &oomStreak)
		if ctx.Err() != nil {
			return
		}
	}
}

// runCycle wakes the engine and produces in interleaved bursts: it
// plans while the rendered buffer is healthy, switches to rendering
// whenever the buffer nears the low-water mark (playback must never
// starve behind a long planning run), and tops the rendered buffer to
// target before finishing. Model swaps happen once per burst switch,
// not per song. Every exit path decides hibernation: the engine stays
// warm only when more work is already due.
func (o *Orchestrator) runCycle(ctx context.Context, failures, oomStreak *int) {
	pe := o.eng.(phasedEngine)
	o.setEngineActive(true)
	defer func() {
		if ctx.Err() == nil && o.wantCycle(o.syncPhasedState()) {
			return // more work due (ramp climbing, or a fresh epoch): stay warm
		}
		if o.exportingNow() {
			return // an export owns the engine; it finishes on its own
		}
		o.hibernateEngine()
	}()
	if !o.waitEngineReady(ctx) {
		o.log.Warn("engine did not become ready for a cycle", "event", "cycle_engine_unready")
		o.coolDown(2 * time.Minute)
		return
	}

	epoch := o.syncPhasedState()
	planTarget, renderTarget, batchCap := o.cycleTargets(epoch)
	if renderTarget <= 0 {
		// Ramp stages render up to the configured depth too; the cap
		// is on how many new plans they write.
		renderTarget = float64(o.cfg.Buffer.RenderAheadMinutes) * 60
	}
	// starve is the floor that interrupts a plan burst, never the
	// refill target: a batch is only a batch if planning gets to run.
	starve := float64(starveMinutes) * 60
	if low := float64(o.cfg.Buffer.RenderLowMinutes) * 60; starve > low {
		starve = low
	}
	plannedThisCycle := 0
	storeFails := 0

	planOne := func(sess *session.Session) bool {
		spec := o.builder.BuildSpec(ctx, sess, o.cfg.TrackSeconds)
		if !spec.Vocal() {
			// Instrumentals have no words to name from; the generic
			// prompt-keyed name is the best available.
			o.builder.TitleAsync(specPromptForLog(spec))
		}
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
			return false
		}
		if err != nil {
			return o.notePhasedFailure(ctx, "plan", err, failures, oomStreak)
		}
		o.notePhasedSuccess(failures, oomStreak)
		seq := o.nextPhasedSeq(epoch)
		if err := o.Buffer.PutPlan(epoch, seq, plan); err != nil {
			storeFails++
			o.log.Error("plan not stored", "event", "buffer_plan_failed", "error", err.Error())
			if storeFails == 1 {
				o.emit("the track buffer cannot be written (" + err.Error() + ")")
			}
			return storeFails < 3
		}
		plannedThisCycle++
		if plan.Lyrics != "" && plan.Lyrics != engine.InstrumentalLyrics {
			// Name the song from its own words, keyed per song - two
			// songs from the same context must never share a name.
			o.builder.TitleSongAsync(songTitleKey(epoch, seq), plan.Caption, plan.Lyrics)
		}
		o.log.Info("plan finished", "event", "plan_finished",
			"elapsed_seconds", time.Since(start).Seconds(), "plan_seconds", plan.Seconds,
			"epoch", epoch, "seq", seq)
		return true
	}

	renderOne := func() bool {
		plan, seq, ok := o.Buffer.NextPlan(epoch)
		if !ok {
			return false
		}
		o.setGenBusy(true)
		start := time.Now()
		o.genMu.Lock()
		track, err := pe.Render(ctx, plan)
		o.genMu.Unlock()
		o.setGenBusy(false)
		if ctx.Err() != nil {
			return false
		}
		if err != nil {
			// A plan whose render keeps failing must not block the
			// whole pipeline from the head of the queue.
			o.mu.Lock()
			if o.renderFails == nil {
				o.renderFails = map[int]int{}
			}
			o.renderFails[seq]++
			bad := o.renderFails[seq] >= 3
			o.mu.Unlock()
			if bad {
				o.Buffer.DropPlan(epoch, seq)
				o.log.Warn("plan dropped after repeated render failures",
					"event", "plan_dropped", "epoch", epoch, "seq", seq, "error", err.Error())
			}
			return o.notePhasedFailure(ctx, "render", err, failures, oomStreak)
		}
		o.notePhasedSuccess(failures, oomStreak)
		if o.cfg.NormalizeLoudness {
			audio.NormalizeLoudness(track.Samples, audio.DefaultTargetRMS)
		}
		key := songTitleKey(epoch, seq)
		o.applySongTitle(track, key)
		if err := o.Buffer.PutTrack(ctx, epoch, seq, key, track); err != nil {
			storeFails++
			o.log.Error("rendered track not stored", "event", "buffer_track_failed", "error", err.Error())
			if storeFails == 1 {
				o.emit("the track buffer cannot be written (" + err.Error() + ")")
			}
			return storeFails < 3
		}
		o.Buffer.DropPlan(epoch, seq)
		elapsed := time.Since(start)
		o.mu.Lock()
		o.genCount++
		o.lastGen = elapsed
		delete(o.renderFails, seq)
		o.mu.Unlock()
		o.log.Info("render finished", "event", "render_finished",
			"elapsed_seconds", elapsed.Seconds(), "track_seconds", track.Duration().Seconds(),
			"epoch", epoch, "seq", seq)
		return true
	}

	for {
		if ctx.Err() != nil {
			return
		}
		if cur := o.syncPhasedState(); cur != epoch {
			return // steered mid-cycle; the defer decides warm vs sleep
		}
		if storeFails >= 3 {
			o.coolDown(2 * time.Minute)
			return
		}
		planned, plannedSecs := o.Buffer.PlanStats(epoch)
		buffered := o.bufferedSeconds(epoch)
		var ok bool
		switch nextCycleStep(cycleState{
			planned:          planned,
			plannedSecs:      plannedSecs,
			buffered:         buffered,
			planTarget:       planTarget,
			renderTarget:     renderTarget,
			starve:           starve,
			batchCap:         batchCap,
			plannedThisCycle: plannedThisCycle,
		}) {
		case stepRender:
			ok = renderOne()
		case stepPlan:
			_, sess := o.snapshotSession()
			ok = planOne(sess)
		default:
			return // all targets met; the defer hibernates
		}
		if !ok {
			if ctx.Err() == nil {
				o.coolDown(time.Minute)
			}
			return
		}
	}
}

// cycleState is everything one scheduling decision depends on.
type cycleState struct {
	// planned and plannedSecs are the stored plans awaiting a render.
	planned     int
	plannedSecs float64
	// buffered is the rendered audio already secured, in seconds.
	buffered float64
	// planTarget and renderTarget are the depths this ramp stage aims
	// for; starve is the depth below which playback is in danger.
	planTarget   float64
	renderTarget float64
	starve       float64
	// batchCap limits how many plans this cycle writes (0 means plan to
	// planTarget instead); plannedThisCycle counts those written so far.
	batchCap         int
	plannedThisCycle int
}

// cycleStep is what a cycle does next.
type cycleStep int

const (
	stepDone cycleStep = iota
	stepPlan
	stepRender
)

// nextCycleStep chooses between planning ahead and rendering now.
// Rendering preempts a plan burst only when playback is genuinely close
// to running dry, so the models swap once per burst rather than once
// per song. The far larger refill trigger decides whether a cycle runs
// at all, never what it does once it is running - conflating the two
// turned every batch into a single plan followed by a single render.
func nextCycleStep(s cycleState) cycleStep {
	if s.planned > 0 && s.buffered < s.starve {
		return stepRender
	}
	needPlan := (s.batchCap > 0 && s.plannedThisCycle < s.batchCap) ||
		(s.batchCap == 0 && s.plannedSecs+s.buffered < s.planTarget)
	if needPlan {
		return stepPlan
	}
	if s.planned > 0 && s.buffered < s.renderTarget {
		return stepRender
	}
	return stepDone
}

// songTitleKey names the helper's title slot for one planned song.
func songTitleKey(epoch, seq int) string {
	return fmt.Sprintf("song:%d-%d", epoch, seq)
}

// applySongTitle sets the song-keyed helper name when it is ready;
// otherwise the track keeps an empty title for a later resolution
// attempt (feed time), with the deterministic fallback as last resort.
func (o *Orchestrator) applySongTitle(t *engine.Track, key string) {
	if title, subtitle, ok := o.builder.TitleForKey(key); ok {
		t.Title = title
		if subtitle != "" {
			t.Subtitle = subtitle
		}
		return
	}
	if !t.Spec.Vocal() {
		// Instrumentals fall back to the prompt-keyed generic name.
		o.fillTitle(t, specPromptForLog(t.Spec))
	}
}

// coolDown blocks new cycles for a while after persistent failures, so
// an unfixable condition cannot spin the engine awake in a hot loop.
func (o *Orchestrator) coolDown(d time.Duration) {
	o.mu.Lock()
	o.cycleCooldown = time.Now().Add(d)
	o.mu.Unlock()
}

// exportingNow reports whether an export is running.
func (o *Orchestrator) exportingNow() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.exporting != ""
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
		track, titleKey, ok := o.Buffer.NextTrack(ctx, epoch)
		if !ok {
			o.kickGen() // nothing on disk: the cycle loop should wake
			continue
		}
		track.ID = newTrackID()
		if track.Title == "" && titleKey != "" {
			// The helper may have finished naming the song after it was
			// rendered; feed time is the last chance to pick that up.
			o.applySongTitle(track, titleKey)
		}
		if track.Title == "" {
			o.fillTitle(track, specPromptForLog(track.Spec))
		}
		o.mu.Lock()
		kept := epoch == o.epoch
		if kept {
			o.queue = append(o.queue, track)
			o.lastGood = track
			o.lastGoodEpoch = epoch
		}
		o.mu.Unlock()
		o.publishBufferStats(epoch)
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
	if o.eng == nil {
		return false
	}
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
