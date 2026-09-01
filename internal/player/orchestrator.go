package player

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"iar/internal/audio"
	"iar/internal/config"
	"iar/internal/engine"
	"iar/internal/library"
	"iar/internal/prompting"
	"iar/internal/session"
	"iar/internal/state"
	"iar/internal/telemetry"
	"iar/internal/trackbuffer"
)

// Event is a transient user-facing message from the stream machinery.
type Event struct {
	// Text is ready to display.
	Text string
}

// LanguageState is one configured vocal language and whether the
// playing session sings in it.
type LanguageState struct {
	// Name is the language as configured ("Bisaya (Cebuano)").
	Name string
	// Engine reports that the music engine has a tag for the language.
	// The rest are still sung: the words are written in them and the
	// engine sings them without a language hint.
	Engine bool
	// On reports that the session currently sings in it.
	On bool
}

// Status is a point-in-time snapshot for status displays.
type Status struct {
	// State summarizes what is audible: starting, playing, noise,
	// looping, bed or paused.
	State string
	// Source describes the current audio source.
	Source string
	// Elapsed and Duration describe progress through the current track
	// (zero for endless sources).
	Elapsed  time.Duration
	Duration time.Duration
	// Queued is the number of generated tracks waiting to play.
	Queued int
	// Generating reports whether a generation is in flight.
	Generating bool
	// EngineName and EngineReady describe the music engine.
	EngineName  string
	EngineReady bool
	// EngineStarting is true while the engine is booting up.
	EngineStarting bool
	// Session is the active session name; SessionDesc summarizes its
	// steering context.
	Session     string
	SessionDesc string
	// Volume is the output volume in percent.
	Volume int
	// Paused reports whether output is paused.
	Paused bool
	// Underruns counts output buffer underruns since start.
	Underruns int64
	// GenCount and LastGenTime describe generation throughput.
	GenCount    int
	LastGenTime time.Duration
	// Exporting describes a running export, empty otherwise.
	Exporting string
	// BufferTarget is how many tracks the generate-ahead worker aims to
	// keep queued. It describes the fused path only; phased generation
	// buffers to disk in minutes of audio, reported below.
	BufferTarget int
	// Phased reports whether the disk-backed pipeline is running. When
	// it is, Queued counts only the small in-memory prefetch and the
	// fields below are the buffer worth showing.
	Phased bool
	// BufferedTracks and BufferedSeconds are the songs already rendered
	// and waiting to play (on disk, plus the in-memory prefetch).
	BufferedTracks  int
	BufferedSeconds float64
	// PlannedTracks and PlannedSeconds are songs the planner has
	// written but the renderer has not turned into audio yet.
	PlannedTracks  int
	PlannedSeconds float64
	// BufferTargetSeconds is the rendered-audio depth the cycle aims
	// for, and BufferLowSeconds the depth that triggers a refill.
	BufferTargetSeconds float64
	BufferLowSeconds    float64
	// Telemetry is a recent machine-resource sample, present only when
	// the radio was started with resource telemetry enabled.
	Telemetry *telemetry.Sample
	// FailStreak counts consecutive generation failures; LastFailure is
	// the most recent failure reason ("" when the last generation
	// succeeded).
	FailStreak  int
	LastFailure string
	// Epoch increments on every steering-context change; remote clients
	// use it to invalidate queued and prefetched tracks.
	Epoch int
	// BasePrompt and Tweaks are the session's steering context;
	// Vocal reports whether it asks for sung vocals.
	BasePrompt string
	Tweaks     []session.Entry
	Vocal      bool
	// LyricsGenerator is the effective lyric writer for vocal tracks.
	LyricsGenerator string
	// Languages lists the configured vocal languages and whether this
	// session sings in each. Empty means the music engine picks the
	// language on its own.
	Languages []LanguageState
	// TrackID and TrackPrompt identify the playing generated track
	// (empty while a stopgap source plays); TrackSaved reports whether
	// it is already saved as a snippet. TrackTitle and TrackSubtitle
	// are its short display names; TrackNum is its per-process play
	// number.
	TrackID       string
	TrackPrompt   string
	TrackTitle    string
	TrackSubtitle string
	TrackNum      int
	TrackSaved    bool
	// PrevTrackID, PrevTrackPrompt and PrevTrackSaved describe the
	// track played before the current one.
	PrevTrackID     string
	PrevTrackPrompt string
	PrevTrackTitle  string
	PrevTrackSaved  bool
	// SavedTrackIDs lists the track ids saved as snippets this run
	// (bounded), so remote clients can grey their own save buttons.
	SavedTrackIDs []string
	// Phase names the current startup phase ("starting engine",
	// "loading models", "generating first track") or "playing".
	Phase string
	// PhaseElapsed is how long the current phase has been running;
	// PhaseExpected is its usual duration from past measurements.
	PhaseElapsed  time.Duration
	PhaseExpected time.Duration
	// PhaseSlow reports the phase has exceeded ~2.5x its usual duration.
	PhaseSlow bool
	// Switching reports a context change waiting for its first fresh
	// track: the queue was dropped and the mixer crosses over as soon
	// as one is generated.
	Switching bool
	// Looping reports the mixer is replaying the last good track for
	// want of anything newer.
	Looping bool
	// TrackLanguage is the language the playing track was sung in, in
	// the listener's own wording; empty when the engine chose.
	TrackLanguage string
	// TrackLyrics is what the playing track is actually singing: the
	// sheet the lyric writer produced, or the one the engine invented
	// for itself. Empty for instrumentals and stopgap audio.
	TrackLyrics string
}

