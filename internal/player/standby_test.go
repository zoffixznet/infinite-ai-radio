package player

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"iar/internal/audio"
	"iar/internal/engine"
	"iar/internal/engine/enginetest"
	"iar/internal/prompting"
	"iar/internal/session"
	"iar/internal/state"
	"iar/internal/trackbuffer"
)

// The point of the hold is that the buffer is still there when the
// listener comes back: a held radio must take nothing out of it and
// put nothing into it. Mute was never enough - it silences the room
// while the mixer keeps eating songs and the card keeps making them.
func TestStandbyStopsConsumingAndGenerating(t *testing.T) {
	o, pl := newTestOrchestrator(t, enginetest.NewMock(), session.New())

	waitFor(t, 15*time.Second, "music playing before the hold", func() bool {
		return pl.written() > 0 && o.Status().TrackTitle != ""
	})

	if ack := o.ToggleStandby(); !strings.Contains(ack, "standby") {
		t.Fatalf("standby ack = %q", ack)
	}
	if !o.Status().Standby {
		t.Fatal("status does not report the hold")
	}
	if o.wantGeneration() {
		t.Fatal("a held radio still wants to generate")
	}

	// The song that was playing is still the song that is playing: the
	// mixer has not moved on, so nothing was spent.
	before := o.Status().TrackTitle
	time.Sleep(1500 * time.Millisecond)
	if after := o.Status().TrackTitle; after != before {
		t.Fatalf("the hold consumed the buffer: %q became %q", before, after)
	}

	// Output keeps flowing - silence, not a stalled device.
	fed := pl.written()
	time.Sleep(500 * time.Millisecond)
	if pl.written() <= fed {
		t.Fatal("the audio device stopped being fed while held")
	}

	if ack := o.ToggleStandby(); !strings.Contains(ack, "awake") {
		t.Fatalf("wake ack = %q", ack)
	}
	if o.Status().Standby {
		t.Fatal("status still reports the hold after waking")
	}
}

// A machine is left on for days; a restart must not quietly put it
// back to work.
func TestStandbySurvivesARestart(t *testing.T) {
	dir, err := state.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if dir.Standby() {
		t.Fatal("a fresh state directory reported a hold")
	}
	if err := dir.SetStandby(true); err != nil {
		t.Fatal(err)
	}
	if !dir.Standby() {
		t.Fatal("the hold was not recorded")
	}

	cfg := testConfig()
	o := New(cfg, enginetest.NewMock(), prompting.NewBuilder(nil, testLogger()),
		session.NewStore(t.TempDir()), session.New(), &capturePlayer{}, testLogger())
	o.StateDir = &dir
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	o.Start(ctx)
	t.Cleanup(func() { o.Close() })

	if !o.Status().Standby {
		t.Fatal("a radio started from a recorded hold began making music anyway")
	}
	o.ToggleStandby()
	if dir.Standby() {
		t.Fatal("waking did not clear the recorded hold")
	}
}

// written reports how many bytes have reached the audio device.
func (p *capturePlayer) written() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.buf)
}

// pausableMock is an engine whose every plan and render succeeds, so a
// whole batch runs and a test can hold the radio in the middle of one.
// phasedMock deliberately stalls after its first plan; this one does
// not, because the bug under test is about what a cycle keeps doing.
type pausableMock struct {
	plans      atomic.Int32
	renders    atomic.Int32
	hibernated atomic.Int32
	wakes      atomic.Int32
	active     atomic.Bool
	work       time.Duration
}

func (m *pausableMock) Name() string { return "pausable-mock" }
func (m *pausableMock) Ready() bool  { return true }

func (m *pausableMock) Generate(ctx context.Context, spec engine.Spec) (*engine.Track, error) {
	panic("phased pipeline must not call the fused Generate")
}

func (m *pausableMock) Plan(ctx context.Context, spec engine.Spec) (*engine.Plan, error) {
	m.plans.Add(1)
	if m.work > 0 {
		time.Sleep(m.work)
	}
	return &engine.Plan{
		Spec: spec, Caption: "mock caption", Lyrics: "[Verse 1]\nmock words",
		AudioCodes: "mock-codes", Seconds: 1,
	}, nil
}

func (m *pausableMock) Render(ctx context.Context, plan *engine.Plan) (*engine.Track, error) {
	m.renders.Add(1)
	if m.work > 0 {
		time.Sleep(m.work)
	}
	return &engine.Track{
		Samples: make([]int16, audio.SampleRate*audio.Channels),
		Spec:    plan.Spec, Prompt: plan.Caption, Lyrics: plan.Lyrics,
	}, nil
}

func (m *pausableMock) SetEngineActive(on bool) {
	if on && !m.active.Swap(on) {
		m.wakes.Add(1)
		return
	}
	m.active.Store(on)
}
func (m *pausableMock) HibernateEngine() bool { m.hibernated.Add(1); return true }

// heldOrchestrator builds a phased orchestrator on the ramp's 20-song
// rung, so there is a batch long enough to be held in the middle of.
func heldOrchestrator(t *testing.T, eng *pausableMock, builder *prompting.Builder, sess *session.Session) *Orchestrator {
	t.Helper()
	cfg := testConfig()
	cfg.Buffer.Phased = true
	if builder == nil {
		builder = prompting.NewBuilder(nil, testLogger())
	}
	o := New(cfg, eng, builder, session.NewStore(t.TempDir()), sess, &capturePlayer{}, testLogger())
	o.Buffer = trackbuffer.New(t.TempDir(), 9, testLogger())
	o.mu.Lock()
	o.rung = 1
	o.mu.Unlock()
	return o
}

