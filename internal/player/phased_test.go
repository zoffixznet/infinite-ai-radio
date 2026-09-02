package player

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"iar/internal/audio"
	"iar/internal/engine"
	"iar/internal/engine/enginetest"
	"iar/internal/library"
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
	// A zero refill trigger keeps the mock's single 1-second track
	// "enough", so exactly one cycle runs and then hibernates.
	cfg.Buffer.RenderLowMinutes = 0
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
		return o.playedInEpoch >= 1 && o.lastGood != nil
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

// A configured disk buffer is not a phased run: with --engine noise
// there is no engine at all, and the phased export path used to walk
// straight into it. `mp3 1` on a noise session crashed the whole radio.
func TestExportWithADiskBufferButNoEngineDoesNotCrash(t *testing.T) {
	cfg := testConfig()
	cfg.Buffer.Phased = true
	sess := session.New()
	sess.Mode = session.ModeNoise
	sess.NoiseColor = "white"
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
	dir := t.TempDir()
	if ack := o.Export(1, "", dir); !strings.Contains(ack, "export") {
		t.Fatalf("export not accepted: %q", ack)
	}
	// The export runs in the background; the crash was immediate, so
	// surviving a moment of it is the assertion.
	waitFor(t, 30*time.Second, "the export to finish", func() bool {
		o.mu.Lock()
		defer o.mu.Unlock()
		return o.exporting == ""
	})
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Error("noise export produced no file")
	}
}

// A restart is not a steer: a buffer left by a previous run for the
// same steering context is adopted - its epoch becomes this run's -
// instead of being invisible to a process whose epoch counter starts
// at zero. Only a prompt, steer or language change resets generation.
func TestRestartAdoptsThePreviousRunsBuffer(t *testing.T) {
	dir := t.TempDir()
	sess := session.New()
	key := library.Key(sess)

	// The previous run: context recorded, songs stored under epoch 3.
	prev := trackbuffer.New(dir, 0, testLogger())
	prev.SetContext(key)
	for seq := 1; seq <= 3; seq++ {
		track := &engine.Track{
			Lyrics:  fmt.Sprintf("[Verse]\nsong %d", seq),
			Samples: make([]int16, 9600),
		}
		if err := prev.PutTrack(context.Background(), 3, seq, "", track); err != nil {
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
	if err := prev.PutTrack(context.Background(), 7, 1, "", track); err != nil {
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

// The deep batch unlocks on played songs that carried written words,
// not on the engine-worded openers of a cold start.
func TestDeepBatchWaitsForProperSongs(t *testing.T) {
	cfg := testConfig()
	cfg.Buffer.Phased = true
	o := New(cfg, enginetest.NewMock(), prompting.NewBuilder(nil, testLogger()), session.NewStore(t.TempDir()), session.New(), &capturePlayer{}, testLogger())

	o.mu.Lock()
	o.playedInEpoch = 8 // eight songs heard...
	o.properPlayedInEpoch = 2
	o.mu.Unlock()
	if _, _, cap := o.cycleTargets(0); cap != rampSmallBatch {
		t.Fatalf("batchCap = %d; two proper plays must not unlock the deep batch", cap)
	}
	o.mu.Lock()
	o.properPlayedInEpoch = rampStableTracks
	o.mu.Unlock()
	if _, _, cap := o.cycleTargets(0); cap != 0 {
		t.Fatalf("batchCap = %d; five proper plays unlock full depth", cap)
	}
}
