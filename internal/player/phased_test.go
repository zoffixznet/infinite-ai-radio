package player

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"iar/internal/audio"
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
	cfg.Buffer.Phased = true
	// A store that fills at one song is full after the opener, so
	// exactly one cycle runs and then hibernates.
	cfg.Buffer.Songs = 1
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
	cfg.Buffer.Phased = true
	sess := session.New()
	builder := prompting.NewBuilder(nil, testLogger())
	o := New(cfg, nil, builder, session.NewStore(t.TempDir()), sess, &capturePlayer{}, testLogger())
	o.Buffer = trackbuffer.New(t.TempDir(), 9, testLogger())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	o.Start(ctx)
	t.Cleanup(func() { o.Close() })

	if o.phasedEnabled() {
		t.Fatal("an orchestrator with no engine must not report a phased pipeline")
	}
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
	cfg.Buffer.Phased = true
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
	cfg.Buffer.Phased = true
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

// The ladder climbs on the clock. One opener; the ten-song batch
// straight after it; then every rung waits as long as its music runs
// before the next, less what listeners skipped; a rung that was cut
// short stays open; and a rung never makes more than the room left in
// the store.
func TestTheLadderClimbsOnTime(t *testing.T) {
	cfg := testConfig()
	cfg.Buffer.Phased = true
	cfg.Buffer.Songs = 25
	o := New(cfg, &phasedMock{}, prompting.NewBuilder(nil, testLogger()),
		session.NewStore(t.TempDir()), session.New(), &capturePlayer{}, testLogger())
	o.Buffer = trackbuffer.New(t.TempDir(), 9, testLogger())
	made := func(songs int, seconds float64) {
		o.mu.Lock()
		o.rungMade += songs
		o.rungSeconds += float64(songs) * seconds
		o.mu.Unlock()
		o.completeRung(0)
	}

	if got := o.batchFor(0); got != 1 {
		t.Fatalf("the first batch = %d, want the opener alone", got)
	}
	// The opener waits for nothing: the ten-song batch follows it.
	made(1, 200)
	if left, next := o.rungWaitLeft(), o.batchFor(0); left != 0 || next != 10 {
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
	if next := o.batchFor(0); next != 17 {
		t.Fatalf("after 3 of 20: next batch %d, want the remaining 17", next)
	}
	made(17, 200)
	o.ReportSkipped(20 * 200)
	// The 40-rung is capped at the room left in a store of 25.
	if next := o.batchFor(0); next != 25 {
		t.Fatalf("the 40-rung's batch = %d, want the store's room of 25", next)
	}
}

// A full store is the off switch: no rung is due, however long the
// wait has been over. Taking a song makes room for exactly that much.
func TestAFullStoreRunsNoCycle(t *testing.T) {
	skipWithoutFFmpeg(t)
	o, _ := idOrchestrator(t)
	o.cfg.Buffer.Songs = 3
	o.mu.Lock()
	o.rung = len(ladder) - 1
	o.mu.Unlock()
	for seq := 1; seq <= 3; seq++ {
		renderSong(t, o, seq, "t-1790000000000-005"+string(rune('0'+seq)))
	}
	if o.batchFor(0) != 0 || o.wantCycle(0) {
		t.Fatal("a full store still wants a cycle")
	}
	o.Buffer.Take(0, "e00000000-00000001")
	if next := o.batchFor(0); next != 1 || !o.wantCycle(0) {
		t.Fatalf("one song taken: next batch %d, want 1 and a cycle due", next)
	}
}

// The full ladder, rung by rung, as the store empties.
func TestBatchLadder(t *testing.T) {
	cfg := testConfig()
	cfg.Buffer.Phased = true
	cfg.Buffer.Songs = 72
	o := New(cfg, &phasedMock{}, prompting.NewBuilder(nil, testLogger()),
		session.NewStore(t.TempDir()), session.New(), &capturePlayer{}, testLogger())
	for _, tc := range []struct{ rung, level, want int }{
		{0, 0, 1},   // the opener: sound as fast as possible
		{1, 1, 10},  // the audition batch
		{2, 11, 20}, // then 20...
		{3, 31, 40}, // ...and 40
		{4, 0, 72},  // 80 is the ceiling, capped at the room in the store
		{4, 71, 1},  // a top-up makes just what was taken
		{4, 72, 0},  // and a full store makes nothing
	} {
		o.mu.Lock()
		o.rung, o.rungMade = tc.rung, 0
		got := o.batchForLocked(tc.level)
		o.mu.Unlock()
		if got != tc.want {
			t.Errorf("rung %d at level %d makes %d, want %d", tc.rung, tc.level, got, tc.want)
		}
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
	cfg.Buffer.Phased = true
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