// A hold in the middle of a batch stops it at the next song boundary -
// and stops it for real, with the card handed back. The radio used to
// check the hold once, on its way into a cycle, so a batch of eighty
// songs begun a second before the button was pressed ran to the end:
// twenty minutes of words, then the engine woken to render all of it.
func TestHoldPausesTheBatchBetweenSongs(t *testing.T) {
	eng := &pausableMock{work: 20 * time.Millisecond}
	o := heldOrchestrator(t, eng, nil, session.New())
	// The cycle is driven directly, with no mixer behind it: playback
	// eats the buffer as fast as this mock fills it, and what is under
	// test is what the CYCLE does with a hold, not what playback does.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan struct{})
	failures, oomStreak := 0, 0
	go func() { defer close(done); o.runCycle(ctx, &failures, &oomStreak) }()

	waitFor(t, 20*time.Second, "the batch well under way", func() bool {
		return eng.renders.Load() >= 2
	})
	epoch := o.syncPhasedState()
	bankedBefore := len(o.Buffer.List(epoch))
	plansBefore, rendersBefore := eng.plans.Load(), eng.renders.Load()
	o.ToggleStandby()

	// One unit may already be in flight; nothing after it may start.
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the held cycle never returned; it is still working through its batch")
	}
	if eng.hibernated.Load() < 1 || eng.active.Load() {
		t.Fatal("the held cycle ended without handing the card back")
	}
	time.Sleep(200 * time.Millisecond)
	if grew := eng.plans.Load() - plansBefore; grew > 1 {
		t.Fatalf("the hold let %d more songs be planned; at most the one in flight may finish", grew)
	}
	if grew := eng.renders.Load() - rendersBefore; grew > 1 {
		t.Fatalf("the hold let %d more songs be rendered; at most the one in flight may finish", grew)
	}
	// Pausing is not discarding: what the batch had already banked is
	// still on disk, waiting for the listener to come back.
	if after := len(o.Buffer.List(epoch)); after < bankedBefore {
		t.Fatalf("the hold cost the buffer songs: %d banked before, %d after", bankedBefore, after)
	}
}

// A cycle that ended staying warm, and then a hold: nothing enters
// runCycle again, so the defer that hibernates never runs and the
// daemon sits on the card for good. The loop hands it back itself.
func TestHoldHandsBackAWarmEngine(t *testing.T) {
	eng := &pausableMock{}
	o := heldOrchestrator(t, eng, nil, session.New())
	o.setEngineActive(true)
	o.ToggleStandby()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	o.Start(ctx)
	t.Cleanup(func() { o.Close() })

	waitFor(t, 15*time.Second, "the held radio to hand the card back", func() bool {
		return eng.hibernated.Load() >= 1
	})
	if eng.active.Load() {
		t.Fatal("a held radio is still holding the engine active")
	}
}

// fakeWordsmith is an Ollama stand-in that writes a sheet slowly enough
// for a hold to land in the middle of a batch of them.
func fakeWordsmith(t *testing.T, per time.Duration) *httptest.Server {
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
			Format json.RawMessage `json:"format"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		reply := "[Verse]\nsteel in the water\n\n[Chorus]\nhold the line"
		switch {
		case len(req.Messages) > 1 && strings.Contains(req.Messages[1].Content, "Reply with exactly"):
			reply = "OK"
		case len(req.Format) > 0:
			reply = `{"title":"Steel In The Water","subtitle":"nu-metal, driving"}`
		default:
			time.Sleep(per)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"message": map[string]string{"role": "assistant", "content": reply},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// The reported bug, at its source: the hold arrives while the writer
// owns the freed card. Writing a deep batch's words is tens of minutes,
// and the radio used to finish every sheet and then wake the engine to
// render the lot - the one thing the button exists to prevent.
func TestHoldDuringTheWordsmithNeverWakesTheEngine(t *testing.T) {
	srv := fakeWordsmith(t, 30*time.Millisecond)
	builder := prompting.NewBuilder(prompting.NewOllama(srv.URL, "", 0), testLogger())
	builder.ProbeAsync(context.Background())
	if !builder.AwaitHelper(context.Background(), 5*time.Second) {
		t.Fatal("the lyric helper never became usable")
	}
	sess := session.New()
	sess.Vocal = true
	sess.BasePrompt = "nu-metal, aggressive, heavy groove"
	sess.LyricsGenerator = "smoothbrain"

	eng := &pausableMock{}
	o := heldOrchestrator(t, eng, builder, sess)
	// Deep enough that the writer wants a whole batch rather than the
	// single emergency sheet a starving buffer asks for.
	o.mu.Lock()
	o.queue = append(o.queue, &engine.Track{
		Samples: make([]int16, 200*audio.SampleRate*audio.Channels),
	})
	o.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan struct{})
	failures, oomStreak := 0, 0
	go func() { defer close(done); o.runCycle(ctx, &failures, &oomStreak) }()

	waitFor(t, 20*time.Second, "the writer some way into its batch", func() bool {
		return o.Status().WordsmithWrote >= 2
	})
	wroteBefore := o.Status().WordsmithWrote
	o.ToggleStandby()

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the held cycle never returned; it is still writing the batch")
	}
	if n := eng.wakes.Load(); n != 0 {
		t.Fatalf("a held radio woke the engine %d time(s) after the writer finished", n)
	}
	if n := eng.plans.Load(); n != 0 {
		t.Fatalf("a held radio planned %d song(s)", n)
	}
	// Whatever was written stays written: the hold pauses the batch,
	// it does not throw the shelf away.
	if builder.AwaitingLyrics(sess) {
		t.Fatalf("the hold lost the %d sheets already written", wroteBefore)
	}
}
