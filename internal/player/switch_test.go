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
	"iar/internal/trackbuffer"
)

// Stopping this machine's player silences its speakers and stops it
// taking songs from the store - and nothing else: the store fills for
// the other listeners exactly as before, and the song that was playing
// is the song that plays when the player comes back on.
func TestStopHoldsTheSpeakersWhileTheRadioRunsOn(t *testing.T) {
	o, pl := newTestOrchestrator(t, enginetest.NewMock(), session.New())

	waitFor(t, 15*time.Second, "music playing before the stop", func() bool {
		return pl.written() > 0 && o.Status().TrackTitle != ""
	})

	if ack := o.Stop(); !strings.Contains(ack, "stopped") {
		t.Fatalf("stop ack = %q", ack)
	}
	if st := o.Status(); !st.Stopped || st.State != "stopped" {
		t.Fatalf("status does not report the stop: %+v", st.State)
	}

	// The song that was playing is still the song that is playing: the
	// mixer has not moved on, so nothing was spent.
	before := o.Status().TrackTitle
	time.Sleep(1500 * time.Millisecond)
	if after := o.Status().TrackTitle; after != before {
		t.Fatalf("the stop consumed a song: %q became %q", before, after)
	}

	// Output keeps flowing - silence, not a stalled device.
	fed := pl.written()
	time.Sleep(500 * time.Millisecond)
	if pl.written() <= fed {
		t.Fatal("the audio device stopped being fed while stopped")
	}

	if ack := o.Play(); !strings.Contains(ack, "playing") {
		t.Fatalf("play ack = %q", ack)
	}
	if o.Status().Stopped {
		t.Fatal("status still reports the stop after play")
	}
}

// A stopped player takes nothing from the store: the level stays where
// it was for the other listeners, and play picks up where it left off.
func TestAStoppedPlayerTakesNothingFromTheStore(t *testing.T) {
	skipWithoutFFmpeg(t)
	o, _ := idOrchestrator(t)
	for seq := 1; seq <= 3; seq++ {
		renderSong(t, o, seq, "t-1790000000000-006"+string(rune('0'+seq)))
	}
	o.Stop()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go o.feedLoop(ctx)
	time.Sleep(1200 * time.Millisecond)
	if n, _ := o.Buffer.Level(0); n != 3 {
		t.Fatalf("a stopped player took songs: level %d, want 3", n)
	}
	o.Play()
	waitFor(t, 20*time.Second, "songs taken once playing", func() bool {
		n, _ := o.Buffer.Level(0)
		return n == 1
	})
}

// A machine started as the station has its player off from the first
// moment: the generator runs and the remote serves, and nothing comes
// out of its speakers until it is asked to.
func TestARemoteStationStartsWithItsPlayerOff(t *testing.T) {
	skipWithoutFFmpeg(t)
	eng := enginetest.NewMock()
	store := session.NewStore(t.TempDir())
	o := New(testConfig(), eng, prompting.NewBuilder(nil, testLogger()), store, session.New(), &capturePlayer{}, testLogger())
	o.Idle = true
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	o.Start(ctx)
	t.Cleanup(func() { o.Close() })

	if !o.Status().Stopped {
		t.Fatal("the station's player started switched on")
	}
	waitFor(t, 15*time.Second, "songs generated for the store", func() bool {
		return o.Status().StoreLevel > 0
	})
	if st := o.Status(); st.TrackTitle != "" || st.Queued != 0 {
		t.Fatalf("a stopped station took from the store: playing %q, %d queued", st.TrackTitle, st.Queued)
	}
	if ack := o.Play(); !strings.Contains(ack, "playing") {
		t.Fatalf("play ack = %q", ack)
	}
	waitFor(t, 15*time.Second, "music once played", func() bool {
		return o.Status().TrackTitle != ""
	})
}

// written reports how many bytes have reached the audio device.
func (p *capturePlayer) written() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.buf)
}

// pausableMock is an engine whose every plan and render succeeds, so a
// whole batch runs. phasedMock deliberately stalls after its first
// plan; this one does not, because what is under test is what a cycle
// keeps doing.
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

// heldOrchestrator builds a phased orchestrator on the ladder's ten-song
// rung, so there is a whole batch for a cycle to work through.
func heldOrchestrator(t *testing.T, eng *pausableMock, builder *prompting.Builder, sess *session.Session) *Orchestrator {
	t.Helper()
	cfg := testConfig()
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
