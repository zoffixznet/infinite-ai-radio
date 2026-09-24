package player

import (
	"context"
	"fmt"
	"time"

	"iar/internal/audio"
	"iar/internal/engine"
	"iar/internal/library"
	"iar/internal/prompting"
	"iar/internal/session"
	"iar/internal/songbook"
	"iar/internal/trackbuffer"
)

// Phased generation runs the two models in separate windows so they
// never share the graphics card: a cycle plans a batch of songs (the
// planner LM alone on the card, the diffusion model dropped), then
// renders the batch (the diffusion model alone, the planner untouched),
// stores everything in the tub, and puts the engine to sleep. The
// engine wakes only when the tub's level - the songs nobody has taken
// yet - is below the configured depth, and a rung is due.
//
// Batch sizes climb a ladder on the clock: one opener as fast as
// possible, then ten songs straight after it, then 20, 40 and 80. After
// a rung is made the tap waits as long as that rung's music runs before
// the next, less whatever listeners skipped - so an evening of
// prompt-fiddling never wastes an hour of planned songs, and a tub
// nobody is drawing from still fills while the house is empty.
const (
	// phasedPrefetch is how many decoded tracks sit in memory ahead of
	// playback (the one playing rides the mixer; these cover decode
	// latency and the crossfade).
	phasedPrefetch = 2
	// engineWakeBudget bounds one engine wake (cold start plus model
	// load); beyond it the cycle backs off and retries.
	engineWakeBudget = 4 * time.Minute
)

// ladder is the batch ladder: how many songs each rung makes. The
// opener rung is one song; the ceiling rung refills the tub in
// batches of eighty (capped at the room left in it).
var ladder = [...]int{1, 10, 20, 40, 80}

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

