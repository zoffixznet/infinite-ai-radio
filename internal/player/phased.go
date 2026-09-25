package player

import (
	"context"
	"fmt"
	"math"
	"time"

	"iar/internal/audio"
	"iar/internal/config"
	"iar/internal/engine"
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
// yet - is below the wake mark, and a rung is due.
//
// Filling runs in two stages. While the ladder climbs - after a steer
// or a wipe - batch sizes go 1, 10, 20, 40, 80 on the clock: one
// opener as fast as possible, ten songs straight after it, and then
// each rung waits as long as its music runs before the next, less
// whatever listeners skipped, so an evening of prompt-fiddling never
// wastes an hour of planned songs. Every rung is made whole; a batch
// is never trimmed to the room left in the tub. Once the top rung has
// been made no clock runs at all: the engine sleeps until the untaken
// songs fall below the wake mark - which only listeners taking songs
// can bring about - then makes a whole top batch and sleeps again. A
// phone paused on a full bank takes nothing, so the engine sleeps
// indefinitely; two phones filling their banks at once drain the tub
// below the mark within minutes and wake it at once.
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
// opener rung is one song; the top rung refills the tub in whole
// batches of eighty whenever the level falls below the wake mark.
var ladder = [...]int{1, 10, 20, 40, 80}

// topRung is the ladder's last rung: the batch the tub is refilled in
// for as long as the steering context stands.
const topRung = len(ladder) - 1

// fallbackSongSeconds stands in for the mean song length while the tub
// holds no untaken song to measure: a typical song is about three and
// a half minutes.
const fallbackSongSeconds = 210.0

// wakeMark is the low-water mark in songs: the engine is woken only
// while fewer untaken songs than this are in the tub. It is the
// reserve - what a fresh phone takes to fill its bank, kept so that
// filling never wakes the engine - plus the low-water minutes of music
// on top, converted to songs at the mean length of what is in the tub
// (fallbackSongSeconds when it is empty). The mark is in songs
// because the reserve is: a phone's bank is counted in songs, and the
// minutes are what must still be playable once it has taken them.
func wakeMark(b config.Buffer, level int, levelSeconds float64) int {
	mean := fallbackSongSeconds
	if level > 0 && levelSeconds > 0 {
		mean = levelSeconds / float64(level)
	}
	return b.ReserveSongs + int(math.Ceil(float64(b.LowMinutes)*60/mean))
}

// storeCeiling is the most untaken songs the tub comes to hold: a whole
// top batch made just under the wake mark.
func storeCeiling(wakeBelow int) int {
	return wakeBelow - 1 + ladder[topRung]
}

// phasedEngine is what the generator needs from the engine: a plan
// step and a render step, so the two models never share the card.
type phasedEngine interface {
	Ready() bool
	Plan(ctx context.Context, spec engine.Spec) (*engine.Plan, error)
	Render(ctx context.Context, plan *engine.Plan) (*engine.Track, error)
}

// fusedEngine makes a one-shot engine look phased: its plan is the
// spec as given and its render is the one job that does everything.
// Every song still lands in the store the same way.
type fusedEngine struct{ engine.Engine }

func (f fusedEngine) Plan(ctx context.Context, spec engine.Spec) (*engine.Plan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	caption := spec.Prompt
	if caption == "" {
		caption = spec.SampleQuery
	}
	return &engine.Plan{Spec: spec, Caption: caption, Lyrics: spec.Lyrics, Seconds: float64(spec.Seconds)}, nil
}

func (f fusedEngine) Render(ctx context.Context, plan *engine.Plan) (*engine.Track, error) {
	return f.Generate(ctx, plan.Spec)
}

// phasedEng is the engine as the generator drives it.
func (o *Orchestrator) phasedEng() phasedEngine {
	if pe, ok := o.eng.(phasedEngine); ok {
		return pe
	}
	return fusedEngine{o.eng}
}

// adoptDiskBuffer continues from the buffer a previous run left
// behind: when the stored context matches the session's, the highest
// epoch on disk becomes this run's, stray older epochs are dropped,
// and the ladder picks up at the rung the store records. A restart is
// not a steer and not a wipe: the store it comes back to was filled by
// a ladder that may well have reached the top, and climbing it again
// - an opener, ten, a clock, twenty, a clock, forty, a clock - would
// be several engine wakes for handfuls of songs over a store that is
// already deep, which is exactly what the ladder exists to avoid.
// Run before the loops start, so the first sync sees a matching world.
func (o *Orchestrator) adoptDiskBuffer() {
	_, sess := o.snapshotSession()
	if sess == nil || o.Buffer.Context() != sess.ContextKey() {
		return
	}
	de, ok := o.Buffer.DiskEpoch()
	if !ok {
		return
	}
	rung := o.Buffer.Rung()
	if rung > topRung {
		rung = topRung
	}
	o.mu.Lock()
	o.epoch = de
	o.phasedEpoch = de
	o.rung = rung
	o.mu.Unlock()
	if dropped := o.Buffer.DropOtherEpochs(de); dropped > 0 {
		o.log.Info("older epochs cleared while adopting the buffer",
			"event", "buffer_adopt_cleared", "files", dropped)
	}
	tracks, secs := o.Buffer.TrackStats(de)
	o.log.Info("buffer adopted from the previous run", "event", "buffer_adopted",
		"epoch", de, "songs", tracks, "seconds", secs, "rung", ladder[rung])
}

// syncPhasedState aligns the on-disk buffer with what the radio is
// actually playing. Two identities are checked: the steering-context
// key (which survives restarts - a buffer written for another session
// or steering context is wiped wholesale) and the in-run epoch (a
// steer drops the old epoch's files and restarts the ramp).
func (o *Orchestrator) syncPhasedState() int {
	_, sess := o.snapshotSession()
	key := sess.ContextKey()
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
	if changed {
		// The store's record of the rung goes back to the first with
		// it, so a restart in the middle of the new context's climb does
		// not resume the old one's.
		o.Buffer.SetRung(0)
	}
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
	o.bufLevelSeconds = secs
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

// batchFor is how many songs the next cycle plans: the rung's size,
// less what this rung has made already and less the plans an
// interrupted cycle left waiting for their audio, which the cycle
// renders as well. A rung is made whole - never trimmed to the room
// left in the tub - because the point of the ladder is one large batch
// and then hours with the card left alone, not a top-up of a handful
// of songs after every take; and never more than whole, which is what
// planning the rung's remainder on top of the plans already stored
// would come to.
func (o *Orchestrator) batchFor(epoch int) int {
	planned, _ := o.Buffer.PlanStats(epoch)
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.batchForLocked(planned)
}

// batchForLocked is batchFor under the lock, given the plans already
// stored for the epoch.
func (o *Orchestrator) batchForLocked(planned int) int {
	size := ladder[o.rung] - o.rungMade - planned
	if size < 0 {
		size = 0
	}
	return size
}

// stocked reports whether the tub holds the wake mark or more: nothing
// is made until listeners take songs.
func (o *Orchestrator) stocked(epoch int) bool {
	level, secs := o.Buffer.Level(epoch)
	return level >= wakeMark(o.cfg.Buffer, level, secs)
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

// plannable is how many songs a cycle starting now may plan: nothing
// while the tub is stocked to the wake mark or while the ladder's
// clock is being waited out; otherwise the rung's remaining size,
// whole. A cycle reads it once, at its start, and finishes the batch
// it set out to make: the level crossing the mark mid-batch does not
// cut a rung short.
func (o *Orchestrator) plannable(epoch int) int {
	if o.stocked(epoch) {
		return 0
	}
	if o.rungWaitLeft() > 0 {
		return 0
	}
	return o.batchFor(epoch)
}

// completeRung closes the rung once its batch is made and the ladder
// climbs. While climbing, the tap then waits as long as the batch's
// music runs before the next rung; the opener's rung waits nothing,
// so the ten-song batch follows it straight away. The top rung sets
// no wait at all: from there the engine wakes on the level alone,
// however long ago the batch was made, so a tub drained below the
// mark a minute after a batch wakes it at once and a tub nobody draws
// from leaves it asleep for good. The next wake makes a whole top
// batch - so the top rung also closes when a cycle ends short of it
// with the tub stocked and no plan left waiting for its audio: the
// writer running out of words at 25 of 80 has still put the tub over
// the mark, and the rest of that rung is not owed to the next wake,
// which comes hours later and makes a whole 80. A rung cut short
// while the tub is under the mark stays open, and the cycle that
// re-enters within the minute makes the rest of it.
func (o *Orchestrator) completeRung(epoch int) {
	level, secs := o.Buffer.Level(epoch)
	planned, _ := o.Buffer.PlanStats(epoch)
	below := wakeMark(o.cfg.Buffer, level, secs)
	o.mu.Lock()
	if o.rungMade == 0 {
		o.mu.Unlock()
		return
	}
	whole := o.rungMade >= ladder[o.rung]
	shortButStocked := o.rung == topRung && level >= below && planned == 0
	if !whole && !shortButStocked {
		o.mu.Unlock()
		return // the rung is not made yet; the next cycle carries on
	}
	o.rungWait = time.Duration(o.rungSeconds * float64(time.Second))
	o.rungDoneAt = time.Now()
	if o.rung == 0 || o.rung == topRung {
		o.rungWait = 0
		o.rungDoneAt = time.Time{}
	}
	o.skipCredit = 0
	o.log.Info("ladder rung made", "event", "ladder_rung", "rung", ladder[o.rung],
		"songs", o.rungMade, "wait_seconds", o.rungWait.Seconds(), "level", level,
		"wake_below", below)
	if o.rung < topRung {
		o.rung++
	}
	o.rungMade = 0
	o.rungSeconds = 0
	rung := o.rung
	o.mu.Unlock()
	// Recorded in the store, so a restart carries on from this rung.
	o.Buffer.SetRung(rung)
}

// ReportSkipped credits the ladder with music a listener skipped:
// skipping is faster consumption, so while the ladder climbs the next
// rung comes sooner. Listeners report it on every check for new songs;
// the credits add up across them. Past the top rung there is no clock
// to shorten - the skip's take is what brings the level down - and
// the credit is simply kept until the next rung closes.
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
// that, a rung is due only while the tub is below the wake mark and
// the last rung's wait, if the ladder is still climbing, has run out.
func (o *Orchestrator) wantCycle(epoch int) bool {
	if o.eng == nil {
		return false // nothing can be made
	}
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
// puts the engine down again.
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
			// its way back - the store stocked, the mode changed, a
			// cooldown started - left the daemon holding the card with
			// nothing left to enter that would put it down. This
			// goroutine is the only one that runs a cycle, so nothing
			// can be mid-plan or mid-render here; an export is the one
			// thing that legitimately owns a warm engine while the
			// radio wants no cycle of its own.
			if o.engineBusy.Load() && !o.exportingNow() {
				o.hibernateEngine(o.sleepReasonNow(epoch))
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
	pe := o.phasedEng()
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
		if ctx.Err() != nil {
			o.hibernateEngine(sleepQuit)
			return
		}
		epoch := o.syncPhasedState()
		if lyricStarved && o.plannable(epoch) > 0 {
			// More plans are due, but they are waiting on words: the
			// wordsmith needs the card, so the render daemon is stopped
			// even though the cycle is not finished. The radio is still
			// making songs; the loop re-enters within seconds. Only
			// while a writer round can actually follow, though: the
			// short batch may have stocked the tub, and then nothing
			// is written and the engine is simply asleep, which is
			// what the line below says.
			o.hibernateEngine(sleepForWriter)
			return
		}
		if o.wantCycle(epoch) {
			return // more work due (ramp climbing, or a fresh epoch): stay warm
		}
		o.hibernateEngine(o.sleepReasonNow(epoch))
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
		// good: in the store, in the play queue, and in the book once it
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
				// the render daemon is stopped, and the next round
				// starts with the wordsmith owning the card.
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
			return // all targets met; the defer puts the engine down
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
		epoch, _ := o.snapshotSession()
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
// songs - while the render daemon is down and the helper has the whole
// graphics card. This is the point of phased generation: every
// model gets the card in turn, none of them fights another for it. The
// engine wakes only after the words are on the shelf, so the plan step
// consumes them instead of falling back to mid-cycle CPU calls. A
// buffer already near starvation skips the phase: audio first.
func (o *Orchestrator) wordsmithPhase(ctx context.Context) {
	if o.engineBusy.Load() {
		// A stay-warm cycle never gave the card back; writing lyrics
		// now would fight the engine for it. The next wake-up from a
		// stopped daemon gets the phase.
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
			// yields immediately and resumes the next time the daemon
			// is down.
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
// (waking it involves a cold start when the daemon was stopped).
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

// sleepReason is why the engine daemon is being put down. To the
// listener there is one engine and it makes songs - writing the words
// is the engine working as much as rendering is - so the daemon being
// stopped for the writer's turn on the card is not the engine going to
// sleep, and the log says which it was.
type sleepReason int

const (
	// sleepForWriter: the render daemon is stopped only so the writer
	// can have the card. The radio is still making songs.
	sleepForWriter sleepReason = iota
	// sleepStocked: the store holds the wake mark or more; nothing is
	// made until the listeners have taken enough songs to bring it
	// under. The log line says how much is in store and the mark.
	sleepStocked
	// sleepUntilDue: the ladder's clock is being waited out.
	sleepUntilDue
	// sleepCooldown: a batch is due, but the last cycle gave up on
	// failures and the engine is rested before it is asked again.
	sleepCooldown
	// sleepNoWork: no cycle is wanted for another reason (an engine
	// that made everything it could).
	sleepNoWork
	// sleepQuit: the radio is shutting down.
	sleepQuit
)

// sleepLog is the log event and message for each reason.
func sleepLog(why sleepReason) (event, msg string) {
	switch why {
	case sleepForWriter:
		return "render_unloaded", "render model unloaded while the words are written"
	case sleepStocked:
		return "engine_hibernated", "engine asleep: the store is stocked"
	case sleepUntilDue:
		return "engine_hibernated", "engine asleep until the next batch is due"
	case sleepCooldown:
		return "engine_hibernated", "engine asleep: resting after failures before the next try"
	case sleepQuit:
		return "engine_hibernated", "engine stopped with the radio"
	}
	return "engine_hibernated", "engine asleep: no batch is due"
}

// sleepReasonNow decides, on the paths where the engine sleeps because
// no cycle is wanted, between a failure cooldown, a stocked store, the
// clock, and anything else. The cooldown comes first: it is what stops
// a cycle whose batch is otherwise due, and a line saying no batch is
// due while one waits on the cooldown would be untrue.
func (o *Orchestrator) sleepReasonNow(epoch int) sleepReason {
	stocked := o.stocked(epoch)
	o.mu.Lock()
	cooling := time.Now().Before(o.cycleCooldown)
	waiting := o.rungWaitLeftLocked() > 0
	o.mu.Unlock()
	switch {
	case cooling:
		return sleepCooldown
	case stocked:
		return sleepStocked
	case waiting:
		return sleepUntilDue
	}
	return sleepNoWork
}

// stockedMessage is the log line for an engine asleep on a stocked
// store: what is in store, how much music that is, and the mark the
// store must fall below before the engine is woken.
func stockedMessage(level int, seconds float64, wakeBelow int) string {
	return fmt.Sprintf("engine asleep: %d songs in store, %s of music; wakes below %d",
		level, spanText(seconds), wakeBelow)
}

// spanText writes a stretch of music the way a listener thinks about
// it: hours and minutes for a deep store, minutes or seconds for a
// shallow one.
func spanText(seconds float64) string {
	d := time.Duration(seconds * float64(time.Second))
	switch {
	case d >= time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d/time.Hour), int(d/time.Minute)%60)
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d > 0:
		return fmt.Sprintf("%ds", int(d/time.Second))
	}
	return "0m"
}

// hibernateEngine stops heartbeating and shuts the engine daemon down,
// giving all of its graphics and system memory back until the next
// cycle. Playback continues from the disk buffer. why says whether
// this is the engine going to sleep or only the writer's turn on the
// card, which is what the log line reports; an engine asleep on a
// stocked store is logged with what is in store and when it wakes.
func (o *Orchestrator) hibernateEngine(why sleepReason) {
	o.setEngineActive(false)
	if h, ok := o.eng.(interface{ HibernateEngine() bool }); ok && h.HibernateEngine() {
		event, msg := sleepLog(why)
		if why == sleepStocked && o.Buffer != nil {
			o.mu.Lock()
			epoch := o.epoch
			o.mu.Unlock()
			level, secs := o.Buffer.Level(epoch)
			below := wakeMark(o.cfg.Buffer, level, secs)
			o.log.Info(stockedMessage(level, secs, below), "event", event,
				"level", level, "seconds", secs, "wake_below", below)
			return
		}
		o.log.Info(msg, "event", event)
	}
}