// Orchestrator owns the stream: session state, generation queue, mixing and
// playback. All exported methods are safe for concurrent use.
type Orchestrator struct {
	cfg     config.Config
	eng     engine.Engine // nil when running without a music engine
	builder *prompting.Builder
	store   *session.Store
	log     *slog.Logger
	player  audio.Player
	ring    *audio.Ring

	// Timings records how long startup phases take across runs; set it
	// before Start. Nil disables persistence (estimates use defaults).
	Timings *state.Timings
	// Library is the on-disk track cache used for instant starts and
	// opportunistic banking; nil disables it. Set before Start.
	Library *library.Library
	// Buffer is the on-disk buffer of phased generation (plans awaiting
	// render, rendered songs awaiting play). Set before Start; nil (or
	// buffer.phased=false in the config) selects the fused path.
	Buffer *trackbuffer.Store
	// SnippetsDir is where the save command writes captured tracks.
	SnippetsDir string
	// Tap, when set before Start, receives the mastered stream at the
	// single pump chokepoint: every PCM chunk the mixer produces, at
	// full level. It sits before pause and volume, which control the
	// speakers in the room rather than the station. Its Write must
	// never block.
	Tap interface{ Write(p []byte) (int, error) }
	// Retention is how long auto-named sessions are kept after they last
	// played before the periodic sweep removes them; zero disables the
	// sweep. Set before Start.
	Retention time.Duration
	// StateDir, when set, records which session is playing so other
	// processes (the CLI's delete) can refuse to remove it.
	StateDir *state.Dir
	// Telemetry, when set before Start, samples system and graphics
	// memory and the engine's model residency for the status display.
	// Nil leaves those fields of Status empty.
	Telemetry *telemetry.Sampler

	mu       sync.Mutex
	sess     *session.Session
	queue    []*engine.Track
	epoch    int
	lastGood *engine.Track
	// lastGoodEpoch is the steering epoch lastGood was generated in.
	// When it is behind, looping lastGood replays the sound the
	// listener has just moved away from, which is worth saying out loud
	// and worth refusing to skip into.
	lastGoodEpoch int
	// loopNoticeEpoch remembers which epoch already announced the loop
	// fallback, so a burst of skips does not flush the event channel.
	loopNoticeEpoch int
	// seededLib holds the library ids already queued as an instant
	// start, so the same audio is not also listed as filler.
	seededLib map[string]bool
	cur       source
	// switchReq forces the mixer to the next source; steerPending asks
	// for the same switch, but only once a post-steer track is queued.
	switchReq    bool
	steerPending bool
	paused       bool
	genBusy      bool
	genCount     int
	// Phased-generation state: the epoch the buffer currently belongs
	// to, the last handed-out file sequence number, and how many tracks
	// of this epoch have been fed to playback (drives the batch ramp).
	phasedEpoch   int
	phasedSeq     int
	playedInEpoch int
	// phasedSynced flags that the buffer was reconciled with this
	// run's identity at least once; cycleCooldown blocks new cycles
	// after persistent failures; renderFails counts render failures
	// per plan sequence so a poisoned plan gets dropped.
	phasedSynced  bool
	cycleCooldown time.Time
	renderFails   map[int]int
	// Cached on-disk buffer depth, refreshed by the phased loops. The
	// status display repaints four times a second; counting the buffer
	// directory that often is not worth the disk.
	bufTracks      int
	bufSeconds     float64
	bufPlans       int
	bufPlanSeconds float64
	lastGen        time.Duration
	exporting      string
	phase          string
	phaseStart     time.Time
	started        time.Time
	firstMusic     bool
	failStreak     int
	lastFailure    string
	curTrack       *engine.Track
	prevTrack      *engine.Track
	// bankRefs remembers where a still-provisional track's banked
	// library copy lives (track ID -> key and library id), so a late
	// name reaches the banked sidecar too. Pruned as tracks retire.
	bankRefs map[string]bankRef
	saving   bool
	// playCount numbers the tracks as they start playing (per process);
	// curTrackNum is the playing track's number.
	playCount   int
	curTrackNum int
	// saved remembers which track ids were saved as snippets this run
	// (bounded by savedOrder), so save buttons can grey out and a
	// repeat save is a no-op.
	saved      map[string]bool
	savedOrder []string
	// saveLanguages persists an edited vocal-language catalogue and
	// which of its languages are switched off. Nil means both only live
	// for this run.
	saveLanguages func(names, off []string) error

	volume atomic.Int32
	events chan Event
	wake   chan struct{}

	genMu  sync.Mutex // serializes engine use between playback and export
	runCtx context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New assembles an orchestrator. eng may be nil to run without a music
// engine (noise only, with clear messaging).
func New(cfg config.Config, eng engine.Engine, builder *prompting.Builder, store *session.Store, sess *session.Session, pl audio.Player, log *slog.Logger) *Orchestrator {
	o := &Orchestrator{
		cfg:      cfg,
		eng:      eng,
		builder:  builder,
		store:    store,
		log:      log,
		player:   pl,
		ring:     audio.NewRing(2 * audio.BytesPerSecond),
		sess:     sess,
		events:   make(chan Event, 16),
		wake:     make(chan struct{}, 1),
		bankRefs: map[string]bankRef{},
	}
	o.volume.Store(int32(cfg.Volume))
	return o
}

// Events returns the stream of transient user-facing messages.
func (o *Orchestrator) Events() <-chan Event { return o.events }

// Start launches the stream. It returns immediately; audio begins with the
// session's noise bed and crossfades into generated music when ready.
func (o *Orchestrator) Start(ctx context.Context) {
	ctx, o.cancel = context.WithCancel(ctx)
	o.runCtx = ctx
	now := time.Now()
	o.mu.Lock()
	o.started = now
	o.phaseStart = now
	o.mu.Unlock()
	o.seedFromLibrary()
	// Prompt-seeded sessions carry a raw user description; let the
	// helper enrich it in the background when it is available.
	o.mu.Lock()
	initial := o.sess
	fromPrompt := strings.HasPrefix(initial.Name, "prompt-") && len(initial.Tweaks) == 0
	o.mu.Unlock()
	if fromPrompt {
		o.expandSeedAsync(initial)
	}
	// Prime the ring with a little silence: the output pump's first reads
	// fill the system player's buffer in a burst, and without priming
	// that burst can outrun the mixer's first write and be counted (and
	// heard) as an underrun right at launch.
	o.ring.Write(make([]byte, audio.DurationToBytes(300*time.Millisecond)))
	o.recordCurrent()
	if o.Telemetry != nil {
		o.Telemetry.Start(ctx)
	}
	o.wg.Add(4)
	if o.phasedEnabled() {
		// Phased generation: a producer cycle (plan batch, render
		// batch, hibernate) and a feeder that decodes rendered songs
		// from disk into the playback prefetch.
		o.wg.Add(1)
		go func() { defer o.wg.Done(); o.cycleLoop(ctx) }()
		go func() { defer o.wg.Done(); o.feedLoop(ctx) }()
	} else {
		go func() { defer o.wg.Done(); o.genLoop(ctx) }()
	}
	o.wg.Add(1)
	go func() { defer o.wg.Done(); o.retitleLoop(ctx) }()
	go func() { defer o.wg.Done(); o.mixLoop(ctx) }()
	go func() { defer o.wg.Done(); o.pumpLoop(ctx) }()
	go func() { defer o.wg.Done(); o.phaseLoop(ctx) }()
	if o.Retention > 0 {
		o.wg.Add(1)
		go func() { defer o.wg.Done(); o.sweepLoop(ctx) }()
	}
}

// expandSeedAsync asks the helper model to enrich a prompt-seeded
// session's vague description into structured fields, in the
// background. Curated preset prompts are already tag-rich and skipped.
func (o *Orchestrator) expandSeedAsync(sess *session.Session) {
	if sess.Preset != "" || sess.Mode != session.ModeMusic {
		return
	}
	o.mu.Lock()
	epochAt := o.epoch
	snap := sess.Snapshot()
	o.mu.Unlock()
	o.builder.ExpandAsync(snap, func(u prompting.SpecUpdate) {
		o.mu.Lock()
		changed := false
		if o.sess == sess && o.epoch == epochAt && len(o.sess.Tweaks) == 0 {
			changed = prompting.MergeUpdate(o.sess, u)
		}
		o.mu.Unlock()
		if changed {
			o.saveSession()
			o.log.Info("seed prompt expanded by the helper model", "event", "seed_expanded", "prompt", snap.BasePrompt)
		}
	})
}

// sweepInterval paces the periodic session sweep (a variable so tests
// can shrink it).
var sweepInterval = 30 * time.Minute

// sweepLoop removes stale auto-named sessions at startup and
// periodically, refreshing the playing session's last-played stamp so
// it is never judged stale after a restart.
func (o *Orchestrator) sweepLoop(ctx context.Context) {
	for {
		o.saveSession()
		removed, err := o.store.Sweep(time.Now(), o.Retention, o.CurrentName())
		switch {
		case err != nil:
			o.log.Warn("session sweep failed", "event", "sessions_sweep_failed", "error", err.Error())
		case len(removed) > 0:
			o.log.Info("stale auto-named sessions removed", "event", "sessions_swept",
				"removed", strings.Join(removed, ","), "count", len(removed), "retention", o.Retention.String())
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(sweepInterval):
		}
	}
}

// recordCurrent notes the playing session in the state directory.
func (o *Orchestrator) recordCurrent() {
	if o.StateDir == nil {
		return
	}
	if err := o.StateDir.WriteCurrentSession(state.CurrentSession{
		Name: o.CurrentName(), PID: os.Getpid(), Since: time.Now(),
	}); err != nil {
		o.log.Warn("could not record the playing session", "event", "session_state_failed", "error", err.Error())
	}
}

// CurrentName returns the playing session's name.
func (o *Orchestrator) CurrentName() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.sess.Name
}

