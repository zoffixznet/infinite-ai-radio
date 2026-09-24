package player

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"iar/internal/audio"
	"iar/internal/config"
	"iar/internal/engine"
	"iar/internal/prompting"
	"iar/internal/session"
	"iar/internal/songbook"
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
	// Configured reports that the language is in the machine's own
	// list. One that is not comes from the session itself - it was
	// saved singing in a language nobody has configured since - and
	// interfaces say so rather than hiding it.
	Configured bool
}

// Status is a point-in-time snapshot for status displays.
type Status struct {
	// State summarizes what is audible: starting, playing, stopped,
	// looping, bed or paused.
	State string
	// Source describes the current audio source.
	Source string
	// Build is the running binary's version string, the same one the
	// disk buffer is stamped with. A listener comparing the machine
	// with the phone is asking whether they are the same build.
	Build string
	// Elapsed and Duration describe progress through the current track
	// (zero for endless sources).
	Elapsed  time.Duration
	Duration time.Duration
	// Queued is the number of decoded tracks this machine's own player
	// holds ahead in memory (a fixed few); the store is reported below.
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
	// Stopped reports this machine's player is switched off: nothing
	// plays here and nothing is taken from the store, which goes on
	// filling for the other listeners.
	Stopped bool
	// Underruns counts output buffer underruns since start.
	Underruns int64
	// GenCount and LastGenTime describe generation throughput.
	GenCount    int
	LastGenTime time.Duration
	// Exporting describes a running export, empty otherwise.
	Exporting string
	// BufferedTracks and BufferedSeconds are the songs already rendered
	// and waiting to play (in the store, plus the in-memory prefetch).
	BufferedTracks int
	// StoreLevel is how many songs nobody has taken yet, and
	// StoreTarget the depth the generator fills to; NextBatchIn is how
	// long the tap still waits before the next rung, zero when a rung
	// is due or the store is full.
	StoreLevel  int
	StoreTarget int
	NextBatchIn time.Duration
	// WordsmithWant/WordsmithWrote report a wordsmith round in
	// progress: the writer holding the card, writing the coming
	// batch's words. Zero when no round is running.
	WordsmithWant  int
	WordsmithWrote int
	// BatchRendered counts songs rendered by the current (or latest)
	// batch cycle, so the gauge can say "rendered 40 · 23 to play"
	// instead of an ambiguous count.
	BatchRendered int
	// RampBatch is the song count the running (or next) batch makes
	// (1 for a fresh context, then the ladder's rungs, capped at the
	// room left in the store); 0 when the store is full.
	RampBatch       int
	BufferedSeconds float64
	// PlannedTracks and PlannedSeconds are songs the planner has
	// written but the renderer has not turned into audio yet.
	PlannedTracks  int
	PlannedSeconds float64
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
	// LoopOn reports a listener asked for the playing track to repeat
	// until they turn the loop off.
	LoopOn bool
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
	// Buffer is the on-disk buffer of phased generation (plans awaiting
	// render, rendered songs awaiting play). Set before Start; nil (or
	// buffer.phased=false in the config) selects the fused path.
	Buffer *trackbuffer.Store
	// BuildStamp identifies the running binary (its version string).
	// A disk buffer stamped by a different build is cleared at start:
	// commits change how songs are made, and a buffer of a fixed bug's
	// output should not outlive the fix.
	BuildStamp string
	// SnippetsDir is where the save command writes captured tracks.
	SnippetsDir string
	// Songbook remembers every song rendered - its hash, name, words
	// and where it was saved - past the life of its audio here. New
	// gives a memory-only book; set a file-backed one before Start.
	Songbook *songbook.Book
	// Retention is how long auto-named sessions are kept after they last
	// played before the periodic sweep removes them; zero disables the
	// sweep, which is the default: sessions branch on every change to
	// the sound, so the old ones are what a listener goes back to, not
	// litter. Set before Start.
	Retention time.Duration
	// LegacyLanguagesOff is the standing off-list from the
	// configuration as it was before the languages a session sings
	// became the session's own. It is read only to convert sessions
	// saved back then, and only those that carry no answer at all.
	LegacyLanguagesOff []string
	// StateDir, when set, records which session is playing so other
	// processes (the CLI's delete) can refuse to remove it.
	StateDir *state.Dir
	// Idle starts this machine's player switched off: the generator
	// runs and the remote serves, but the speakers take nothing until
	// the play command. Set before Start.
	Idle bool
	// tempStore is the run-only store Start made when none was set;
	// Close removes it.
	tempStore string
	// Telemetry, when set before Start, samples system and graphics
	// memory and the engine's model residency for the status display.
	// Nil leaves those fields of Status empty.
	Telemetry *telemetry.Sampler

	mu       sync.Mutex
	sess     *session.Session
	queue    []*engine.Track
	epoch    int
	lastGood *engine.Track
	// incoming is the song the mixer is fading in: taken off the queue,
	// not yet the current one. For the length of the crossfade it is in
	// no other field, so without this a listener whose phone is already
	// playing it - the usual case, a phone runs ahead of the speakers -
	// could neither save nor rename it for those seconds.
	incoming *engine.Track
	// lastGoodEpoch is the steering epoch lastGood was generated in.
	// When it is behind, looping lastGood replays the sound the
	// listener has just moved away from, which is worth saying out loud
	// and worth refusing to skip into.
	lastGoodEpoch int
	// loopNoticeEpoch remembers which epoch already announced the loop
	// fallback, so a burst of skips does not flush the event channel.
	loopNoticeEpoch int
	cur             source
	// loopOn marks a listener's request to repeat the playing track;
	// it holds only while loopEpoch matches the steering epoch, so any
	// context change breaks the loop without ceremony.
	loopOn    bool
	loopEpoch int
	// switchReq forces the mixer to the next source; steerPending asks
	// for the same switch, but only once a post-steer track is queued.
	switchReq    bool
	steerPending bool
	paused       bool
	// produced reports that the playing session has made a song - in
	// this run, or before it was loaded. A change to the sound branches
	// the session only once it has: a state that never made a song is
	// not one anyone wants to go back to.
	produced bool
	// lastTweak is when the sound was last changed; changes within the
	// branch window of it stay in the same branch.
	lastTweak time.Time
	// stopped switches this machine's player off: the mixer feeds the
	// speakers silence and the taker takes nothing from the store.
	// Distinct from paused, which only mutes what is playing.
	stopped  bool
	genBusy  bool
	genCount int
	// Phased-generation state: the epoch the buffer currently belongs
	// to and the last handed-out file sequence number.
	phasedEpoch int
	phasedSeq   int
	// The ladder: rung indexes ladder for the batch the next cycle
	// makes; rungMade and rungSeconds count what this rung has made so
	// far; rungDoneAt and rungWait say when the last rung finished and
	// how long its music runs, which is how long the tap waits before
	// the next; skipCredit is the music listeners skipped since, which
	// shortens that wait. All reset by a steer.
	rung        int
	rungMade    int
	rungSeconds float64
	rungDoneAt  time.Time
	rungWait    time.Duration
	skipCredit  time.Duration
	// phasedSynced flags that the buffer was reconciled with this
	// run's identity at least once; cycleCooldown blocks new cycles
	// after persistent failures; renderFails counts render failures
	// per plan sequence so a poisoned plan gets dropped.
	phasedSynced  bool
	cycleCooldown time.Time
	renderFails   map[int]int
	// Cached store depth, refreshed by the phased loops for the status
	// display: the level (songs nobody has taken), and the same plus
	// the in-memory prefetch as songs and seconds.
	bufLevel       int
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
	// engineBusy mirrors whether a generation cycle currently holds the
	// graphics card; the lyric writer works only while it is free.
	engineBusy atomic.Bool
	// batchRenderedNow counts renders in the current batch cycle.
	batchRenderedNow int
	// batchCapNow is the size THIS cycle set out to render.
	batchCapNow int
	// wordsmithWantNow/wordsmithWroteNow mirror the running wordsmith
	// round for the status display.
	wordsmithWantNow, wordsmithWroteNow int
	// The snippet writer. Saves wait their turn in saveQueue, oldest
	// first, and one worker writes them one after another; saveNow is
	// the one it is writing. saveUploads names the songs arriving as
	// copies from a device, which are written on the caller's own
	// goroutine; together with the queue they say which songs already
	// have a save on the way, so no song is ever saved twice. savePaths
	// are the file names spoken for by saves not yet finished, so two
	// songs saved within one second do not land on one name. All
	// guarded by o.mu; saveWorker says the worker goroutine is alive,
	// saveClosed that Close has begun and no save may start.
	saveQueue   []*saveJob
	saveNow     *saveJob
	saveWorker  bool
	saveClosed  bool
	saveUploads map[string]bool
	savePaths   map[string]bool
	saveWG      sync.WaitGroup
	// playCount numbers the tracks as they start playing (per process);
	// curTrackNum is the playing track's number.
	playCount   int
	curTrackNum int
	// saveLanguages persists an edited vocal-language catalogue - the
	// machine's list of what can be offered. Which of them a session
	// sings in belongs to the session, not here. Nil means an edited
	// catalogue lives only for this run.
	saveLanguages func(names []string) error

	volume atomic.Int32
	events chan Event
	wake   chan struct{}

	genMu  sync.Mutex // serializes engine use between playback and export
	runCtx context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New assembles an orchestrator. eng may be nil to run without a music
// engine (silence, with clear messaging).
func New(cfg config.Config, eng engine.Engine, builder *prompting.Builder, store *session.Store, sess *session.Session, pl audio.Player, log *slog.Logger) *Orchestrator {
	o := &Orchestrator{
		cfg:         cfg,
		eng:         eng,
		builder:     builder,
		store:       store,
		log:         log,
		player:      pl,
		ring:        audio.NewRing(2 * audio.BytesPerSecond),
		sess:        sess,
		events:      make(chan Event, 16),
		wake:        make(chan struct{}, 1),
		Songbook:    songbook.Open("", log),
		saveUploads: map[string]bool{},
		savePaths:   map[string]bool{},
		// A session that has played before has made songs before:
		// resuming one and changing it straight away must still keep
		// what it sounded like. A session made moments ago has nothing
		// behind it to keep.
		produced: !sess.LastPlayed.IsZero(),
	}
	o.volume.Store(int32(cfg.Volume))
	o.adoptLanguages(sess)
	return o
}

// adoptLanguages settles which languages a session sings in as it
// becomes the playing one. Two cases: a session saved before that was
// part of the session at all (the old shape said which of the
// then-configured languages were switched OFF, so it can only be read
// against a catalogue - the machine's current one), and a preset that
// names a language of its own, which becomes its session's list so the
// pills show what the songs will actually be sung in.
func (o *Orchestrator) adoptLanguages(s *session.Session) {
	if s == nil {
		return
	}
	cat := o.builder.Languages()
	names := make([]string, 0, len(cat))
	for _, l := range cat {
		names = append(names, l.Name)
	}
	s.AdoptLanguages(names, o.LegacyLanguagesOff)
	// Only a session that has never been asked takes the preset's
	// answer. A session that says it sings in nothing said so on
	// purpose, and must not have a language handed back to it on every
	// load - which is what "empty" used to mean here.
	if s.SungLanguages != nil || s.Spec == nil || s.Spec.LanguagePinned || s.Spec.VocalLanguage == "" {
		return
	}
	name := prompting.LanguageName(s.Spec.VocalLanguage)
	if name == "" || prompting.LanguageCode(name) == "" {
		// A tag nobody can name back is no use as a language: it would
		// be shown as a language called "ceb" and written in by that
		// name. Leave the choice to the engine instead.
		return
	}
	s.SungLanguages = []string{name}
}

// Events returns the stream of transient user-facing messages.
func (o *Orchestrator) Events() <-chan Event { return o.events }

// Start launches the stream. It returns immediately; audio begins with the
// silence and crossfades into generated music when ready.
func (o *Orchestrator) Start(ctx context.Context) {
	ctx, o.cancel = context.WithCancel(ctx)
	o.runCtx = ctx
	now := time.Now()
	o.mu.Lock()
	o.started = now
	o.phaseStart = now
	o.stopped = o.Idle
	o.mu.Unlock()
	if o.Idle {
		o.log.Info("starting with the player off", "event", "player_idle")
	}
	// Prompt-seeded sessions carry a raw user description; let the
	// helper enrich it in the background when it is available.
	o.mu.Lock()
	initial := o.sess
	fromPrompt := strings.HasPrefix(initial.Name, "prompt-") && len(initial.Tweaks) == 0 &&
		!initial.SeedExpanded
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
	o.sweepSaveScraps()
	if o.Telemetry != nil {
		o.Telemetry.Start(ctx)
	}
	o.wg.Add(5)
	o.builder.SetPhased(true)
	if o.Buffer == nil {
		// A radio with nowhere to keep its songs keeps them for the run
		// only, in a store that goes with the process.
		dir := filepath.Join(os.TempDir(), fmt.Sprintf("iar-store-%d-%d", os.Getpid(), rand.IntN(100000)))
		o.Buffer = trackbuffer.New(dir, o.cfg.MP3Quality, o.log)
		o.tempStore = dir
		o.log.Warn("no store configured; songs are kept for this run only", "event", "store_temporary", "dir", dir)
	}
	// A different commit built this store: newer builds fix bugs and
	// change how songs are made, so yesterday's output does not get to
	// speak for today's binary.
	if o.BuildStamp != "" && o.Buffer.Build() != o.BuildStamp {
		if dropped := o.Buffer.DropAll(); dropped > 0 {
			o.log.Info("buffer from another build cleared",
				"event", "buffer_build_dropped", "files", dropped,
				"was", o.Buffer.Build(), "now", o.BuildStamp)
		}
		o.Buffer.SetBuild(o.BuildStamp)
	}
	// A restart is not a steer. The store a previous run left for this
	// same steering context is hours of finished work; adopt its epoch
	// (the in-memory counter starts at zero every run) so playback
	// continues from it instead of planning the world again. Only a
	// prompt, a steer or a language change resets generation.
	o.adoptDiskBuffer()
	// A backlog from before sheet reuse was capped can hold dozens of
	// plans and songs singing identical words; sweep it once so the
	// cap holds for what is already on disk too.
	o.Buffer.DedupeSheets(2)
	// The generator: a producer cycle (plan batch, render batch,
	// hibernate) filling the store; and this machine's own player: a
	// taker that decodes songs from the store into the prefetch, the
	// mixer and the pump.
	go func() { defer o.wg.Done(); o.cycleLoop(ctx) }()
	go func() { defer o.wg.Done(); o.feedLoop(ctx) }()
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
	if sess.Preset != "" {
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
		// Answered once is answered: a session that is resumed on every
		// restart would otherwise be re-expanded - and quietly
		// re-steered - each time.
		o.sess.SeedExpanded = true
		o.mu.Unlock()
		o.saveSession()
		if changed {
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
	// No save may join the queue from here on: the worker is about to
	// be waited for, and a save arriving after that would have nobody
	// to write it.
	o.mu.Lock()
	o.saveClosed = true
	o.mu.Unlock()
	if o.cancel != nil {
		o.cancel()
	}
	o.ring.Close()
	o.wg.Wait()
	o.saveWG.Wait()
	o.saveSession()
	if o.tempStore != "" {
		os.RemoveAll(o.tempStore)
	}
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

// newTrackID returns a unique id for a track entering the stream.
func newTrackID() string {
	return fmt.Sprintf("t-%d-%04d", time.Now().UnixMilli(), rand.IntN(10000))
}

// fillTitle gives a track a deterministic display name derived from its
// own description. It is the last resort, for tracks that arrive with no
// name of their own - a song stored by an older run, or one whose
// words the engine invented - and what it writes is final.
func (o *Orchestrator) fillTitle(t *engine.Track) {
	if t.Title == "" {
		t.Title, t.Subtitle = prompting.TrackTitle(t.Prompt)
	}
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
// back stalls the mixer against a two-second buffer within seconds.
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
	genCount := o.genCount
	queued := len(o.queue)
	last := o.lastGood
	writing := o.wordsmithWantNow > 0
	vocal := o.sess.Vocal
	o.mu.Unlock()
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
				// The render daemon is down. While the writer is at
				// work the radio is making songs - the words come
				// first - and the phase says so rather than claiming a
				// startup that has not begun.
				if writing || o.builder.WriterWorking() {
					return writingPhase(vocal)
				}
				// Otherwise phased mode sleeps the engine on purpose
				// while music plays from the buffer; that is normal
				// operation, not a stuck startup.
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

// writingPhase is the phase shown while the writer works: words for
// a singing session, descriptions for an instrumental one.
func writingPhase(vocal bool) string {
	if vocal {
		return "writing song words"
	}
	return "writing song descriptions"
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
