package player

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"iar/internal/audio"
	"iar/internal/config"
	"iar/internal/engine"
	"iar/internal/engine/enginetest"
	"iar/internal/prompting"
	"iar/internal/session"
	"iar/internal/trackbuffer"
)

// phasedMock is an engine that supports the phased pipeline and counts
// what happened to it.
type phasedMock struct {
	plans      atomic.Int32
	renders    atomic.Int32
	hibernated atomic.Int32
	active     atomic.Bool
}

func (m *phasedMock) Name() string { return "phased-mock" }
func (m *phasedMock) Ready() bool  { return true }

func (m *phasedMock) Generate(ctx context.Context, spec engine.Spec) (*engine.Track, error) {
	panic("phased pipeline must not call the fused Generate")
}

func (m *phasedMock) Plan(ctx context.Context, spec engine.Spec) (*engine.Plan, error) {
	// Only the first cycle's plan succeeds; later cycles block until
	// the test tears down, keeping every assertion race-free.
	if m.plans.Add(1) > 1 {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return &engine.Plan{
		Spec:       spec,
		Caption:    "mock caption",
		Lyrics:     "[Verse 1]\nmock words",
		AudioCodes: "mock-codes",
		Seconds:    1,
	}, nil
}

func (m *phasedMock) Render(ctx context.Context, plan *engine.Plan) (*engine.Track, error) {
	m.renders.Add(1)
	return &engine.Track{
		Samples: make([]int16, audio.SampleRate*audio.Channels), // 1s of silence
		Spec:    plan.Spec,
		Prompt:  plan.Caption,
		Lyrics:  plan.Lyrics,
	}, nil
}

func (m *phasedMock) SetEngineActive(on bool) { m.active.Store(on) }
func (m *phasedMock) HibernateEngine() bool   { m.hibernated.Add(1); return true }

func TestPhasedCyclePlansRendersFeedsAndHibernates(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	eng := &phasedMock{}
	pl := &capturePlayer{}
	cfg := testConfig()
	// A store stocked at one song is stocked after the opener, so
	// exactly one cycle runs and then hibernates.
	cfg.Buffer.ReserveSongs, cfg.Buffer.LowMinutes = 1, 0
	sess := session.New()
	builder := prompting.NewBuilder(nil, testLogger())
	o := New(cfg, eng, builder, session.NewStore(t.TempDir()), sess, pl, testLogger())
	o.Buffer = trackbuffer.New(t.TempDir(), 9, testLogger())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	o.Start(ctx)
	t.Cleanup(func() { o.Close() })

	// Stage 0 of the ramp: exactly one song planned and rendered, the
	// engine hibernated after the cycle, and the song fed to playback.
	waitFor(t, 15*time.Second, "first phased cycle", func() bool {
		return eng.renders.Load() >= 1 && eng.hibernated.Load() >= 1
	})
	waitFor(t, 15*time.Second, "track fed from disk", func() bool {
		o.mu.Lock()
		defer o.mu.Unlock()
		return o.lastGood != nil
	})
	if got := eng.renders.Load(); got != 1 {
		t.Fatalf("stage-0 cycle rendered %d tracks; want exactly 1", got)
	}
	o.mu.Lock()
	track := o.lastGood
	o.mu.Unlock()
	if track.Prompt != "mock caption" || track.Lyrics != "[Verse 1]\nmock words" {
		t.Fatalf("track lost its plan identity: %+v", track)
	}
	if track.Title == "" {
		t.Fatal("fed track has no title")
	}
}

// A configured disk buffer is not a phased run when there is no engine
// at all, and the phased export path used to walk straight into it:
// `mp3 1` crashed the whole radio. Now the export is refused and the
// radio stands.
func TestExportWithADiskBufferButNoEngineDoesNotCrash(t *testing.T) {
	cfg := testConfig()
	sess := session.New()
	builder := prompting.NewBuilder(nil, testLogger())
	o := New(cfg, nil, builder, session.NewStore(t.TempDir()), sess, &capturePlayer{}, testLogger())
	o.Buffer = trackbuffer.New(t.TempDir(), 9, testLogger())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	o.Start(ctx)
	t.Cleanup(func() { o.Close() })

	if ack := o.Export(1, "", t.TempDir()); !strings.Contains(ack, "not available") {
		t.Fatalf("export ack = %q, want it refused", ack)
	}
	if st := o.Status(); st.Exporting != "" {
		t.Fatalf("an export is running with no engine: %q", st.Exporting)
	}
}

// A restart is not a steer: a buffer left by a previous run for the
// same steering context is adopted - its epoch becomes this run's -
// instead of being invisible to a process whose epoch counter starts
// at zero. Only a prompt, steer or language change resets generation.
func TestRestartAdoptsThePreviousRunsBuffer(t *testing.T) {
	dir := t.TempDir()
	sess := session.New()
	key := sess.ContextKey()

	// The previous run: context recorded, songs stored under epoch 3.
	prev := trackbuffer.New(dir, 0, testLogger())
	prev.SetContext(key)
	for seq := 1; seq <= 3; seq++ {
		track := &engine.Track{
			Lyrics:  fmt.Sprintf("[Verse]\nsong %d", seq),
			Samples: make([]int16, 9600),
		}
		if _, err := prev.PutTrack(context.Background(), 3, seq, track); err != nil {
			t.Fatal(err)
		}
	}

	// The new run boots with epoch 0.
	cfg := testConfig()
	o := New(cfg, enginetest.NewMock(), prompting.NewBuilder(nil, testLogger()), session.NewStore(t.TempDir()), sess, &capturePlayer{}, testLogger())
	o.Buffer = trackbuffer.New(dir, 0, testLogger())
	o.adoptDiskBuffer()

	o.mu.Lock()
	epoch := o.epoch
	o.mu.Unlock()
	if epoch != 3 {
		t.Fatalf("epoch = %d; the previous run's buffer was not adopted", epoch)
	}
	if got := len(o.Buffer.List(3)); got != 3 {
		t.Fatalf("adopted buffer lists %d songs, want 3", got)
	}
}

// A different steering context on disk is another radio's leftovers;
// adoption declines and the ordinary context check clears it later.
func TestAdoptionRefusesAnotherContextsBuffer(t *testing.T) {
	dir := t.TempDir()
	prev := trackbuffer.New(dir, 0, testLogger())
	prev.SetContext("some-other-station")
	track := &engine.Track{Lyrics: "[Verse]\nx", Samples: make([]int16, 9600)}
	if _, err := prev.PutTrack(context.Background(), 7, 1, track); err != nil {
		t.Fatal(err)
	}

	cfg := testConfig()
	o := New(cfg, enginetest.NewMock(), prompting.NewBuilder(nil, testLogger()), session.NewStore(t.TempDir()), session.New(), &capturePlayer{}, testLogger())
	o.Buffer = trackbuffer.New(dir, 0, testLogger())
	o.adoptDiskBuffer()

	o.mu.Lock()
	epoch := o.epoch
	o.mu.Unlock()
	if epoch != 0 {
		t.Fatalf("epoch = %d; another context's buffer must not be adopted", epoch)
	}
}

// ladderOrchestrator is a phased radio with an empty store and nothing
// running, for walking the ladder by hand: made closes a rung the way
// a cycle's renders do.
func ladderOrchestrator(t *testing.T, cfg config.Config) (o *Orchestrator, made func(songs int, seconds float64)) {
	t.Helper()
	o = New(cfg, &phasedMock{}, prompting.NewBuilder(nil, testLogger()),
		session.NewStore(t.TempDir()), session.New(), &capturePlayer{}, testLogger())
	o.Buffer = trackbuffer.New(t.TempDir(), 9, testLogger())
	made = func(songs int, seconds float64) {
		o.mu.Lock()
		o.rungMade += songs
		o.rungSeconds += float64(songs) * seconds
		o.mu.Unlock()
		o.completeRung(0)
	}
	return o, made
}

// The ladder climbs on the clock. One opener; the ten-song batch
// straight after it; then every rung waits as long as its music runs
// before the next, less what listeners skipped; a rung that was cut
// short stays open; and every rung is made whole, never trimmed to
// the room left in the store.
func TestTheLadderClimbsOnTime(t *testing.T) {
	o, made := ladderOrchestrator(t, testConfig())

	if got := o.batchFor(); got != 1 {
		t.Fatalf("the first batch = %d, want the opener alone", got)
	}
	// The opener waits for nothing: the ten-song batch follows it.
	made(1, 200)
	if left, next := o.rungWaitLeft(), o.batchFor(); left != 0 || next != 10 {
		t.Fatalf("after the opener: wait %v, next batch %d; want 0 and 10", left, next)
	}
	// Ten songs of 200 seconds: the next rung is due when they have
	// played out, and nothing is planned before then.
	made(10, 200)
	if left := o.rungWaitLeft(); left < 1990*time.Second || left > 2000*time.Second {
		t.Fatalf("after the ten-batch the wait is %v, want about 2000s", left)
	}
	if o.plannable(0) != 0 || o.wantCycle(0) {
		t.Fatal("a rung was due while the last one's music was still playing")
	}
	// Listeners skipping through it bring the rung forward; the
	// credits add up across them.
	o.ReportSkipped(1500)
	o.ReportSkipped(500)
	if left, next := o.rungWaitLeft(), o.plannable(0); left != 0 || next != 20 {
		t.Fatalf("after 2000s skipped: wait %v, next batch %d; want 0 and 20", left, next)
	}
	// A rung cut short stays open: the next cycle makes the rest.
	made(3, 200)
	if next := o.batchFor(); next != 17 {
		t.Fatalf("after 3 of 20: next batch %d, want the remaining 17", next)
	}
	made(17, 200)
	o.ReportSkipped(20 * 200)
	// The 40-rung is made whole, whatever the store holds.
	if next := o.batchFor(); next != 40 {
		t.Fatalf("the 40-rung's batch = %d, want the whole 40", next)
	}
}

// The wake mark: the reserve, plus the low-water minutes turned into
// songs at the mean length of what is in store - or at a typical
// song's length while the store holds nothing to measure. The default
// settings give 93: a fresh phone's 80, and 45 minutes of three-and-a-
// half-minute songs on top.
func TestTheWakeMarkArithmetic(t *testing.T) {
	def := config.Default().Buffer
	for _, tc := range []struct {
		name    string
		b       config.Buffer
		level   int
		seconds float64
		want    int
	}{
		{"defaults, an empty store", def, 0, 0, 93},
		{"defaults, typical songs", def, 20, 20 * 210, 93},
		{"defaults, three-minute songs", def, 10, 10 * 180, 95},
		{"defaults, five-minute songs", def, 10, 10 * 300, 89},
		{"a partial song still counts whole", def, 1, 1, 80 + 2700},
		{"no minutes beyond the reserve", config.Buffer{ReserveSongs: 80}, 5, 5 * 200, 80},
		{"a reserve of one", config.Buffer{ReserveSongs: 1}, 0, 0, 1},
		{"minutes on a small reserve", config.Buffer{ReserveSongs: 3, LowMinutes: 10}, 4, 4 * 120, 8},
	} {
		if got := wakeMark(tc.b, tc.level, tc.seconds); got != tc.want {
			t.Errorf("%s: wake mark %d, want %d", tc.name, got, tc.want)
		}
	}
	// The store's ceiling is a whole top batch made just under the mark.
	if got := storeCeiling(93); got != 172 {
		t.Errorf("ceiling at a mark of 93 = %d, want 172", got)
	}
}

// A stocked store is the off switch: no rung is due while the store
// holds the wake mark or more, however many songs are taken above it.
// A fresh phone filling its bank takes dozens at once, and none of
// that wakes the engine; the take that brings the level under the
// mark does, and the batch it wakes for is the whole top rung.
func TestTakingSongsAboveTheMarkDoesNotWakeTheEngine(t *testing.T) {
	skipWithoutFFmpeg(t)
	o, _ := idOrchestrator(t)
	o.cfg.Buffer.ReserveSongs, o.cfg.Buffer.LowMinutes = 3, 0
	o.mu.Lock()
	o.rung = topRung
	o.mu.Unlock()
	for seq := 1; seq <= 10; seq++ {
		renderSong(t, o, seq, fmt.Sprintf("t-1790000000000-05%02d", seq))
	}
	if o.plannable(0) != 0 || o.wantCycle(0) {
		t.Fatal("a stocked store wants a cycle")
	}
	// Six songs go at once, and the store still holds more than the mark.
	for seq := 1; seq <= 6; seq++ {
		o.Buffer.Take(0, fmt.Sprintf("e00000000-%08d", seq))
	}
	if level, _ := o.Buffer.Level(0); level != 4 {
		t.Fatalf("level after six takes = %d, want 4", level)
	}
	if o.plannable(0) != 0 || o.wantCycle(0) {
		t.Fatal("six songs taken above the mark woke the engine")
	}
	// At the mark exactly the store is still stocked.
	o.Buffer.Take(0, "e00000000-00000007")
	if o.plannable(0) != 0 || o.wantCycle(0) {
		t.Fatal("a store holding exactly the mark woke the engine")
	}
	// Under it, the engine wakes for a whole top batch.
	o.Buffer.Take(0, "e00000000-00000008")
	if next := o.plannable(0); next != 80 || !o.wantCycle(0) {
		t.Fatalf("under the mark: next batch %d, want the whole 80 and a cycle due", next)
	}
	if got := o.sleepReasonNow(0); got != sleepNoWork {
		t.Errorf("under the mark the sleep reason is %d, want no work (a batch is due)", got)
	}
}

// The ladder's rungs, whole: a batch is the rung's size less what an
// interrupted cycle already made of it, and never less for the store
// being deep.
func TestBatchLadder(t *testing.T) {
	cfg := testConfig()
	o := New(cfg, &phasedMock{}, prompting.NewBuilder(nil, testLogger()),
		session.NewStore(t.TempDir()), session.New(), &capturePlayer{}, testLogger())
	for _, tc := range []struct{ rung, made, want int }{
		{0, 0, 1},   // the opener: sound as fast as possible
		{1, 0, 10},  // the audition batch
		{2, 0, 20},  // then 20...
		{3, 0, 40},  // ...and 40
		{4, 0, 80},  // 80 at the top, again and again
		{4, 30, 50}, // a rung cut short: the rest of it
		{4, 80, 0},  // a rung made: nothing until it closes
	} {
		o.mu.Lock()
		o.rung, o.rungMade = tc.rung, tc.made
		got := o.batchForLocked()
		o.mu.Unlock()
		if got != tc.want {
			t.Errorf("rung %d with %d made makes %d, want %d", tc.rung, tc.made, got, tc.want)
		}
	}
}

// Past the top rung there is no clock: the rung closes with no wait,
// the ladder stays at the top, and the moment the store falls under
// the mark a whole top batch is plannable - not after the eighty
// songs' music has run, which for a store two phones just drained
// would be a stale timer. Skips have nothing left to shorten.
func TestTheTopRungSetsNoClock(t *testing.T) {
	skipWithoutFFmpeg(t)
	cfg := testConfig()
	cfg.Buffer.ReserveSongs, cfg.Buffer.LowMinutes = 3, 0
	o, made := ladderOrchestrator(t, cfg)
	o.mu.Lock()
	o.rung = topRung
	o.mu.Unlock()
	made(80, 200)
	o.mu.Lock()
	rung, wait, doneAt := o.rung, o.rungWait, o.rungDoneAt
	o.mu.Unlock()
	if rung != topRung || wait != 0 || !doneAt.IsZero() {
		t.Fatalf("after the top rung: rung %d, wait %v, done at %v; want the top, no wait, no clock", rung, wait, doneAt)
	}
	if left := o.rungWaitLeft(); left != 0 {
		t.Fatalf("the top rung left a wait of %v", left)
	}
	// An empty store is under the mark: the next batch is due at once,
	// and it is the whole top rung.
	if next := o.plannable(0); next != 80 || !o.wantCycle(0) {
		t.Fatalf("under the mark after the top rung: plannable %d, want 80 at once", next)
	}
	// Stocked, nothing is due; a skip changes nothing, because there
	// is no clock for it to shorten.
	for seq := 1; seq <= 3; seq++ {
		track := &engine.Track{Samples: make([]int16, audio.SampleRate*audio.Channels), Spec: engine.Spec{Prompt: "p"}}
		if _, err := o.Buffer.PutTrack(context.Background(), 0, seq, track); err != nil {
			t.Fatal(err)
		}
	}
	o.ReportSkipped(3600)
	if o.plannable(0) != 0 || o.wantCycle(0) {
		t.Fatal("a stocked store wants a cycle after a skip")
	}
	if got := o.sleepReasonNow(0); got != sleepStocked {
		t.Errorf("stocked after the top rung: sleep reason %d, want stocked", got)
	}
	// One take under the mark, and the batch is due this instant.
	o.Buffer.Take(0, "e00000000-00000001")
	if next := o.plannable(0); next != 80 || !o.wantCycle(0) {
		t.Fatalf("one take under the mark: plannable %d, want 80 at once", next)
	}
}

// A batch is the unit: a cycle that set out to make a rung makes all
// of it, even though the store crosses the wake mark a song or two
// in. Only the next wake reads the level.
func TestABatchInProgressCompletesAsTheLevelCrossesTheMark(t *testing.T) {
	skipWithoutFFmpeg(t)
	eng := &pausableMock{}
	sess := session.New()
	sess.Vocal = true
	o := heldOrchestrator(t, eng, nil, sess)
	o.cfg.Buffer.ReserveSongs, o.cfg.Buffer.LowMinutes = 2, 0
	// The ten-song rung is due: the store is empty.
	if next := o.plannable(0); next != 10 {
		t.Fatalf("plannable before the cycle = %d, want the ten-song rung", next)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	failures, oomStreak := 0, 0
	o.runCycle(ctx, &failures, &oomStreak)

	if got := eng.renders.Load(); got != 10 {
		t.Fatalf("the cycle rendered %d songs, want the whole rung of 10", got)
	}
	if level, _ := o.Buffer.Level(0); level != 10 {
		t.Fatalf("level after the cycle = %d, want 10", level)
	}
	o.mu.Lock()
	rung, rungMade := o.rung, o.rungMade
	o.mu.Unlock()
	if rung != 2 || rungMade != 0 {
		t.Errorf("after the batch: rung %d with %d made; want the ladder on 20 with nothing made of it", rung, rungMade)
	}
	// Stocked well past the mark, the engine was put down for that
	// reason, and nothing more is due.
	if got := eng.hibernated.Load(); got != 1 {
		t.Errorf("the engine was put down %d times, want once", got)
	}
	if o.plannable(0) != 0 || o.wantCycle(0) {
		t.Error("a stocked store wants a cycle after the batch")
	}
}

// A buffer stamped by another build is cleared wholesale on start: a
// newer commit may have fixed the very bugs those songs carry.
func TestAnotherBuildsBufferIsCleared(t *testing.T) {
	dir := t.TempDir()
	prev := trackbuffer.New(dir, 0, testLogger())
	prev.SetBuild("old-commit")
	prev.SetContext(session.New().ContextKey())
	track := &engine.Track{Lyrics: "[Verse]\nx", Samples: make([]int16, 9600)}
	if _, err := prev.PutTrack(context.Background(), 0, 1, track); err != nil {
		t.Fatal(err)
	}

	cur := trackbuffer.New(dir, 0, testLogger())
	if cur.Build() != "old-commit" {
		t.Fatalf("stored build = %q", cur.Build())
	}
	if cur.Build() != "new-commit" {
		cur.DropAll()
		cur.SetBuild("new-commit")
	}
	if got := len(cur.List(0)); got != 0 {
		t.Fatalf("%d songs survived a build change", got)
	}
	if cur.Build() != "new-commit" {
		t.Fatalf("build stamp not updated: %q", cur.Build())
	}
}

// The ladder rung is recomputed from live play counts, so it climbs
// while a batch is still running. Reporting the batch against the new
// rung is how a finished ten-song batch came to read as "11 of 20" -
// a batch that looked abandoned half way through. The gauge must be
// told the size this cycle actually set out to render.
func TestStatusReportsTheBatchItActuallyRan(t *testing.T) {
	cfg := testConfig()
	o := New(cfg, &phasedMock{}, prompting.NewBuilder(nil, testLogger()),
		session.NewStore(t.TempDir()), session.New(), &capturePlayer{}, testLogger())
	o.Buffer = trackbuffer.New(t.TempDir(), 9, testLogger())

	// Before any cycle the next rung stands in, so the bar has a target.
	if got := o.Status().RampBatch; got != 1 {
		t.Fatalf("pre-cycle batch = %d, want the opener", got)
	}

	// A cycle is running a ten-song batch while the ladder has already
	// moved on to twenty.
	o.mu.Lock()
	o.genBusy = true
	o.batchCapNow = 10
	o.batchRenderedNow = 10
	o.rung = 2
	o.mu.Unlock()

	st := o.Status()
	if st.RampBatch != 10 {
		t.Errorf("batch reported as %d; want the 10 this cycle ran", st.RampBatch)
	}
	if st.BatchRendered != 10 {
		t.Errorf("rendered = %d, want 10", st.BatchRendered)
	}
}

// instrumentalSession is a session with no singing in it: the words to
// write are descriptions of each song, not lyrics.
func instrumentalSession() *session.Session {
	s := session.New()
	s.Vocal = false
	s.BasePrompt = "warm analog dub techno, deep, hypnotic"
	return s
}

// An instrumental batch gets a description written for each of its
// songs, the same way a vocal batch gets words. The phase used to turn
// every non-vocal session away at the door, three lines above the
// branch written to serve it, so this had never once run and every
// track of a batch went out under the one terse steering caption.
func TestInstrumentalBatchGetsItsDescriptionsWritten(t *testing.T) {
	srv := fakeWordsmith(t, 0)
	builder := prompting.NewBuilder(prompting.NewOllama(srv.URL, "", 0), testLogger())
	builder.ProbeAsync(context.Background())
	if !builder.AwaitHelper(context.Background(), 5*time.Second) {
		t.Fatal("the helper never became usable")
	}
	sess := instrumentalSession()
	o := heldOrchestrator(t, &pausableMock{}, builder, sess)
	// Past the opener, with a healthy buffer: the state in which a
	// whole batch is described rather than one emergency caption.
	o.mu.Lock()
	o.queue = append(o.queue, &engine.Track{
		Samples: make([]int16, 200*audio.SampleRate*audio.Channels),
	})
	o.mu.Unlock()

	if !builder.AwaitingInstrumentalCaptions(sess) {
		t.Fatal("the shelf should start bare")
	}
	o.wordsmithPhase(context.Background())
	if builder.AwaitingInstrumentalCaptions(sess) {
		t.Fatal("the instrumental batch came out of the wordsmith phase with nothing written")
	}
}

// And the cost of that, which was paid in graphics card time: with the
// shelf permanently bare, planning refused on every cycle, so the radio
// woke the engine, described nothing, planned nothing and hibernated
// again - once a minute, for as long as the buffer stayed healthy.
func TestInstrumentalCycleRendersInsteadOfWakingForNothing(t *testing.T) {
	srv := fakeWordsmith(t, 0)
	builder := prompting.NewBuilder(prompting.NewOllama(srv.URL, "", 0), testLogger())
	builder.ProbeAsync(context.Background())
	if !builder.AwaitHelper(context.Background(), 5*time.Second) {
		t.Fatal("the helper never became usable")
	}
	sess := instrumentalSession()
	eng := &pausableMock{}
	o := heldOrchestrator(t, eng, builder, sess)
	o.mu.Lock()
	o.queue = append(o.queue, &engine.Track{
		Samples: make([]int16, 200*audio.SampleRate*audio.Channels),
	})
	o.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	failures, oomStreak := 0, 0
	o.runCycle(ctx, &failures, &oomStreak)

	if eng.plans.Load() == 0 {
		t.Fatal("the cycle woke the engine and planned nothing")
	}
	if eng.renders.Load() == 0 {
		t.Fatal("the cycle woke the engine and rendered nothing")
	}
	// The render is in the book, hash and all: a copy of it on a phone
	// can be saved long after the file here is gone.
	o.mu.Lock()
	epoch := o.epoch
	o.mu.Unlock()
	made := o.Buffer.List(epoch)
	if len(made) == 0 {
		t.Fatal("the render left nothing on disk")
	}
	s, ok := o.Songbook.ByID(made[0].ID)
	if !ok || s.Hash == "" || s.Hash != made[0].Hash {
		t.Fatalf("the songbook has %+v (ok=%v) for the song on disk %+v", s, ok, made[0])
	}
}

// sleepingMock is an engine whose daemon is down, the way the real one
// reports it between cycles and while the writer has the card.
type sleepingMock struct{ phasedMock }

func (m *sleepingMock) Ready() bool   { return false }
func (m *sleepingMock) Phase() string { return "hibernated" }

// To the listener there is one engine and it makes songs: the phase
// shown while the writer works reads as making songs, never as a
// startup that has not begun. The log line for putting the daemon
// down says which it was, too - stopping the render daemon so the
// writer can have the card is not the engine going to sleep.
func TestTheWriterPhaseIsMakingSongs(t *testing.T) {
	sess := session.New()
	sess.Vocal = true
	o := New(testConfig(), &sleepingMock{}, prompting.NewBuilder(nil, testLogger()),
		session.NewStore(t.TempDir()), sess, &capturePlayer{}, testLogger())
	if got := o.currentPhase(); got != "starting engine" {
		t.Fatalf("with nothing made and nothing written the phase reads %q", got)
	}
	o.mu.Lock()
	o.wordsmithWantNow = 10
	o.mu.Unlock()
	if got := o.currentPhase(); got != "writing song words" {
		t.Errorf("during a wordsmith round the phase reads %q", got)
	}
	o.mu.Lock()
	o.sess.Vocal = false
	o.mu.Unlock()
	if got := o.currentPhase(); got != "writing song descriptions" {
		t.Errorf("during an instrumental round the phase reads %q", got)
	}

	for _, tc := range []struct {
		why   sleepReason
		event string
		msg   string
	}{
		{sleepForWriter, "render_unloaded", "render model unloaded while the words are written"},
		{sleepStocked, "engine_hibernated", "engine asleep: the store is stocked"},
		{sleepUntilDue, "engine_hibernated", "engine asleep until the next batch is due"},
		{sleepCooldown, "engine_hibernated", "engine asleep: resting after failures before the next try"},
		{sleepNoWork, "engine_hibernated", "engine asleep: no batch is due"},
		{sleepQuit, "engine_hibernated", "engine stopped with the radio"},
	} {
		event, msg := sleepLog(tc.why)
		if event != tc.event || msg != tc.msg {
			t.Errorf("sleepLog(%d) = %q, %q; want %q, %q", tc.why, event, msg, tc.event, tc.msg)
		}
	}
}

// The reason picked when no cycle is wanted follows the store and the
// ladder's clock, the same facts the buffer row shows - except that a
// cooldown after failures outranks both, because it is what is
// actually keeping the engine from a batch that is due. An engine put
// down on a stocked store is logged with what is in store and the
// mark it wakes below.
func TestSleepReasonFollowsTheStoreAndTheClock(t *testing.T) {
	cfg := testConfig()
	cfg.Buffer.ReserveSongs, cfg.Buffer.LowMinutes = 1, 0
	var logbuf lockedBuffer
	o := New(cfg, &phasedMock{}, prompting.NewBuilder(nil, testLogger()),
		session.NewStore(t.TempDir()), session.New(), &capturePlayer{},
		slog.New(slog.NewJSONHandler(&logbuf, nil)))
	o.Buffer = trackbuffer.New(t.TempDir(), 9, testLogger())
	// An empty store with no rung waiting: nothing to say but that
	// no batch is due.
	if got := o.sleepReasonNow(0); got != sleepNoWork {
		t.Errorf("empty store, no wait: %d", got)
	}
	o.mu.Lock()
	o.rungDoneAt, o.rungWait = time.Now(), time.Hour
	o.mu.Unlock()
	if got := o.sleepReasonNow(0); got != sleepUntilDue {
		t.Errorf("waiting out the clock: %d", got)
	}
	// A stocked store outranks the clock.
	skipWithoutFFmpeg(t)
	track := &engine.Track{Samples: make([]int16, 90*audio.SampleRate*audio.Channels), Spec: engine.Spec{Prompt: "p"}}
	if _, err := o.Buffer.PutTrack(context.Background(), 0, 1, track); err != nil {
		t.Fatal(err)
	}
	if got := o.sleepReasonNow(0); got != sleepStocked {
		t.Errorf("stocked store: %d", got)
	}
	// A cycle that gave up on failures rests the engine; that is the
	// reason, whatever the store and the clock say.
	o.coolDown(time.Minute)
	if got := o.sleepReasonNow(0); got != sleepCooldown {
		t.Errorf("cooling down after failures: %d", got)
	}
	o.coolDown(-time.Second)
	if got := o.sleepReasonNow(0); got != sleepStocked {
		t.Errorf("cooldown over: %d", got)
	}
	// The log line says what is in store and when the engine wakes.
	o.hibernateEngine(sleepStocked)
	if log := logbuf.String(); !strings.Contains(log, "engine asleep: 1 songs in store, 1m of music; wakes below 1") ||
		!strings.Contains(log, `"event":"engine_hibernated"`) || !strings.Contains(log, `"wake_below":1`) {
		t.Errorf("a stocked store's sleep was logged as:\n%s", log)
	}
}

// The words of the stocked-store line, for the log and for the rows
// that say the same thing.
func TestStockedMessage(t *testing.T) {
	for _, tc := range []struct {
		level     int
		seconds   float64
		wakeBelow int
		want      string
	}{
		{151, 8*3600 + 50*60, 93, "engine asleep: 151 songs in store, 8h50m of music; wakes below 93"},
		{12, 37 * 60, 8, "engine asleep: 12 songs in store, 37m of music; wakes below 8"},
		{3, 1.5, 3, "engine asleep: 3 songs in store, 1s of music; wakes below 3"},
		{1, 0, 1, "engine asleep: 1 songs in store, 0m of music; wakes below 1"},
	} {
		if got := stockedMessage(tc.level, tc.seconds, tc.wakeBelow); got != tc.want {
			t.Errorf("stockedMessage(%d, %v, %d) = %q, want %q", tc.level, tc.seconds, tc.wakeBelow, got, tc.want)
		}
	}
}

// The helper answers the health check at startup and every steer's
// refinement over the same connection it writes songs on. None of
// that is the words being written: with nothing to make, the phase
// stays what it was, rather than reading "writing song words" for half
// a minute after every ping beside a store that is full.
func TestAHelperCallIsNotTheWordsBeingWritten(t *testing.T) {
	srv := fakeWordsmith(t, 0)
	builder := prompting.NewBuilder(prompting.NewOllama(srv.URL, "", 0), testLogger())
	sess := session.New()
	sess.Vocal = true
	o := New(testConfig(), &sleepingMock{}, builder,
		session.NewStore(t.TempDir()), sess, &capturePlayer{}, testLogger())
	builder.ProbeAsync(context.Background())
	if !builder.AwaitHelper(context.Background(), 5*time.Second) {
		t.Fatal("the helper never became usable")
	}
	// The health check has just been answered; no round is running.
	if builder.WriterWorking() {
		t.Fatal("the health check counts as the writer working")
	}
	if got := o.currentPhase(); got != "starting engine" {
		t.Errorf("after the health check, with nothing made, the phase reads %q", got)
	}
	o.mu.Lock()
	o.genCount = 1
	o.mu.Unlock()
	if got := o.currentPhase(); got != "playing" {
		t.Errorf("after the health check, with music made, the phase reads %q", got)
	}
	if st := o.Status(); st.Phase != "playing" || st.WordsmithWant != 0 {
		t.Errorf("status reads phase %q with %d words wanted", st.Phase, st.WordsmithWant)
	}
}

// dryWordsmith is an Ollama stand-in that answers the health check and
// fails everything else: a helper that is usable and has no words.
func dryWordsmith(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/tags", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"models": []map[string]string{{"name": "test-model"}},
		})
	})
	mux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if len(req.Messages) > 1 && strings.Contains(req.Messages[1].Content, "Reply with exactly") {
			json.NewEncoder(w).Encode(map[string]any{
				"message": map[string]string{"role": "assistant", "content": "OK"},
			})
			return
		}
		http.Error(w, "no words today", http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// A cycle that runs out of written words stops the render daemon so
// the writer can have the card. That is not the engine going to sleep
// - the radio is still making songs - and the log says which it was:
// the daemon was put down once, for the writer, and nothing on that
// path claims the engine is asleep.
func TestHandingTheCardToTheWriterIsNotLoggedAsSleep(t *testing.T) {
	srv := dryWordsmith(t)
	builder := prompting.NewBuilder(prompting.NewOllama(srv.URL, "", 0), testLogger())
	builder.ProbeAsync(context.Background())
	if !builder.AwaitHelper(context.Background(), 5*time.Second) {
		t.Fatal("the helper never became usable")
	}
	sess := session.New()
	sess.Vocal = true
	sess.LyricsGenerator = "smoothbrain"
	eng := &pausableMock{}
	var logbuf lockedBuffer
	o := New(testConfig(), eng, builder, session.NewStore(t.TempDir()), sess, &capturePlayer{},
		slog.New(slog.NewJSONHandler(&logbuf, nil)))
	o.Buffer = trackbuffer.New(t.TempDir(), 9, testLogger())
	// Past the opener: every song waits for the writer.
	o.mu.Lock()
	o.rung = 1
	o.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	failures, oomStreak := 0, 0
	o.runCycle(ctx, &failures, &oomStreak)

	if got := eng.plans.Load(); got != 0 {
		t.Fatalf("the cycle planned %d songs past a writer with no words", got)
	}
	if got := eng.hibernated.Load(); got != 1 {
		t.Fatalf("the daemon was put down %d times, want once for the writer", got)
	}
	log := logbuf.String()
	if !strings.Contains(log, `"event":"render_unloaded"`) ||
		!strings.Contains(log, "render model unloaded while the words are written") {
		t.Errorf("the writer's path did not log the render unload:\n%s", log)
	}
	if strings.Contains(log, `"event":"engine_hibernated"`) || strings.Contains(log, "asleep") {
		t.Errorf("the writer's path logged the engine asleep:\n%s", log)
	}
	if o.Status().EngineAwake {
		t.Error("the daemon is down for the writer, and the status says the engine is awake")
	}
}

// The status says whether the radio is keeping the engine up for work
// of its own, so a daemon still loading its models reads as waking
// for a batch and not as asleep.
func TestEngineAwakeFollowsTheCycle(t *testing.T) {
	o := New(testConfig(), &phasedMock{}, prompting.NewBuilder(nil, testLogger()),
		session.NewStore(t.TempDir()), session.New(), &capturePlayer{}, testLogger())
	if o.Status().EngineAwake {
		t.Fatal("an engine nobody has woken reads awake")
	}
	o.setEngineActive(true)
	if !o.Status().EngineAwake {
		t.Fatal("an engine woken for a batch reads asleep")
	}
	o.hibernateEngine(sleepStocked)
	if o.Status().EngineAwake {
		t.Fatal("an engine put down reads awake")
	}
}