// Close stops all goroutines, saves the session and releases the player.
func (o *Orchestrator) Close() error {
	if o.cancel != nil {
		o.cancel()
	}
	o.ring.Close()
	o.wg.Wait()
	o.saveSession()
	return o.player.Close()
}

// emit delivers an event without ever blocking (oldest are dropped).
func (o *Orchestrator) emit(text string) {
	select {
	case o.events <- Event{Text: text}:
	default:
		select {
		case <-o.events:
		default:
		}
		select {
		case o.events <- Event{Text: text}:
		default:
		}
	}
}

// lastGoodStaleLocked reports that the loop fallback would replay audio
// from before the last context change. Callers hold o.mu.
func (o *Orchestrator) lastGoodStaleLocked() bool {
	return o.lastGood != nil && o.lastGoodEpoch != o.epoch
}

func (o *Orchestrator) kickGen() {
	select {
	case o.wake <- struct{}{}:
	default:
	}
}

// saveSession persists a snapshot of the current session, logging
// failures. The session is the one playing, so its last-played stamp is
// refreshed on the way.
func (o *Orchestrator) saveSession() {
	o.mu.Lock()
	o.sess.LastPlayed = time.Now()
	o.mu.Unlock()
	_, cp := o.snapshotSession()
	if err := o.store.Save(cp); err != nil {
		o.log.Error("session save failed", "event", "session_save_failed", "error", err.Error())
	}
}

