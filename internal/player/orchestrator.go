package player

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"bgm/internal/audio"
	"bgm/internal/config"
	"bgm/internal/engine"
	"bgm/internal/prompting"
	"bgm/internal/session"
	"bgm/internal/state"
)

// Event is a transient user-facing message from the stream machinery.
type Event struct {
	// Text is ready to display.
	Text string
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
	// EngineTail holds recent engine output lines, newest last, when the
	// engine exposes them.
	EngineTail []string
	// Phase names the current startup phase ("starting engine",
	// "loading models", "generating first track") or "playing".
	Phase string
	// PhaseElapsed is how long the current phase has been running;
	// PhaseExpected is its usual duration from past measurements.
	PhaseElapsed  time.Duration
	PhaseExpected time.Duration
	// PhaseSlow reports the phase has exceeded ~2.5x its usual duration.
	PhaseSlow bool
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

	mu         sync.Mutex
	sess       *session.Session
	queue      []*engine.Track
	epoch      int
	lastGood   *engine.Track
	cur        source
	switchReq  bool
	paused     bool
	genBusy    bool
	genCount   int
	lastGen    time.Duration
	exporting  string
	phase      string
	phaseStart time.Time
	started    time.Time
	firstMusic bool

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
		cfg:     cfg,
		eng:     eng,
		builder: builder,
		store:   store,
		log:     log,
		player:  pl,
		ring:    audio.NewRing(2 * audio.BytesPerSecond),
		sess:    sess,
		events:  make(chan Event, 16),
		wake:    make(chan struct{}, 1),
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
	o.wg.Add(4)
	go func() { defer o.wg.Done(); o.genLoop(ctx) }()
	go func() { defer o.wg.Done(); o.mixLoop(ctx) }()
	go func() { defer o.wg.Done(); o.pumpLoop(ctx) }()
	go func() { defer o.wg.Done(); o.phaseLoop(ctx) }()
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

func (o *Orchestrator) kickGen() {
	select {
	case o.wake <- struct{}{}:
	default:
	}
}

// saveSession persists a snapshot of the current session, logging failures.
func (o *Orchestrator) saveSession() {
	_, cp := o.snapshotSession()
	if err := o.store.Save(cp); err != nil {
		o.log.Error("session save failed", "event", "session_save_failed", "error", err.Error())
	}
}

// snapshotSession returns the epoch and a copy of the session safe to read
// without the lock.
func (o *Orchestrator) snapshotSession() (int, *session.Session) {
	o.mu.Lock()
	defer o.mu.Unlock()
	cp := *o.sess
	cp.Tweaks = append([]session.Entry(nil), o.sess.Tweaks...)
	cp.History = append([]session.Entry(nil), o.sess.History...)
	return o.epoch, &cp
}

// genLoop keeps the queue filled while in music mode.
func (o *Orchestrator) genLoop(ctx context.Context) {
	failures := 0
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
			failures++
			o.log.Error("generation failed", "event", "generation_failed", "error", err.Error(), "failures", failures)
			if failures == 1 || failures%5 == 0 {
				o.emit("generation failed; will keep retrying (see log)")
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff(failures)):
			}
			continue
		}
		failures = 0
		o.mu.Lock()
		if epoch == o.epoch {
			o.queue = append(o.queue, track)
			o.lastGood = track
			o.genCount++
			o.lastGen = elapsed
		}
		kept := epoch == o.epoch
		o.mu.Unlock()
		o.log.Info("generation finished", "event", "generation_finished",
			"elapsed_seconds", elapsed.Seconds(), "track_seconds", track.Duration().Seconds(),
			"kept", kept, "prompt", track.Prompt)
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

// backoff returns the retry delay after n consecutive failures.
func backoff(n int) time.Duration {
	d := time.Duration(n) * 5 * time.Second
	if d > 2*time.Minute {
		d = 2 * time.Minute
	}
	return d
}

// pumpLoop moves audio from the ring buffer to the playback backend,
// applying volume and pause. The backend's own pacing provides
// backpressure; the ring's non-blocking reads make silence the floor.
func (o *Orchestrator) pumpLoop(ctx context.Context) {
	const chunkFrames = audio.SampleRate / 10 // 100 ms
	buf := make([]byte, audio.FramesToBytes(chunkFrames))
	silence := make([]byte, len(buf))
	var lastUnderruns int64
	for ctx.Err() == nil {
		o.mu.Lock()
		paused := o.paused
		o.mu.Unlock()
		var out []byte
		if paused {
			out = silence
		} else {
			if _, err := o.ring.Read(buf); err != nil {
				return // ring closed and drained
			}
			out = buf
			if u := o.ring.Underruns(); u != lastUnderruns {
				o.log.Warn("output underrun", "event", "underrun", "total", u)
				lastUnderruns = u
			}
			vol := float64(o.volume.Load()) / 100
			if vol < 1 {
				samples := audio.BytesToSamples(out)
				audio.ApplyGain(samples, vol*vol) // perceptual taper
				out = audio.SamplesToBytes(samples)
			}
		}
		if _, err := o.player.Write(out); err != nil {
			if ctx.Err() != nil {
				return
			}
			o.log.Error("player write failed, switching to silent output", "event", "player_failed", "error", err.Error())
			o.emit("audio output failed; continuing silently (see log)")
			o.player.Close()
			np, nerr := audio.NewPlayer("null", "")
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