// adoptDiskBuffer continues from the buffer a previous run left
// behind: when the stored context matches the session's, the highest
// epoch on disk becomes this run's, and stray older epochs are
// dropped. Run before the loops start, so the first sync sees a
// matching world.
func (o *Orchestrator) adoptDiskBuffer() {
	_, sess := o.snapshotSession()
	if sess == nil || o.Buffer.Context() != library.Key(sess) {
		return
	}
	de, ok := o.Buffer.DiskEpoch()
	if !ok {
		return
	}
	o.mu.Lock()
	o.epoch = de
	o.phasedEpoch = de
	o.mu.Unlock()
	if dropped := o.Buffer.DropOtherEpochs(de); dropped > 0 {
		o.log.Info("older epochs cleared while adopting the buffer",
			"event", "buffer_adopt_cleared", "files", dropped)
	}
	tracks, secs := o.Buffer.TrackStats(de)
	o.log.Info("buffer adopted from the previous run", "event", "buffer_adopted",
		"epoch", de, "songs", tracks, "seconds", secs)
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
		o.phasedSeq = 0
		// A fresh context starts the ladder over at the opener.
		o.rung = 0
		o.rungMade = 0
		o.rungSeconds = 0
		o.rungDoneAt = time.Time{}
		o.rungWait = 0
		o.skipCredit = 0
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
	tracks, secs := o.Buffer.Level(epoch)
	plans, planSecs := o.Buffer.PlanStats(epoch)
	o.mu.Lock()
	o.bufLevel = tracks
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

// bufferedSeconds is the audio already secured for the epoch: the
// store's level (songs nobody has taken) plus the decoded prefetch in
// memory. Songs kept for other players are behind the listener, not
// ahead, so they do not count.
func (o *Orchestrator) bufferedSeconds(epoch int) float64 {
	_, secs := o.Buffer.Level(epoch)
	o.mu.Lock()
	for _, t := range o.queue {
		secs += t.Duration().Seconds()
	}
	o.mu.Unlock()
	return secs
}

// batchFor is how many songs the next cycle makes: the rung's size,
// less what this rung has made already, capped at the room left in the
// tub. Zero when the tub is full.
func (o *Orchestrator) batchFor(epoch int) int {
	level, _ := o.Buffer.Level(epoch)
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.batchForLocked(level)
}

// batchForLocked is batchFor against a known level. Callers hold o.mu.
func (o *Orchestrator) batchForLocked(level int) int {
	size := ladder[o.rung] - o.rungMade
	if room := o.cfg.Buffer.Songs - level; size > room {
		size = room
	}
	if size < 0 {
		size = 0
	}
	return size
}

// rungWaitLeft is how long the tap still waits before the next rung:
// the last rung's music, less the time gone by and the music listeners
// skipped meanwhile. Zero when a rung is due.
func (o *Orchestrator) rungWaitLeft() time.Duration {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.rungWaitLeftLocked()
}

// rungWaitLeftLocked is rungWaitLeft under the lock.
func (o *Orchestrator) rungWaitLeftLocked() time.Duration {
	if o.rungDoneAt.IsZero() {
		return 0
	}
	left := o.rungWait - time.Since(o.rungDoneAt) - o.skipCredit
	if left < 0 {
		return 0
	}
	return left
}

// plannable is how many songs a cycle starting now may plan: the
// batch, once the rung is due; nothing while the tap is waiting.
func (o *Orchestrator) plannable(epoch int) int {
	if o.rungWaitLeft() > 0 {
		return 0
	}
	return o.batchFor(epoch)
}

// completeRung closes the rung once its batch is made (or the tub is
// full): the tap waits as long as the batch's music runs before the
// next rung, and the ladder climbs. The opener's rung waits nothing,
// so the ten-song batch follows it straight away.
func (o *Orchestrator) completeRung(epoch int) {
	level, _ := o.Buffer.Level(epoch)
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.rungMade == 0 {
		return
	}
	if o.rungMade < ladder[o.rung] && level < o.cfg.Buffer.Songs {
		return // the rung is not made yet; the next cycle carries on
	}
	o.rungWait = time.Duration(o.rungSeconds * float64(time.Second))
	if o.rung == 0 {
		o.rungWait = 0
	}
	o.rungDoneAt = time.Now()
	o.skipCredit = 0
	o.log.Info("ladder rung made", "event", "ladder_rung", "rung", ladder[o.rung],
		"songs", o.rungMade, "wait_seconds", o.rungWait.Seconds(), "level", level)
	if o.rung < len(ladder)-1 {
		o.rung++
	}
	o.rungMade = 0
	o.rungSeconds = 0
}

// ReportSkipped credits the ladder with music a listener skipped:
// skipping is faster consumption, so the next rung comes sooner.
// Listeners report it on every check for new songs; the credits add up
// across them.
func (o *Orchestrator) ReportSkipped(seconds float64) {
	if seconds <= 0 {
		return
	}
	o.mu.Lock()
	o.skipCredit += time.Duration(seconds * float64(time.Second))
	o.mu.Unlock()
	o.kickGen()
}

// wantCycle reports whether the engine should wake and produce: plans
// left over from an interrupted cycle always get their audio; past
// that, a rung is due only while the tub is below its depth and the
// last rung's wait has run out.
func (o *Orchestrator) wantCycle(epoch int) bool {
	o.mu.Lock()
	cooldown := o.cycleCooldown
	o.mu.Unlock()
	if time.Now().Before(cooldown) {
		// A recent cycle gave up on persistent failures; do not spin
		// the engine awake again until the cooldown passes.
		return false
	}
	if planned, _ := o.Buffer.PlanStats(epoch); planned > 0 {
		return true
	}
	return o.plannable(epoch) > 0
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
			// Hibernation only ever happens inside runCycle's defer, so
			// a cycle that ended staying warm and then found no work on
			// its way back - the store filled, the mode changed, a
			// cooldown started - left the daemon holding the card with
			// nothing left to enter that would put it down. This
			// goroutine is the only one that runs a cycle, so nothing
			// can be mid-plan or mid-render here; an export is the one
			// thing that legitimately owns a warm engine while the
			// radio wants no cycle of its own.
			if o.engineBusy.Load() && !o.exportingNow() {
				o.hibernateEngine()
			}
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
	o.wordsmithPhase(ctx)
	// lyricStarved is set when planning runs out of written words; the
	// defer hands the card back to the wordsmith instead of staying
	// warm.
	lyricStarved := false
	// A fresh batch cycle: the gauge's rendered-count starts over.
	o.mu.Lock()
	o.batchRenderedNow = 0
	o.mu.Unlock()

	o.setEngineActive(true)
	defer func() {
		if o.exportingNow() {
			return // an export owns the engine; it finishes on its own
		}
		if lyricStarved {
			// More plans are due, but they are waiting on words: the
			// wordsmith needs the card, so the engine sleeps even
			// though the cycle is not finished. The loop re-enters
			// within seconds.
			o.hibernateEngine()
			return
		}
		if ctx.Err() == nil && o.wantCycle(o.syncPhasedState()) {
			return // more work due (ramp climbing, or a fresh epoch): stay warm
		}
		o.hibernateEngine()
	}()
	if !o.waitEngineReady(ctx) {
		o.log.Warn("engine did not become ready for a cycle", "event", "cycle_engine_unready")
		o.coolDown(2 * time.Minute)
		return
	}

	epoch := o.syncPhasedState()
	// Nothing while the tap is waiting: a cycle then only renders the
	// plans an interrupted one left behind.
	batchCap := o.plannable(epoch)
	// This cycle's own size is what its rendered count is read against.
	o.mu.Lock()
	o.batchCapNow = batchCap
	o.mu.Unlock()
	// The rung is closed on every way out, and only closes when its
	// batch is made: an interrupted cycle leaves it open for the next.
	defer o.completeRung(epoch)
	plannedThisCycle := 0
	storeFails := 0

	planOne := func(sess *session.Session) bool {
		spec := o.builder.BuildSpec(ctx, sess, o.cfg.TrackSeconds)
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
		// The song was named when its words were written; the name came
		// here on the plan and goes to disk with the audio. Nothing
		// names it later.
		o.nameTrack(track)
		// The song gets its id here, with its audio, and keeps it for
		// good: on disk, in the play queue, and in the library once it
		// has played. It used to be named afresh at each of those
		// stops, so a phone watching the listing saw one song as two
		// or three and downloaded every one of them.
		track.ID = newTrackID()
		hash, err := o.Buffer.PutTrack(ctx, epoch, seq, track)
		if err != nil {
			storeFails++
			o.log.Error("rendered track not stored", "event", "buffer_track_failed", "error", err.Error())
			if storeFails == 1 {
				o.emit("the track buffer cannot be written (" + err.Error() + ")")
			}
			return storeFails < 3
		}
		o.Buffer.DropPlan(epoch, seq)
		// The record outlives the file: a copy of this song coming back
		// from a phone months from now is still recognised and saved
		// under this name and these words.
		if err := o.Songbook.Record(songbook.Song{
			ID: track.ID, Hash: hash, Title: track.Title, Subtitle: track.Subtitle,
			Prompt: track.Prompt, Lyrics: track.Lyrics, Language: track.Spec.VocalLanguage,
			Seconds: track.Duration().Seconds(), Created: time.Now(),
		}); err != nil {
			o.log.Warn("song not recorded in the songbook", "event", "songbook_write_failed", "error", err.Error())
		}
		elapsed := time.Since(start)
		o.mu.Lock()
		o.genCount++
		o.batchRenderedNow++
		o.rungMade++
		o.rungSeconds += track.Duration().Seconds()
		o.lastGen = elapsed
		o.produced = true
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
		planned, _ := o.Buffer.PlanStats(epoch)
		var ok bool
		switch nextCycleStep(cycleState{
			planned:          planned,
			batchCap:         batchCap,
			plannedThisCycle: plannedThisCycle,
		}) {
		case stepRender:
			ok = renderOne()
		case stepPlan:
			_, sess := o.snapshotSession()
			// Engine-invented words are allowed only for the opener,
			// when the tub is empty and a listener may be waiting on
			// the first song; past that, every song waits for the
			// writer - the audition batch especially.
			o.mu.Lock()
			opener := o.rung == 0
			o.mu.Unlock()
			if !opener && (o.builder.AwaitingLyrics(sess) || o.builder.AwaitingInstrumentalCaptions(sess)) {
				// The writer has no words ready for the next song, and
				// planning past the writer is what turns a station
				// into one song in a hundred costumes. Planning stops
				// here - but plans already written still deserve their
				// audio, so renders drain first; then the cycle ends,
				// hibernates, and the next round starts with the
				// wordsmith owning the freed card.
				if !lyricStarved {
					lyricStarved = true
					o.log.Info("plan paused for the wordsmith",
						"event", "plan_awaits_lyrics", "epoch", epoch,
						"planned_this_cycle", plannedThisCycle)
				}
				if planned > 0 {
					ok = renderOne()
					break
				}
				if plannedThisCycle == 0 {
					// The whole cycle accomplished nothing: no sheets
					// were written and nothing waited to render. A
					// failing helper would otherwise respawn the
					// engine every two seconds; give it a breath.
					o.coolDown(time.Minute)
				}
				return
			}
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
	// planned is the stored plans awaiting a render.
	planned int
	// batchCap is how many plans this cycle writes (0 for a cycle that
	// only renders what an interrupted one left); plannedThisCycle
	// counts those written so far.
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

// nextCycleStep plans the whole batch, then renders all of it: the
// models swap once per cycle, not once per song. Nothing preempts the
// plan burst - no player draws straight from the generator, so there
// is nothing to starve.
func nextCycleStep(s cycleState) cycleStep {
	if s.plannedThisCycle < s.batchCap {
		return stepPlan
	}
	if s.planned > 0 {
		return stepRender
	}
	return stepDone
}

// nameTrack settles a track's display name for good. The name written
// with the song's words rides in on the spec; a song whose words the
// engine invented, or an instrumental, takes the deterministic name
// derived from its description. Either way it is decided here, once,
// before anyone can hear the song.
func (o *Orchestrator) nameTrack(t *engine.Track) {
	if t.Title != "" && t.Subtitle != "" {
		return
	}
	// The engine's own description of what it made, which is what the
	// sidecar and every listing derive from too - one song, one name,
	// wherever it is read.
	prompt := t.Prompt
	if prompt == "" {
		prompt = specPromptForLog(t.Spec)
	}
	title, subtitle := prompting.TrackTitle(prompt)
	if t.Title == "" {
		t.Title, t.Subtitle = t.Spec.Title, t.Spec.Subtitle
	}
	if t.Title == "" {
		t.Title, t.Subtitle = title, subtitle
	}
	if t.Subtitle == "" {
		// A helper that answered with a name but no genre line would
		// otherwise leave lock screens with a blank second line for
		// good.
		t.Subtitle = subtitle
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

// playerCursor names this process's player in the store: where it got
// to is kept there, so a restart continues after the last song it took
// rather than from the oldest song still kept for other players.
const playerCursor = "player"

// feedLoop keeps the in-memory prefetch filled from the store. Taking
// a song marks it consumed and leaves it on disk for players that have
// not caught up; the store trims the oldest taken songs past the
// configured depth.
func (o *Orchestrator) feedLoop(ctx context.Context) {
	cursor := o.Buffer.Cursor(playerCursor)
	for {
		select {
		case <-ctx.Done():
			return
		case <-o.wake:
		case <-time.After(500 * time.Millisecond):
		}
		o.mu.Lock()
		// A stopped player takes nothing: the store fills for the
		// others, and the song it was on is still its next.
		need := len(o.queue) < phasedPrefetch && !o.stopped
		o.mu.Unlock()
		if !need {
			continue
		}
		epoch, sess := o.snapshotSession()
		entry, ok := o.Buffer.Next(epoch, cursor)
		if !ok {
			o.kickGen() // nothing on disk: the cycle loop should wake
			continue
		}
		track, ok := o.Buffer.Peek(ctx, epoch, entry.Base)
		if ctx.Err() != nil {
			return
		}
		if !ok {
			// Unplayable, or trimmed between the listing and the read:
			// dropped here so it cannot block the head of the store.
			o.log.Warn("unplayable buffered track dropped", "event", "buffer_track_bad", "file", entry.Base)
			o.Buffer.DropTrack(epoch, entry.Base)
			continue
		}
		base := entry.Base
		o.Buffer.Take(epoch, base)
		cursor = base
		o.Buffer.SetCursor(playerCursor, base)
		if dropped := o.Buffer.Trim(epoch, o.cfg.Buffer.Songs); dropped > 0 {
			o.log.Info("oldest taken songs trimmed from the store", "event", "buffer_trimmed", "songs", dropped)
		}
		if track.ID == "" {
			// Rendered before songs carried their own id: the name the
			// listing gave it on disk is the one it keeps.
			track.ID = bufTrackPrefix + base
		}
		// Named when its words were written, and stored with the audio;
		// the fallback covers songs the engine worded itself.
		o.nameTrack(track)
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
		// buffer never churns the library. The banked copy is a
		// snapshot taken under the lock - a listener may rename the
		// live track at any moment - and the banked location is
		// remembered so that rename reaches the sidecar too.
		key := library.Key(sess)
		o.mu.Lock()
		banked := *track // shallow: Samples are shared and immutable
		o.mu.Unlock()
		o.wg.Add(1)
		go func() {
			defer o.wg.Done()
			id, err := o.Library.Put(ctx, key, &banked)
			if err != nil {
				o.log.Debug("library banking failed", "event", "library_put_failed", "error", err.Error())
				return
			}
			o.rememberBank(banked.ID, bankRef{key: key, id: id})
		}()
	}
}

// wordsmithWant is how many lyric sheets the coming batch needs: the
// whole batch, because a deep batch is in no hurry - the writer sits
// on the free card until every planned song has its own words. The
// opener wants a single sheet so the first song is never kept waiting,
// and a tap that is waiting wants none.
func (o *Orchestrator) wordsmithWant(epoch int) int {
	return o.plannable(epoch)
}

// wordsmithPhase writes the coming batch's lyrics - and names their
// songs - while the engine is still hibernated and the helper has the
// whole graphics card. This is the point of phased generation: every
// model gets the card in turn, none of them fights another for it. The
// engine wakes only after the words are on the shelf, so the plan step
// consumes them instead of falling back to mid-cycle CPU calls. A
// buffer already near starvation skips the phase: audio first.
func (o *Orchestrator) wordsmithPhase(ctx context.Context) {
	if o.engineBusy.Load() {
		// A stay-warm cycle never gave the card back; writing lyrics
		// now would fight the engine for it. The next hibernated
		// wake-up gets the phase.
		return
	}
	epoch, sess := o.snapshotSession()
	if sess == nil {
		return
	}
	// An instrumental session comes through here too. It has no words
	// to write, but it has the same need and the same one chance at the
	// card: something of its own to describe each song of the batch,
	// rather than the one terse steering caption under every track. The
	// branch further down has always been written for it; this guard
	// used to turn it away at the door, so it had never once run.
	want := o.wordsmithWant(epoch)
	if want <= 0 {
		return
	}
	// The phase ends when a steer makes its context stale or an export
	// claims the card. Nothing else hurries it: no player draws straight
	// from the generator, so a deep batch's words are written in full
	// before the engine wakes.
	stop := func(wrote int) bool {
		o.mu.Lock()
		steered := o.epoch != epoch
		o.wordsmithWroteNow = wrote // progress for the status display
		o.mu.Unlock()
		if steered {
			return true
		}
		if o.exportingNow() {
			// An export claims the engine and the card; the writer
			// yields immediately and resumes on the next hibernation.
			return true
		}
		return false
	}
	phaseCtx, cancel := context.WithTimeout(ctx, wordsmithBudget)
	defer cancel()
	o.mu.Lock()
	o.wordsmithWantNow, o.wordsmithWroteNow = want, 0
	o.mu.Unlock()
	defer func() {
		o.mu.Lock()
		o.wordsmithWantNow, o.wordsmithWroteNow = 0, 0
		o.mu.Unlock()
	}()
	start := time.Now()
	// An instrumental session has no words to write, but it has the
	// same need: something of its own per song, rather than the same
	// terse tag list under every track of the batch.
	if !sess.Vocal {
		if wrote := o.builder.StockInstrumentalCaptions(phaseCtx, sess, want, stop); wrote > 0 {
			o.log.Info("wordsmith phase described the batch's tracks",
				"event", "wordsmith_done", "descriptions", wrote, "want", want,
				"elapsed_seconds", time.Since(start).Seconds())
		}
		return
	}
	if wrote := o.builder.StockLyrics(phaseCtx, sess, want, stop); wrote > 0 {
		o.log.Info("wordsmith phase wrote the batch's lyrics",
			"event", "wordsmith_done", "sheets", wrote, "want", want,
			"elapsed_seconds", time.Since(start).Seconds())
	}
}

// wordsmithBudget is a pure backstop against a wedged helper; the real
// governor of a wordsmith round is the rendered buffer running low
// (see wordsmithPhase's stop rule). Writing a deep batch's words on
// the card takes tens of minutes, and the buffer holds hours.
const wordsmithBudget = 45 * time.Minute

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
	o.engineBusy.Store(active)
	o.builder.SetEngineBusy(active)
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