// snapshotSession returns the epoch and a copy of the session safe to read
// without the lock. The session's own Snapshot is the single copier:
// a hand-rolled one here drifted out of date and left the language map
// aliased, which the generator reads while the controls write it.
func (o *Orchestrator) snapshotSession() (int, *session.Session) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.epoch, o.sess.Snapshot()
}

// genLoop keeps the queue filled while in music mode.
func (o *Orchestrator) genLoop(ctx context.Context) {
	failures := 0
	oomStreak := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-o.wake:
		case <-time.After(time.Second):
		}
		if !o.wantGeneration() {
			continue
		}
		epoch, sess := o.snapshotSession()
		seconds := o.cfg.TrackSeconds
		o.mu.Lock()
		firstTrack := o.genCount == 0 && o.lastGood == nil
		o.mu.Unlock()
		if firstTrack && seconds > 60 {
			// A shorter first track gets music playing sooner; later
			// tracks use the configured length.
			seconds = 60
		}
		spec := o.builder.BuildSpec(ctx, sess, seconds)
		// Ask the helper for an evocative short name while the track
		// generates; generation takes far longer, so the name is
		// usually ready when the track lands.
		o.builder.TitleAsync(specPromptForLog(spec))
		o.mu.Lock()
		o.genBusy = true
		o.mu.Unlock()
		o.log.Info("generation started", "event", "generation_started",
			"prompt", specPromptForLog(spec), "lyric_mode", lyricMode(spec),
			"vocal", spec.Vocal(), "seconds", spec.Seconds, "epoch", epoch)
		start := time.Now()
		o.genMu.Lock()
		track, err := o.eng.Generate(ctx, spec)
		o.genMu.Unlock()
		elapsed := time.Since(start)
		o.mu.Lock()
		o.genBusy = false
		o.mu.Unlock()
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			reason := err.Error()
			deviceFault := isDeviceFault(reason)
			// A full graphics card is somebody else's memory, not a
			// broken engine. Restarting would reload every model and
			// take exactly the memory the other program is waiting for,
			// so an out-of-memory failure neither counts toward the
			// restart streak nor retries at the usual pace.
			oom := isOutOfMemory(reason)
			if oom {
				oomStreak++
			} else {
				oomStreak = 0
				failures++
			}
			o.mu.Lock()
			o.failStreak = failures
			o.lastFailure = reason
			o.mu.Unlock()
			o.log.Error("generation failed", "event", "generation_failed",
				"error", reason, "failures", failures, "device_fault", deviceFault,
				"out_of_memory", oom)
			if oom {
				o.emit("the graphics card is full right now; waiting for room before generating again")
			}
			// Health checks alone cannot catch a poisoned engine that
			// still answers /health: restart on a failure streak, and
			// immediately on the known-fatal device fault.
			if !oom && (deviceFault || failures >= restartStreak) {
				if rst, ok := o.eng.(interface{ RestartEngine(string) bool }); ok && rst.RestartEngine(reason) {
					why := "repeated generation failures"
					if deviceFault {
						why = "a device-placement fault (it never recovers on its own)"
					}
					o.emit("engine restarting after " + why + "; music keeps playing meanwhile")
					o.log.Warn("engine restart requested", "event", "engine_restart_requested",
						"streak", failures, "device_fault", deviceFault)
					failures = 0
				} else if failures%5 == 0 || deviceFault {
					o.emit("generation keeps failing and the engine cannot be restarted from here (see log and 'iar doctor')")
				}
			} else if !oom && failures == 1 {
				o.emit("generation failed; retrying (details in the log)")
			}
			wait := backoff(failures)
			if oom {
				wait = oomBackoff(oomStreak)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
			continue
		}
		failures = 0
		oomStreak = 0
		o.mu.Lock()
		o.failStreak = 0
		o.lastFailure = ""
		o.mu.Unlock()
		if o.cfg.NormalizeLoudness {
			gain := audio.NormalizeLoudness(track.Samples, audio.DefaultTargetRMS)
			if gain != 1 {
				o.log.Debug("track loudness normalized", "event", "normalized", "gain", gain)
			}
		}
		track.ID = newTrackID()
		o.fillTitle(track, specPromptForLog(spec))
		o.mu.Lock()
		if epoch == o.epoch {
			o.queue = append(o.queue, track)
			o.lastGood = track
			o.lastGoodEpoch = epoch
			o.genCount++
			o.lastGen = elapsed
		}
		kept := epoch == o.epoch
		o.mu.Unlock()
		if kept {
			// Bank the fresh track for future instant starts. Tracked
			// in the WaitGroup so shutdown never races a disk write
			// (adding here is safe: genLoop itself holds the group).
			key := library.Key(sess)
			o.wg.Add(1)
			go func(t *engine.Track) {
				defer o.wg.Done()
				if _, err := o.Library.Put(key, t); err != nil {
					o.log.Debug("library banking failed", "event", "library_put_failed", "error", err.Error())
				}
			}(track)
		}
		o.log.Info("generation finished", "event", "generation_finished",
			"elapsed_seconds", elapsed.Seconds(), "track_seconds", track.Duration().Seconds(),
			"kept", kept, "prompt", track.Prompt)
	}
}

