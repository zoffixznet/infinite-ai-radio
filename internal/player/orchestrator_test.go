package player

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"bgm/internal/audio"
	"bgm/internal/engine"
	"bgm/internal/library"

	"bgm/internal/config"
	"bgm/internal/engine/enginetest"
	"bgm/internal/prompting"
	"bgm/internal/session"
)

// capturePlayer records everything written to it, with light pacing so the
// pump does not spin.
type capturePlayer struct {
	mu  sync.Mutex
	buf []byte
}

func (p *capturePlayer) Write(b []byte) (int, error) {
	p.mu.Lock()
	p.buf = append(p.buf, b...)
	p.mu.Unlock()
	time.Sleep(time.Millisecond)
	return len(b), nil
}
func (p *capturePlayer) Close() error { return nil }
func (p *capturePlayer) Name() string { return "capture" }

func (p *capturePlayer) bytes() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.buf)
}

// nonSilent reports whether the tail of captured audio has signal.
func (p *capturePlayer) nonSilentTail() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.buf) < 4000 {
		return false
	}
	tail := p.buf[len(p.buf)-4000:]
	for _, b := range tail {
		if b != 0 {
			return true
		}
	}
	return false
}

func testConfig() config.Config {
	cfg := config.Default()
	cfg.TrackSeconds = 2
	cfg.CrossfadeSeconds = 0.5
	cfg.BufferTracks = 2
	return cfg
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestOrchestrator(t *testing.T, eng *enginetest.Mock, sess *session.Session) (*Orchestrator, *capturePlayer) {
	t.Helper()
	pl := &capturePlayer{}
	store := session.NewStore(t.TempDir())
	builder := prompting.NewBuilder(nil, testLogger())
	o := New(testConfig(), eng, builder, store, sess, pl, testLogger())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	o.Start(ctx)
	t.Cleanup(func() { o.Close() })
	return o, pl
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestStreamStartsSilentThenPlaysGeneratedMusic(t *testing.T) {
	eng := enginetest.NewMock()
	o, pl := newTestOrchestrator(t, eng, session.New())

	// The stream starts immediately, but with silence (no noise bed).
	waitFor(t, 3*time.Second, "output flowing", func() bool { return pl.bytes() > 0 })
	if src := o.Status().Source; !strings.Contains(src, "silence") {
		t.Fatalf("startup source = %q; want silence", src)
	}

	// Generated music takes over and becomes audible.
	waitFor(t, 10*time.Second, "playing state", func() bool {
		st := o.Status()
		return st.State == "playing" && st.GenCount >= 1
	})
	waitFor(t, 10*time.Second, "audible output", pl.nonSilentTail)
	st := o.Status()
	if st.Queued > testConfig().BufferTracks {
		t.Fatalf("queue overfilled: %d", st.Queued)
	}
}

func TestSteeringDropsQueueAndReachesNextSpec(t *testing.T) {
	eng := enginetest.NewMock()
	o, _ := newTestOrchestrator(t, eng, session.New())
	waitFor(t, 10*time.Second, "buffer filled", func() bool { return o.Status().Queued >= 1 })

	ack := o.Steer("make it more energetic")
	if !strings.Contains(ack, "energetic") {
		t.Fatalf("ack = %q", ack)
	}
	if got := o.Status().Queued; got != 0 {
		t.Fatalf("queue after steering = %d; want 0", got)
	}
	waitFor(t, 10*time.Second, "new spec generated", func() bool {
		specs := eng.Specs()
		last := specs[len(specs)-1]
		return strings.Contains(last.Prompt, "energetic")
	})
}

func TestNoiseModeSwitch(t *testing.T) {
	eng := enginetest.NewMock()
	o, _ := newTestOrchestrator(t, eng, session.New())
	ack := o.Steer("generate brown noise")
	if !strings.Contains(ack, "brown noise") {
		t.Fatalf("ack = %q", ack)
	}
	waitFor(t, 5*time.Second, "noise state", func() bool { return o.Status().State == "noise" })
	waitFor(t, 5*time.Second, "brown noise source", func() bool {
		return strings.Contains(o.Status().Source, "brown noise")
	})
}

func TestEngineNotReadyStaysSilentWithProgress(t *testing.T) {
	eng := enginetest.NewMock()
	eng.SetReady(false)
	o, pl := newTestOrchestrator(t, eng, session.New())
	waitFor(t, 3*time.Second, "output flows", func() bool { return pl.bytes() > 0 })
	st := o.Status()
	if !strings.Contains(st.Source, "silence") {
		t.Fatalf("source = %q; want silence while the engine loads", st.Source)
	}
	if st.GenCount != 0 {
		t.Fatal("generated without a ready engine")
	}
	waitFor(t, 3*time.Second, "startup phase reported", func() bool {
		return o.Status().Phase == "starting engine"
	})
	// Engine comes up; music follows without any noise in between.
	eng.SetReady(true)
	waitFor(t, 10*time.Second, "recovery to music", func() bool { return o.Status().State == "playing" })
}

func TestNoEngineFallsBackToBedWithProminentError(t *testing.T) {
	pl := &capturePlayer{}
	store := session.NewStore(t.TempDir())
	builder := prompting.NewBuilder(nil, testLogger())
	o := New(testConfig(), nil, builder, store, session.New(), pl, testLogger())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	o.Start(ctx)
	t.Cleanup(func() { o.Close() })

	var errEvent string
	waitFor(t, 3*time.Second, "prominent error event", func() bool {
		select {
		case ev := <-o.Events():
			if strings.Contains(ev.Text, "bgm setup") {
				errEvent = ev.Text
				return true
			}
		default:
		}
		return false
	})
	if !strings.Contains(strings.ToUpper(errEvent), "UNAVAILABLE") {
		t.Fatalf("error event not prominent: %q", errEvent)
	}
	waitFor(t, 3*time.Second, "noise bed playing", func() bool {
		return strings.Contains(o.Status().Source, "noise bed")
	})
}

func TestVolumeAndPause(t *testing.T) {
	eng := enginetest.NewMock()
	o, _ := newTestOrchestrator(t, eng, session.New())
	if ack := o.SetVolume(150); !strings.Contains(ack, "100") {
		t.Fatalf("volume clamp ack = %q", ack)
	}
	if ack := o.TogglePause(); !strings.Contains(ack, "paused") {
		t.Fatalf("pause ack = %q", ack)
	}
	if !o.Status().Paused {
		t.Fatal("not paused")
	}
	if ack := o.Resume(); ack != "resumed" {
		t.Fatalf("resume ack = %q", ack)
	}
}

func TestSessionNamingAndReload(t *testing.T) {
	eng := enginetest.NewMock()
	pl := &capturePlayer{}
	store := session.NewStore(t.TempDir())
	builder := prompting.NewBuilder(nil, testLogger())
	o := New(testConfig(), eng, builder, store, session.New(), pl, testLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	o.Start(ctx)
	defer o.Close()

	o.Steer("calmer")
	ack := o.NameSession("evening chill")
	if !strings.Contains(ack, "evening-chill") {
		t.Fatalf("ack = %q", ack)
	}
	names := o.SessionNames()
	found := false
	for _, n := range names {
		if n == "evening-chill" {
			found = true
		}
	}
	if !found {
		t.Fatalf("named session not listed: %v", names)
	}
	ack = o.LoadSession("evening-chill")
	if !strings.Contains(ack, "evening-chill") {
		t.Fatalf("load ack = %q", ack)
	}
	// Loading a preset seeds a new session.
	ack = o.LoadPreset("sleep")
	if !strings.Contains(ack, "sleep") {
		t.Fatalf("preset ack = %q", ack)
	}
}

func TestSaveSnippetDuringPlayback(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	eng := enginetest.NewMock()
	o, _ := newTestOrchestrator(t, eng, session.New())
	// A directory that does not exist yet: save must create it.
	o.SnippetsDir = t.TempDir() + "/music/bgm-snippets"

	// Nothing to save before a generated track plays.
	if ack := o.SaveSnippet(""); !strings.Contains(ack, "nothing to save") {
		t.Fatalf("early save ack = %q", ack)
	}
	waitFor(t, 10*time.Second, "playing", func() bool { return o.Status().State == "playing" })

	ack := o.SaveSnippet("")
	if !strings.Contains(ack, "saving this track") {
		t.Fatalf("save ack = %q", ack)
	}
	var files []os.DirEntry
	waitFor(t, 15*time.Second, "snippet file", func() bool {
		files, _ = os.ReadDir(o.SnippetsDir)
		return len(files) == 1
	})
	if !strings.HasSuffix(files[0].Name(), ".mp3") {
		t.Fatalf("snippet name = %q", files[0].Name())
	}
	if u := o.Status().Underruns; u != 0 {
		t.Fatalf("underruns while saving: %d", u)
	}
	// prev works after two distinct tracks; here at least verify the
	// unknown-prev message before one exists.
	o2, _ := newTestOrchestrator(t, enginetest.NewMock(), session.New())
	o2.SnippetsDir = t.TempDir()
	if ack := o2.SaveSnippet("prev"); !strings.Contains(ack, "no previous track") {
		t.Fatalf("prev ack = %q", ack)
	}
}

func TestNewSessionFromPromptCommand(t *testing.T) {
	eng := enginetest.NewMock()
	o, _ := newTestOrchestrator(t, eng, session.New())
	ack := o.NewSession("dreamy jazz with vocals about rain")
	if !strings.Contains(ack, "new session") || !strings.Contains(ack, "vocals") {
		t.Fatalf("ack = %q", ack)
	}
	waitFor(t, 10*time.Second, "vocal spec generated", func() bool {
		specs := eng.Specs()
		if len(specs) == 0 {
			return false
		}
		last := specs[len(specs)-1]
		return last.Vocal() && strings.Contains(last.SampleQuery+last.Prompt, "dreamy jazz")
	})
	// The fresh session is persisted.
	found := false
	for _, n := range o.SessionNames() {
		if strings.HasPrefix(n, "prompt-dreamy-jazz") {
			found = true
		}
	}
	if !found {
		t.Fatalf("prompt session not persisted: %v", o.SessionNames())
	}
}

func TestLibraryInstantStart(t *testing.T) {
	dir := t.TempDir()
	lib := library.New(dir, 100, testLogger())
	banked := &engine.Track{Samples: make([]int16, audio.SampleRate*2*2), Prompt: "banked lofi"}
	for i := range banked.Samples {
		banked.Samples[i] = int16(i % 2000)
	}
	sess := session.New()
	if err := lib.Put(library.Key(sess), banked); err != nil {
		t.Fatal(err)
	}

	eng := enginetest.NewMock()
	eng.Delay = 800 * time.Millisecond // fresh generation is not instant
	pl := &capturePlayer{}
	store := session.NewStore(t.TempDir())
	builder := prompting.NewBuilder(nil, testLogger())
	o := New(testConfig(), eng, builder, store, sess, pl, testLogger())
	o.Library = lib
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	o.Start(ctx)
	t.Cleanup(func() { o.Close() })

	// The banked track must be playing well before fresh generation lands.
	waitFor(t, 2*time.Second, "library track playing", func() bool {
		st := o.Status()
		return st.State == "playing" && strings.Contains(st.Source, "[library]")
	})
	// The freshly generated track takes over when it arrives.
	waitFor(t, 10*time.Second, "fresh track takes over", func() bool {
		st := o.Status()
		return st.State == "playing" && !strings.Contains(st.Source, "[library]")
	})
}

// drainEvents collects events until the predicate matches or timeout.
func drainEvents(t *testing.T, o *Orchestrator, timeout time.Duration, match func(string) bool) string {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case ev := <-o.Events():
			if match(ev.Text) {
				return ev.Text
			}
		case <-deadline:
			t.Fatal("expected event never arrived")
		}
	}
}

func TestHealthyButFailingEngineRestartsAfterStreak(t *testing.T) {
	old := failureBackoffBase
	failureBackoffBase = 10 * time.Millisecond
	t.Cleanup(func() { failureBackoffBase = old })

	eng := enginetest.NewMock()
	eng.FailErr = errors.New("some transient-looking engine error")
	eng.HealOnRestart = true
	o, _ := newTestOrchestrator(t, eng, session.New())

	msg := drainEvents(t, o, 10*time.Second, func(s string) bool {
		return strings.Contains(s, "engine restarting")
	})
	if !strings.Contains(msg, "repeated generation failures") {
		t.Fatalf("restart message = %q", msg)
	}
	if got := eng.Restarts(); got != 1 {
		t.Fatalf("restarts = %d; want 1", got)
	}
	// The streak threshold, not the first failure, triggered it.
	specs := eng.Specs()
	if len(specs) < restartStreak {
		t.Fatalf("restart before the streak threshold: %d attempts", len(specs))
	}
	// Healed engine: music follows and the streak resets.
	waitFor(t, 10*time.Second, "recovery to playing", func() bool {
		st := o.Status()
		return st.State == "playing" && st.FailStreak == 0
	})
}

func TestDeviceFaultRestartsImmediately(t *testing.T) {
	old := failureBackoffBase
	failureBackoffBase = 10 * time.Millisecond
	t.Cleanup(func() { failureBackoffBase = old })

	eng := enginetest.NewMock()
	eng.FailErr = errors.New("generation failed: Expected all tensors to be on the same device, but found at least two devices, cuda:0 and cpu!")
	eng.HealOnRestart = true
	o, _ := newTestOrchestrator(t, eng, session.New())

	msg := drainEvents(t, o, 10*time.Second, func(s string) bool {
		return strings.Contains(s, "engine restarting")
	})
	if !strings.Contains(msg, "device-placement fault") {
		t.Fatalf("restart message = %q", msg)
	}
	if got := eng.Restarts(); got != 1 {
		t.Fatalf("restarts = %d; want 1", got)
	}
	// Immediately: exactly one failed attempt before the restart.
	if specs := eng.Specs(); len(specs) < 1 || len(specs) > 2 {
		t.Fatalf("device fault should restart on first failure; attempts=%d", len(specs))
	}
	waitFor(t, 10*time.Second, "recovery to playing", func() bool { return o.Status().State == "playing" })
}

func TestFailureStreakVisibleInStatus(t *testing.T) {
	old := failureBackoffBase
	failureBackoffBase = 20 * time.Millisecond
	t.Cleanup(func() { failureBackoffBase = old })

	eng := enginetest.NewMock()
	eng.FailErr = errors.New("boom reason")
	// No HealOnRestart: the streak keeps climbing across restarts.
	o, _ := newTestOrchestrator(t, eng, session.New())
	waitFor(t, 10*time.Second, "streak in status", func() bool {
		st := o.Status()
		return st.FailStreak >= 1 && strings.Contains(st.LastFailure, "boom reason")
	})
}