// newTrackID returns a unique id for a track entering the stream.
func newTrackID() string {
	return fmt.Sprintf("t-%d-%04d", time.Now().UnixMilli(), rand.IntN(10000))
}

// fillTitle gives a track its short display name before it enters the
// stream (immutable afterwards): the helper model's name when it
// arrived in time (keyed on the prompt that requested the track),
// otherwise the deterministic fallback, which must look finished on its
// own. requestPrompt may be empty for library tracks.
func (o *Orchestrator) fillTitle(t *engine.Track, requestPrompt string) {
	if t.Title == "" {
		t.Title, t.Subtitle = prompting.TrackTitle(t.Prompt)
	}
	if requestPrompt == "" {
		return
	}
	if title, subtitle, ok := o.builder.TitleFor(requestPrompt); ok {
		t.Title = title
		if subtitle != "" {
			t.Subtitle = subtitle
		}
	}
}

// wantGeneration reports whether the generate-ahead worker should produce
// another track right now.
func (o *Orchestrator) wantGeneration() bool {
	if o.eng == nil || !o.eng.Ready() {
		return false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.sess.Mode == session.ModeMusic && len(o.queue) < o.cfg.BufferTracks
}

// failureBackoffBase scales the retry delay after generation failures
// (a variable so tests can shrink it).
var failureBackoffBase = 5 * time.Second

// backoff returns the retry delay after n consecutive failures.
func backoff(n int) time.Duration {
	d := time.Duration(n) * failureBackoffBase
	if d > 2*time.Minute {
		d = 2 * time.Minute
	}
	return d
}

// restartStreak is how many consecutive generation failures trigger an
// automatic engine restart even while health checks still pass.
const restartStreak = 3

// oomBackoff waits out a full graphics card. The wait grows faster than
// the ordinary one and climbs higher: whatever else is using the card
// needs room, and generating again immediately is what denies it.
func oomBackoff(n int) time.Duration {
	d := time.Duration(n) * 4 * failureBackoffBase
	if d > 5*time.Minute {
		d = 5 * time.Minute
	}
	return d
}

// oomNeedles identify a generation that failed only because the
// graphics card had no room left, in the wording torch and the engine
// use for it.
var oomNeedles = []string{"CUDA out of memory", "OutOfMemoryError", "out of memory"}

// isOutOfMemory reports whether a failure reason is a full graphics
// card rather than a broken engine.
func isOutOfMemory(reason string) bool {
	for _, needle := range oomNeedles {
		if strings.Contains(reason, needle) {
			return true
		}
	}
	return false
}

// deviceFaultNeedle identifies the known-fatal device-placement fault:
// once the engine is in that state it never recovers on its own, so it
// is restarted on the first occurrence.
const deviceFaultNeedle = "Expected all tensors to be on the same device"

// isDeviceFault reports whether a failure reason is the known-fatal
// device-placement fault.
func isDeviceFault(reason string) bool {
	return strings.Contains(reason, deviceFaultNeedle)
}

// pumpLoop moves audio from the ring buffer to the playback backend,
// applying volume and pause. The backend's own pacing provides
// backpressure; the ring's non-blocking reads make silence the floor.
//
// The ring is drained on every pass, pause included. Holding the reads
// back stalls the mixer against a two-second buffer within seconds, and
// a stalled mixer stops the queue draining, which stops generation and
// leaves anyone listening on the phone circling the same few tracks.
// Pause and volume are controls for the speakers in this room, so they
// are applied after the stream has been handed to the tap: a listener
// on the phone is not in that room, and muting a laptop is no reason to
// broadcast dead air to them.
func (o *Orchestrator) pumpLoop(ctx context.Context) {
	const chunkFrames = audio.SampleRate / 10 // 100 ms
	buf := make([]byte, audio.FramesToBytes(chunkFrames))
	silence := make([]byte, len(buf))
	var lastUnderruns int64
	for ctx.Err() == nil {
		o.mu.Lock()
		paused := o.paused
		o.mu.Unlock()
		if _, err := o.ring.Read(buf); err != nil {
			return // ring closed and drained
		}
		if u := o.ring.Underruns(); u != lastUnderruns {
			o.log.Warn("output underrun", "event", "underrun", "total", u)
			lastUnderruns = u
		}
		// BytesToSamples allocates, so buf is never mutated behind the
		// tap's back.
		if o.Tap != nil {
			o.Tap.Write(buf)
		}
		out := buf
		if paused {
			out = silence
		} else if vol := float64(o.volume.Load()) / 100; vol < 1 {
			samples := audio.BytesToSamples(out)
			audio.ApplyGain(samples, vol*vol) // perceptual taper
			out = audio.SamplesToBytes(samples)
		}
		if _, err := o.player.Write(out); err != nil {
			if ctx.Err() != nil {
				return
			}
			o.log.Error("player write failed, switching to silent output", "event", "player_failed", "error", err.Error())
			o.emit("audio output failed; continuing silently (see log)")
			o.player.Close()
			np, nerr := audio.NewPlayer(audio.PlayerOptions{Kind: "null"})
			if nerr != nil {
				return
			}
			o.player = np
		}
	}
}

// specPromptForLog returns the text that actually drives generation.
func specPromptForLog(spec engine.Spec) string {
	if spec.SampleQuery != "" {
		return spec.SampleQuery
	}
	return spec.Prompt
}

// lyricMode names how lyrics are produced for a spec.
func lyricMode(spec engine.Spec) string {
	switch {
	case spec.SampleQuery != "":
		return "engine-planned"
	case spec.Lyrics == engine.InstrumentalLyrics || spec.Lyrics == "":
		return "instrumental"
	default:
		return "custom-lyrics"
	}
}

// phaseKeys maps display phases to timing-store keys.
var phaseKeys = map[string]string{
	"starting engine":        state.PhaseEngineStart,
	"loading models":         state.PhaseModelLoad,
	"generating first track": state.PhaseFirstTrack,
}

// phaseLoop tracks the startup phase for progress displays, records phase
// durations for future estimates, and logs transitions.
func (o *Orchestrator) phaseLoop(ctx context.Context) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		next := o.currentPhase()
		o.mu.Lock()
		prev := o.phase
		if prev == next {
			o.mu.Unlock()
			continue
		}
		elapsed := time.Since(o.phaseStart)
		o.phase = next
		o.phaseStart = time.Now()
		o.mu.Unlock()
		if key, ok := phaseKeys[prev]; ok && elapsed > 2*time.Second {
			o.Timings.Record(key, elapsed)
		}
		o.log.Info("phase changed", "event", "phase_changed",
			"from", prev, "to", next, "prev_seconds", elapsed.Seconds())
	}
}

// currentPhase derives the user-visible startup phase.
func (o *Orchestrator) currentPhase() string {
	o.mu.Lock()
	mode := o.sess.Mode
	genCount := o.genCount
	queued := len(o.queue)
	last := o.lastGood
	o.mu.Unlock()
	if mode == session.ModeNoise {
		return "playing"
	}
	if o.eng == nil {
		return "engine unavailable"
	}
	if !o.eng.Ready() {
		if p, ok := o.eng.(interface{ Phase() string }); ok {
			switch ph := p.Phase(); ph {
			case "ready":
				return "generating first track"
			case "":
				return "starting engine"
			case "hibernated":
				// Phased mode sleeps the engine on purpose while music
				// plays from the buffer; that is normal operation, not
				// a stuck startup.
				if queued > 0 || last != nil || genCount > 0 {
					return "playing"
				}
				return "starting engine"
			default:
				return ph
			}
		}
		return "starting engine"
	}
	if genCount == 0 && queued == 0 && last == nil {
		return "generating first track"
	}
	return "playing"
}

// PhaseInfo reports the current phase with elapsed and expected durations
// for progress displays.
func (o *Orchestrator) PhaseInfo() (phase string, elapsed, expected time.Duration, slow bool) {
	o.mu.Lock()
	phase = o.phase
	start := o.phaseStart
	o.mu.Unlock()
	if phase == "" {
		phase = o.currentPhase()
		start = time.Now()
	}
	elapsed = time.Since(start)
	if key, ok := phaseKeys[phase]; ok {
		expected = o.Timings.Expected(key)
		slow = elapsed > expected*5/2
	}
	return phase, elapsed, expected, slow
}

// engineFailed reports whether the engine is in a failed/unavailable state
// (as opposed to still starting).
func (o *Orchestrator) engineFailed() bool {
	if o.eng == nil {
		return true
	}
	if p, ok := o.eng.(interface{ Phase() string }); ok {
		return p.Phase() == "unavailable"
	}
	return false
}

// seedFromLibrary starts playback instantly from a banked track when the
// library has one for this session's vibe.
func (o *Orchestrator) seedFromLibrary() {
	o.mu.Lock()
	mode := o.sess.Mode
	sessCopy := o.sess
	o.mu.Unlock()
	if o.eng == nil || mode != session.ModeMusic {
		return
	}
	track, libID, ok := o.Library.Pick(library.Key(sessCopy))
	if !ok {
		return
	}
	track.ID = newTrackID()
	o.fillTitle(track, "")
	o.mu.Lock()
	o.queue = append(o.queue, track)
	o.lastGood = track
	o.lastGoodEpoch = o.epoch
	// The same file must not also be offered as filler under its
	// library id: a remote client would list and play it twice.
	if o.seededLib == nil {
		o.seededLib = map[string]bool{}
	}
	o.seededLib[libID] = true
	o.mu.Unlock()
	o.log.Info("instant start from library", "event", "library_start", "prompt", track.Prompt)
	o.emit("playing a saved track for this vibe while a fresh one generates")
}
