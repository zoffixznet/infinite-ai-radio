package player

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"iar/internal/audio"
	"iar/internal/engine"
	"iar/internal/library"

	"iar/internal/config"
	"iar/internal/engine/enginetest"
	"iar/internal/prompting"
	"iar/internal/session"
	"iar/internal/snippets"
	"iar/internal/state"
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
	return newTestOrchestratorWithStore(t, eng, sess, session.NewStore(t.TempDir()))
}

func newTestOrchestratorWithStore(t *testing.T, eng *enginetest.Mock, sess *session.Session, store *session.Store) (*Orchestrator, *capturePlayer) {
	t.Helper()
	pl := &capturePlayer{}
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
			if strings.Contains(ev.Text, "iar setup") {
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
	o.SnippetsDir = t.TempDir() + "/music/radio-snippets"

	// Nothing to save before a generated track plays.
	if ack := o.SaveSnippet("", ""); !strings.Contains(ack, "nothing to save") {
		t.Fatalf("early save ack = %q", ack)
	}
	waitFor(t, 10*time.Second, "playing", func() bool { return o.Status().State == "playing" })

	// A tagged save lands in the tag's slug directory with the tag in
	// the album field; the ack carries the path.
	ack := o.SaveSnippet("", "Gym Grind!")
	if !strings.Contains(ack, "saving this track to gym_grind/") {
		t.Fatalf("save ack = %q", ack)
	}
	// The file appears atomically once the encode is complete.
	var files []os.DirEntry
	waitFor(t, 15*time.Second, "snippet file", func() bool {
		files = nil
		entries, _ := os.ReadDir(filepath.Join(o.SnippetsDir, "gym_grind"))
		for _, e := range entries {
			if !strings.HasPrefix(e.Name(), ".") {
				files = append(files, e)
			}
		}
		return len(files) == 1
	})
	if !strings.HasSuffix(files[0].Name(), ".mp3") {
		t.Fatalf("snippet name = %q", files[0].Name())
	}
	info, err := snippets.ReadInfo(filepath.Join(o.SnippetsDir, "gym_grind", files[0].Name()))
	if err != nil || info.Album != "gym_grind" || info.Title == "" || info.Duration < time.Second {
		t.Fatalf("snippet tags = %+v, %v", info, err)
	}
	if u := o.Status().Underruns; u != 0 {
		t.Fatalf("underruns while saving: %d", u)
	}
	// An untagged save goes to the untagged directory.
	waitFor(t, 15*time.Second, "save slot free", func() bool {
		return strings.Contains(o.SaveSnippet("", ""), "saving this track to untagged/")
	})
	// prev works after two distinct tracks; here at least verify the
	// unknown-prev message before one exists.
	o2, _ := newTestOrchestrator(t, enginetest.NewMock(), session.New())
	o2.SnippetsDir = t.TempDir()
	if ack := o2.SaveSnippet("prev", ""); !strings.Contains(ack, "no previous track") {
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
	if _, err := lib.Put(library.Key(sess), banked); err != nil {
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

// tapRecorder captures everything the orchestrator taps (what the remote
// stream would encode).
type tapRecorder struct {
	mu  sync.Mutex
	buf []byte
}

func (t *tapRecorder) Write(p []byte) (int, error) {
	t.mu.Lock()
	t.buf = append(t.buf, p...)
	t.mu.Unlock()
	return len(p), nil
}

// tail returns the last n bytes captured.
func (t *tapRecorder) tail(n int) []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.buf) < n {
		n = len(t.buf)
	}
	return append([]byte(nil), t.buf[len(t.buf)-n:]...)
}

func (t *tapRecorder) size() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.buf)
}

func nonSilent(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return true
		}
	}
	return false
}

// newTappedOrchestrator builds an orchestrator with a tap recorder.
func newTappedOrchestrator(t *testing.T, eng *enginetest.Mock, sess *session.Session) (*Orchestrator, *tapRecorder) {
	t.Helper()
	tap := &tapRecorder{}
	pl := &capturePlayer{}
	store := session.NewStore(t.TempDir())
	builder := prompting.NewBuilder(nil, testLogger())
	var e engine.Engine
	if eng != nil {
		e = eng
	}
	o := New(testConfig(), e, builder, store, sess, pl, testLogger())
	o.Tap = tap
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	o.Start(ctx)
	t.Cleanup(func() { o.Close() })
	return o, tap
}

// The remote stream must carry exactly what the speakers play on every
// audible path.
func TestTapCarriesEveryAudiblePath(t *testing.T) {
	t.Run("silence filler while engine loads", func(t *testing.T) {
		eng := enginetest.NewMock()
		eng.SetReady(false)
		_, tap := newTappedOrchestrator(t, eng, session.New())
		waitFor(t, 5*time.Second, "tap flowing", func() bool { return tap.size() > 40000 })
		// Silence phase: the tap must flow (silent bytes are correct here).
	})

	t.Run("noise mode", func(t *testing.T) {
		sess := session.New()
		sess.Mode = session.ModeNoise
		sess.NoiseColor = "pink"
		_, tap := newTappedOrchestrator(t, nil, sess)
		waitFor(t, 5*time.Second, "noise on tap", func() bool {
			return tap.size() > 40000 && nonSilent(tap.tail(8000))
		})
	})

	t.Run("fresh generation", func(t *testing.T) {
		o, tap := newTappedOrchestrator(t, enginetest.NewMock(), session.New())
		waitFor(t, 10*time.Second, "playing", func() bool { return o.Status().State == "playing" })
		waitFor(t, 10*time.Second, "music on tap", func() bool { return nonSilent(tap.tail(8000)) })
	})

	t.Run("library track", func(t *testing.T) {
		dir := t.TempDir()
		lib := library.New(dir, 100, testLogger())
		banked := &engine.Track{Samples: make([]int16, audio.SampleRate*2*2), Prompt: "banked"}
		for i := range banked.Samples {
			banked.Samples[i] = int16(1500)
		}
		sess := session.New()
		if _, err := lib.Put(library.Key(sess), banked); err != nil {
			t.Fatal(err)
		}
		eng := enginetest.NewMock()
		eng.Delay = 2 * time.Second
		tap := &tapRecorder{}
		o := New(testConfig(), eng, prompting.NewBuilder(nil, testLogger()),
			session.NewStore(t.TempDir()), sess, &capturePlayer{}, testLogger())
		o.Tap = tap
		o.Library = lib
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		o.Start(ctx)
		t.Cleanup(func() { o.Close() })
		waitFor(t, 5*time.Second, "library audio on tap", func() bool {
			return strings.Contains(o.Status().Source, "[library]") && nonSilent(tap.tail(8000))
		})
	})

	t.Run("loop-last-track fallback", func(t *testing.T) {
		old := failureBackoffBase
		failureBackoffBase = 50 * time.Millisecond
		t.Cleanup(func() { failureBackoffBase = old })
		eng := enginetest.NewMock()
		o, tap := newTappedOrchestrator(t, eng, session.New())
		// One good track, then the engine goes down hard.
		waitFor(t, 10*time.Second, "first track playing", func() bool { return o.Status().State == "playing" })
		eng.FailErr = errors.New("engine down")
		eng.SetReady(false)
		// Ride past the end of the first track into the loop fallback.
		waitFor(t, 20*time.Second, "loop fallback active", func() bool {
			return strings.Contains(o.Status().Source, "looping")
		})
		before := tap.size()
		waitFor(t, 5*time.Second, "loop audio still on tap", func() bool {
			return tap.size() > before+40000 && nonSilent(tap.tail(8000))
		})
	})

	// Pause and volume are the room's controls, not the station's: a
	// listener on the phone keeps hearing the radio when the speakers
	// at the machine go quiet. This deliberately reverses the earlier
	// rule that the tap matched the speakers.
	t.Run("pausing the speakers keeps the stream live", func(t *testing.T) {
		o, tap := newTappedOrchestrator(t, enginetest.NewMock(), session.New())
		waitFor(t, 10*time.Second, "playing", func() bool { return o.Status().State == "playing" })
		o.Pause()
		time.Sleep(400 * time.Millisecond) // flush in-flight chunks
		before := tap.size()
		waitFor(t, 5*time.Second, "tap still flowing while paused", func() bool { return tap.size() > before+20000 })
		if !nonSilent(tap.tail(4000)) {
			t.Fatal("a pause at the machine silenced the shared stream")
		}
	})

	t.Run("the machine's volume does not attenuate the stream", func(t *testing.T) {
		o, tap := newTappedOrchestrator(t, enginetest.NewMock(), session.New())
		waitFor(t, 10*time.Second, "playing", func() bool { return o.Status().State == "playing" })
		o.SetVolume(0)
		time.Sleep(400 * time.Millisecond)
		before := tap.size()
		waitFor(t, 5*time.Second, "tap still flowing at zero volume", func() bool { return tap.size() > before+20000 })
		if !nonSilent(tap.tail(4000)) {
			t.Fatal("turning the machine down silenced the shared stream")
		}
	})
}

// A pause silences the speakers in the room. It must not stall the
// stream behind them: the ring holds two seconds, so a pump that stops
// reading blocks the mixer almost at once, which stops the queue
// draining and stops generation - and anyone listening from the phone
// is left circling the same few tracks.
func TestPauseKeepsTheStreamMoving(t *testing.T) {
	o, _ := newTestOrchestrator(t, enginetest.NewMock(), session.New())
	waitFor(t, 10*time.Second, "playing", func() bool { return o.Status().State == "playing" })
	o.Pause()
	before := o.Status()
	waitFor(t, 20*time.Second, "tracks still being generated while paused", func() bool {
		return o.Status().GenCount > before.GenCount
	})
	waitFor(t, 20*time.Second, "the mixer still advancing while paused", func() bool {
		return o.Status().TrackID != before.TrackID
	})
}

func TestSessionDeleteGuardsAndTombstones(t *testing.T) {
	o, _ := newTestOrchestrator(t, enginetest.NewMock(), session.New())
	sd, err := state.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	o.StateDir = &sd
	o.recordCurrent()
	if cs, playing := sd.ReadCurrentSession(); !playing || cs.Name != o.CurrentName() {
		t.Fatalf("current session state = %+v, playing=%v", cs, playing)
	}

	// The playing session is refused.
	if ack := o.DeleteSession(o.CurrentName()); !strings.Contains(ack, "playing right now") {
		t.Fatalf("delete current ack = %q", ack)
	}
	// Naming marks the session user-named and persists it.
	ack := o.NameSession("Road Trip")
	if !strings.Contains(ack, "road-trip") {
		t.Fatalf("name ack = %q", ack)
	}
	saved, err := o.store.Load("road-trip")
	if err != nil || !saved.Named || saved.AutoNamed() || saved.LastPlayed.IsZero() {
		t.Fatalf("named session on disk = %+v, %v", saved, err)
	}
	if ack := o.NameSession("sleep"); !strings.Contains(ack, "built-in preset") {
		t.Fatalf("naming after a preset ack = %q", ack)
	}
	// Another saved session can be deleted; unknown names are reported.
	other := session.New()
	other.Name = "old-favourite"
	other.Named = true
	o.store.Save(other)
	if ack := o.DeleteSession("old-favourite"); ack != "session old-favourite deleted" {
		t.Fatalf("delete ack = %q", ack)
	}
	if _, err := o.store.Load("old-favourite"); err == nil {
		t.Fatal("deleted session still loads")
	}
	if ack := o.DeleteSession("ghost"); !strings.Contains(ack, "no session or preset") {
		t.Fatalf("delete unknown ack = %q", ack)
	}
	// Deleting a preset tombstones it: gone from listings, cannot be
	// started, LoadByName falls through to sessions.
	if ack := o.DeleteSession("sleep"); !strings.Contains(ack, "preset sleep deleted") {
		t.Fatalf("delete preset ack = %q", ack)
	}
	for _, p := range o.Presets() {
		if p.Name == "sleep" {
			t.Fatal("tombstoned preset still listed")
		}
	}
	if ack := o.LoadPreset("sleep"); !strings.Contains(ack, "restore-presets") {
		t.Fatalf("load tombstoned preset ack = %q", ack)
	}
	if ack := o.LoadByName("sleep"); !strings.Contains(ack, "not found") {
		t.Fatalf("LoadByName tombstoned = %q", ack)
	}
	// LoadByName resolves presets and sessions; the state file follows.
	if ack := o.LoadByName("jazz-club"); !strings.Contains(ack, "preset jazz-club") {
		t.Fatalf("LoadByName preset = %q", ack)
	}
	if ack := o.LoadByName("road-trip"); !strings.Contains(ack, "session road-trip loaded") {
		t.Fatalf("LoadByName session = %q", ack)
	}
	if cs, _ := sd.ReadCurrentSession(); cs.Name != "road-trip" {
		t.Fatalf("state after load = %+v", cs)
	}
	l := o.Listing()
	if len(l.Named) == 0 || l.Named[0].Name != "road-trip" {
		t.Fatalf("listing named = %+v", l.Named)
	}
}

func TestSweepLoopRemovesStaleAutoSessions(t *testing.T) {
	store := session.NewStore(t.TempDir())
	now := time.Now()
	stale := session.New()
	stale.Name = "session-20260801-120000"
	stale.Created, stale.Updated, stale.LastPlayed = now.Add(-72*time.Hour), now.Add(-72*time.Hour), now.Add(-72*time.Hour)
	store.Save(stale)
	keepNamed := session.New()
	keepNamed.Name = "gym-grind"
	keepNamed.Named = true
	keepNamed.Updated, keepNamed.LastPlayed = now.Add(-72*time.Hour), now.Add(-72*time.Hour)
	store.Save(keepNamed)
	// The playing session is old on disk too, but must survive.
	cur := session.New()
	cur.Name = "session-20260802-120000"
	cur.Created, cur.Updated, cur.LastPlayed = now.Add(-72*time.Hour), now.Add(-72*time.Hour), now.Add(-72*time.Hour)
	store.Save(cur)

	old := sweepInterval
	sweepInterval = 50 * time.Millisecond
	t.Cleanup(func() { sweepInterval = old })
	o := New(testConfig(), enginetest.NewMock(), prompting.NewBuilder(nil, testLogger()), store, cur, &capturePlayer{}, testLogger())
	o.Retention = 48 * time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	o.Start(ctx)
	t.Cleanup(func() { o.Close() })

	waitFor(t, 5*time.Second, "stale session swept", func() bool {
		_, err := store.Load("session-20260801-120000")
		return err != nil
	})
	if _, err := store.Load("gym-grind"); err != nil {
		t.Fatal("named session swept")
	}
	got, err := store.Load(cur.Name)
	if err != nil {
		t.Fatal("playing session swept")
	}
	if time.Since(got.LastPlayed) > time.Minute {
		t.Fatalf("playing session's last-played stamp not refreshed: %v", got.LastPlayed)
	}
}

// mkTrack builds a short in-memory track for queue plumbing tests.
func mkTrack(prompt string, seconds int) *engine.Track {
	return &engine.Track{
		Samples: make([]int16, seconds*audio.SampleRate*audio.Channels),
		Prompt:  prompt,
	}
}

func TestSteerInterruptFlagConsumedExactlyOnce(t *testing.T) {
	// No Start: exercise the flag mechanics directly, under o.mu.
	store := session.NewStore(t.TempDir())
	o := New(testConfig(), enginetest.NewMock(), prompting.NewBuilder(nil, testLogger()), store, session.New(), &capturePlayer{}, testLogger())

	// A steer with nothing queued must not switch prematurely: the
	// current (pre-steer) source keeps playing, the flag stays armed.
	o.Steer("more energetic")
	if o.takeSwitch() {
		t.Fatal("switch requested with an empty queue")
	}
	o.mu.Lock()
	armed := o.steerPending
	o.mu.Unlock()
	if !armed {
		t.Fatal("steerPending consumed while the queue was empty")
	}

	// Once a post-steer track is queued, exactly one switch fires.
	o.mu.Lock()
	o.queue = append(o.queue, mkTrack("steered", 1))
	o.mu.Unlock()
	if !o.takeSwitch() {
		t.Fatal("no switch with a post-steer track queued")
	}
	if o.takeSwitch() {
		t.Fatal("switch fired twice for one steer")
	}

	// Loading something else disarms a pending steer switch.
	o.Steer("calmer")
	o.LoadPreset("pink-noise")
	o.mu.Lock()
	armed = o.steerPending
	o.mu.Unlock()
	if armed {
		t.Fatal("steerPending survived a preset load")
	}
}

func TestSteerInterruptsCurrentTrackMidPlay(t *testing.T) {
	eng := enginetest.NewMock()
	sess := session.New()
	sess.BasePrompt = "techno" // short: keeps tweaks inside the source label
	o, _ := newTestOrchestrator(t, eng, sess)
	waitFor(t, 10*time.Second, "music playing", func() bool {
		st := o.Status()
		return st.State == "playing" && st.Duration > 0
	})

	// Tracks are 2s long and mixing runs much faster than wall time;
	// the switch to the steered track must happen almost immediately,
	// not after the pre-steer track's natural end.
	ack := o.Steer("darker")
	if !strings.Contains(ack, "switching") {
		t.Fatalf("steer ack says nothing about switching: %q", ack)
	}
	waitFor(t, 10*time.Second, "steered track playing", func() bool {
		return strings.Contains(o.Status().Source, "dark")
	})
}

func TestLanguageEditReleasesAHandSteeredPin(t *testing.T) {
	o, _ := newTestOrchestrator(t, enginetest.NewMock(), session.New())
	o.SetLanguages([]string{"English", "Russian"})
	o.Steer("vocals in french")
	o.mu.Lock()
	pinned := o.sess.Spec != nil && o.sess.Spec.LanguagePinned
	o.mu.Unlock()
	if !pinned {
		t.Fatal("steering a language should pin it")
	}
	// The chips are the newest and most explicit thing said about
	// language, and on the phone they are the only thing that can be
	// said, so they release the older steer instead of being ignored.
	ack := o.SetLanguage("Russian", false)
	o.mu.Lock()
	spec := o.sess.Spec
	o.mu.Unlock()
	if spec.LanguagePinned || spec.VocalLanguage != "" {
		t.Fatalf("the pin survived a language edit: %+v", spec)
	}
	if !strings.Contains(ack, "steered in earlier") {
		t.Fatalf("the ack said nothing about replacing the steer: %q", ack)
	}
}

func TestSavingTheSameLanguagesKeepsTheQueue(t *testing.T) {
	o, _ := newTestOrchestrator(t, enginetest.NewMock(), session.New())
	o.mu.Lock()
	o.sess.Vocal = true
	o.mu.Unlock()
	o.SetLanguages([]string{"English", "Russian"})
	waitFor(t, 20*time.Second, "a track queued", func() bool {
		o.mu.Lock()
		defer o.mu.Unlock()
		return len(o.queue) > 0
	})
	o.mu.Lock()
	epoch := o.epoch
	o.mu.Unlock()
	// Saving an unchanged list must not throw away minutes of audio
	// that is already generated and still correct. Dropping the queue
	// IS the epoch bump, so an unchanged epoch proves no drop; the
	// queue's length itself is no signal - the generator grows it and
	// the mixer consumes from it concurrently throughout.
	ack := o.SetLanguages([]string{"English", "Russian"})
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.epoch != epoch {
		t.Fatalf("an unchanged list dropped the queue: epoch %d->%d", epoch, o.epoch)
	}
	if !strings.Contains(ack, "unchanged") {
		t.Fatalf("ack = %q", ack)
	}
}

func TestSkipAckHonesty(t *testing.T) {
	store := session.NewStore(t.TempDir())
	o := New(testConfig(), enginetest.NewMock(), prompting.NewBuilder(nil, testLogger()), store, session.New(), &capturePlayer{}, testLogger())

	if got := o.Skip(); !strings.Contains(got, "still generating") {
		t.Fatalf("empty-queue skip ack = %q", got)
	}
	o.mu.Lock()
	o.lastGood = mkTrack("x", 1)
	o.lastGoodEpoch = o.epoch
	armed := o.switchReq
	o.mu.Unlock()
	if armed {
		t.Fatal("a skip with an empty queue armed a switch into the track already playing")
	}
	if got := o.Skip(); !strings.Contains(got, "looping") {
		t.Fatalf("loop-fallback skip ack = %q", got)
	}
	// After a context change the last good track is the sound the
	// listener just left, and the ack must not pretend otherwise.
	o.mu.Lock()
	o.epoch++
	o.mu.Unlock()
	if got := o.Skip(); !strings.Contains(got, "previous sound") {
		t.Fatalf("stale loop-fallback skip ack = %q", got)
	}
	o.mu.Lock()
	o.queue = append(o.queue, mkTrack("y", 1))
	o.mu.Unlock()
	if got := o.Skip(); got != "skipping to the next track" {
		t.Fatalf("queued skip ack = %q", got)
	}
	o.mu.Lock()
	o.sess.Mode = session.ModeNoise
	o.mu.Unlock()
	if got := o.Skip(); !strings.Contains(got, "noise") {
		t.Fatalf("noise-mode skip ack = %q", got)
	}
}

func TestSnippetAckShowsOnlyTagAndFile(t *testing.T) {
	eng := enginetest.NewMock()
	o, _ := newTestOrchestrator(t, eng, session.New())
	o.SnippetsDir = t.TempDir()
	waitFor(t, 10*time.Second, "music playing", func() bool { return o.Status().State == "playing" })

	ack := o.SaveSnippet("", "Road Trip!")
	if strings.Contains(ack, o.SnippetsDir) || strings.Contains(ack, "/home/") {
		t.Fatalf("save ack leaks an absolute path: %q", ack)
	}
	if !strings.Contains(ack, "road_trip/") {
		t.Fatalf("save ack missing the tag-slug/file form: %q", ack)
	}
	// The completion event uses the same short form.
	waitFor(t, 10*time.Second, "completion event", func() bool {
		select {
		case ev := <-o.Events():
			if strings.Contains(ev.Text, "track saved:") {
				if strings.Contains(ev.Text, o.SnippetsDir) {
					t.Fatalf("completion event leaks the absolute path: %q", ev.Text)
				}
				return true
			}
		default:
		}
		return false
	})
}

// The generator reads the session's language map on its own goroutine
// while the controls write it. A hand-rolled session copy used to leave
// that map aliased, which is a concurrent map read and write: Go kills
// the process outright, and it would happen exactly while someone was
// fiddling with the language chips.
func TestLanguageSwitchingRacesGeneration(t *testing.T) {
	sess := session.New()
	sess.Vocal = true // BuildSpec only reads the language map for vocals
	o, _ := newTestOrchestrator(t, enginetest.NewMock(), sess)
	o.SetLanguages([]string{"English", "Russian", "French"})
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			_, snap := o.snapshotSession()
			o.builder.BuildSpec(context.Background(), snap, 30)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-done:
				return
			default:
			}
			o.SetLanguage("Russian", i%2 == 0)
		}
	}()
	time.Sleep(500 * time.Millisecond)
	close(done)
	wg.Wait()
}

func TestTrackLanguageOnlyReportsWhatIsSung(t *testing.T) {
	vocal := &engine.Track{Spec: engine.Spec{SampleQuery: "pop, with sung vocals", VocalLanguage: "ru"}}
	if got := trackLanguage(vocal); got != "Russian" {
		t.Errorf("vocal track language = %q", got)
	}
	named := &engine.Track{Spec: engine.Spec{
		SampleQuery: "island pop", VocalLanguage: "ceb", VocalLanguageName: "Bisaya (Cebuano)",
	}}
	// The listener's own wording wins over the tag: "ceb" is not a name
	// anyone would recognise on a now-playing line.
	if got := trackLanguage(named); got != "Bisaya (Cebuano)" {
		t.Errorf("named-language track = %q", got)
	}
	inst := &engine.Track{Spec: engine.Spec{Prompt: "lofi", Lyrics: engine.InstrumentalLyrics, VocalLanguage: "fr"}}
	if got := trackLanguage(inst); got != "" {
		t.Errorf("an instrumental reported a language: %q", got)
	}
}

func TestTrackLyricsShowsOnlyRealWords(t *testing.T) {
	// The engine echoes back what it actually sang, which for a track it
	// planned itself is the only record of the words.
	sung := &engine.Track{Lyrics: "  [Verse]\nnaay usa ka gabii\n"}
	if got := trackLyrics(sung); got != "[Verse]\nnaay usa ka gabii" {
		t.Errorf("lyrics = %q", got)
	}
	if got := trackLyrics(&engine.Track{Lyrics: engine.InstrumentalLyrics}); got != "" {
		t.Errorf("the instrumental marker is not lyrics: %q", got)
	}
	if got := trackLyrics(&engine.Track{}); got != "" {
		t.Errorf("empty lyrics = %q", got)
	}
}

func TestLanguageSwitchesSurviveAReload(t *testing.T) {
	dir := t.TempDir()
	store := session.NewStore(dir)
	sess := session.New()
	sess.Name = "keeper"
	sess.Vocal = true
	o, _ := newTestOrchestratorWithStore(t, enginetest.NewMock(), sess, store)
	o.SetLanguages([]string{"English", "Russian", "French"})
	o.SetLanguage("Russian", false)

	// Within the run, every reader sees the same thing (a browser
	// reload just re-reads this).
	for _, l := range o.Status().Languages {
		if l.Name == "Russian" && l.On {
			t.Fatal("Russian still reads as on straight after switching it off")
		}
	}
	// And it reached the saved session, so it is not lost with the
	// process.
	waitFor(t, 5*time.Second, "session saved", func() bool {
		got, err := store.Load("keeper")
		return err == nil && got.Languages != nil
	})
	got, err := store.Load("keeper")
	if err != nil {
		t.Fatal(err)
	}
	if on, ok := got.Languages["Russian"]; !ok || on {
		t.Fatalf("saved session languages = %+v", got.Languages)
	}
}

func TestQueueListingDuringASwitchover(t *testing.T) {
	lib := library.New(t.TempDir(), 100, testLogger())
	sess := session.New()
	sess.Vocal = true
	o, _ := newTestOrchestrator(t, enginetest.NewMock(), sess)
	o.Library = lib
	o.SetLanguages([]string{"English", "Russian"})

	// Two banked tracks for this vibe, one in each language.
	for _, lang := range []string{"English", "Russian"} {
		tr := mkTrack("banked "+lang, 1)
		tr.Spec.VocalLanguageName = lang
		if _, err := lib.Put(library.Key(sess), tr); err != nil {
			t.Fatal(err)
		}
		time.Sleep(1100 * time.Millisecond) // ids start with a whole-second timestamp
	}

	fillerLangs := func() []string {
		var out []string
		_, tracks := o.QueueTracks()
		for _, qt := range tracks {
			if qt.Kind == "library" {
				out = append(out, qt.Prompt)
			}
		}
		return out
	}

	// A language the session no longer sings in must not be offered:
	// a remote client would download it and present it as the new
	// setting taking effect.
	o.SetLanguage("English", false)
	o.mu.Lock()
	o.steerPending = false // the switchover suppression is tested below
	o.mu.Unlock()
	for _, p := range fillerLangs() {
		if strings.Contains(p, "English") {
			t.Fatalf("filler still offers a language that is switched off: %q", p)
		}
	}
	if len(fillerLangs()) == 0 {
		t.Fatal("filler dropped the language that is still switched on")
	}

	// While the first track of a new context generates, nothing banked
	// is offered at all: it is all older than what is already playing.
	o.mu.Lock()
	o.steerPending = true
	o.mu.Unlock()
	if got := fillerLangs(); len(got) != 0 {
		t.Fatalf("filler offered during a switchover: %v", got)
	}
}

func TestQueueTracksAndTrackData(t *testing.T) {
	eng := enginetest.NewMock()
	sess := session.New()
	o, pl := newTestOrchestrator(t, eng, sess)
	dir := t.TempDir()
	o.Library = library.New(dir, 100, testLogger())
	// Read the listing inside the wait: checking Status first and
	// listing after leaves a window for the mixer to take the only
	// queued track, which makes this test flake under load.
	var epoch int
	var tracks []QueueTrack
	waitFor(t, 10*time.Second, "a freshly generated track listed", func() bool {
		epoch, tracks = o.QueueTracks()
		for _, qt := range tracks {
			if qt.Kind == "queue" {
				return true
			}
		}
		return false
	})
	queued := 0
	for _, qt := range tracks {
		if qt.ID == "" || qt.Seconds <= 0 || (qt.Kind != "queue" && qt.Kind != "library") {
			t.Fatalf("bad queue row: %+v", qt)
		}
		if qt.Title == "" {
			t.Fatalf("queue row without a short title: %+v", qt)
		}
		if qt.Kind == "queue" {
			queued++
		}
	}
	if queued == 0 {
		t.Fatal("no freshly generated tracks listed")
	}
	if st := o.Status(); st.Epoch != epoch {
		t.Fatalf("epoch mismatch: %d vs %d", st.Epoch, epoch)
	}

	// Ids resolve to audio; unknown ids do not.
	track, ok := o.TrackData(tracks[0].ID)
	if !ok || len(track.Samples) == 0 {
		t.Fatalf("TrackData(%q) failed", tracks[0].ID)
	}
	if _, ok := o.TrackData("nope"); ok {
		t.Fatal("unknown id resolved")
	}

	// The playing track stays resolvable after leaving the queue.
	waitFor(t, 10*time.Second, "a track playing", func() bool { return o.Status().TrackID != "" })
	cur := o.Status().TrackID
	if _, ok := o.TrackData(cur); !ok {
		t.Fatalf("playing track %q not resolvable", cur)
	}

	// Library filler appears once tracks are banked for this vibe.
	waitFor(t, 10*time.Second, "library banked", func() bool {
		_, tracks := o.QueueTracks()
		for _, qt := range tracks {
			if qt.Kind == "library" {
				if _, ok := o.TrackData(qt.ID); !ok {
					t.Fatalf("library id %q not loadable", qt.ID)
				}
				return true
			}
		}
		return false
	})

	// Serving track data never disturbs playback: hammer the queue
	// surface while audio flows and expect zero underruns.
	before := pl.bytes()
	done := make(chan struct{})
	for i := 0; i < 4; i++ {
		go func() {
			for {
				select {
				case <-done:
					return
				default:
				}
				_, tracks := o.QueueTracks()
				for _, qt := range tracks {
					o.TrackData(qt.ID)
				}
			}
		}()
	}
	time.Sleep(1500 * time.Millisecond)
	close(done)
	if pl.bytes() <= before {
		t.Fatal("playback stalled while serving queue data")
	}
	if u := o.Status().Underruns; u != 0 {
		t.Fatalf("underruns while serving queue data: %d", u)
	}

	// Noise mode lists nothing to prefetch.
	o.Steer("pink noise")
	if _, tracks := o.QueueTracks(); len(tracks) != 0 {
		t.Fatalf("noise mode lists %d tracks", len(tracks))
	}
}

func TestSaveSnippetByIDAndIdempotency(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	eng := enginetest.NewMock()
	o, _ := newTestOrchestrator(t, eng, session.New())
	o.SnippetsDir = t.TempDir()
	o.Library = library.New(t.TempDir(), 100, testLogger())
	waitFor(t, 10*time.Second, "a track playing", func() bool { return o.Status().TrackID != "" })

	// Saving by explicit track id (what a buffered phone sends).
	id := o.Status().TrackID
	if ack := o.SaveSnippet(id, "drive"); !strings.Contains(ack, "saving this track to drive/") {
		t.Fatalf("save-by-id ack = %q", ack)
	}
	waitFor(t, 15*time.Second, "saved id recorded", func() bool {
		for _, s := range o.Status().SavedTrackIDs {
			if s == id {
				return true
			}
		}
		return false
	})
	// The state surface reports the saved flag for current/previous.
	st := o.Status()
	if st.TrackID == id && !st.TrackSaved {
		t.Fatalf("playing track not flagged saved: %+v", st)
	}
	if st.PrevTrackID == id && !st.PrevTrackSaved {
		t.Fatalf("previous track not flagged saved: %+v", st)
	}

	// A repeat save of the same track is a success no-op, never an
	// error and never a second file.
	entries, _ := os.ReadDir(filepath.Join(o.SnippetsDir, "drive"))
	before := len(entries)
	if ack := o.SaveSnippet(id, "drive"); !strings.Contains(ack, "already saved") {
		t.Fatalf("repeat save ack = %q", ack)
	}
	entries, _ = os.ReadDir(filepath.Join(o.SnippetsDir, "drive"))
	if len(entries) != before {
		t.Fatalf("repeat save wrote a file: %d -> %d", before, len(entries))
	}

	// Unknown and vanished ids are refused gently.
	if ack := o.SaveSnippet("t-0-0000", ""); !strings.Contains(ack, "no longer available") {
		t.Fatalf("unknown id ack = %q", ack)
	}

	// A banked library track saves through its lib: id.
	var libID string
	waitFor(t, 10*time.Second, "library filler listed", func() bool {
		_, tracks := o.QueueTracks()
		for _, qt := range tracks {
			if qt.Kind == "library" {
				libID = qt.ID
				return true
			}
		}
		return false
	})
	if ack := o.SaveSnippet(libID, "banked"); !strings.Contains(ack, "saving this track to banked/") {
		t.Fatalf("library save ack = %q", ack)
	}
	waitFor(t, 15*time.Second, "library snippet file", func() bool {
		entries, _ := os.ReadDir(filepath.Join(o.SnippetsDir, "banked"))
		return len(entries) == 1
	})
}

// TestTracksGetTitlesAndNumbers: every generated track enters the
// stream with a short display name, and playing tracks are numbered in
// play order.
func TestTracksGetTitlesAndNumbers(t *testing.T) {
	eng := enginetest.NewMock()
	o, _ := newTestOrchestrator(t, eng, session.New())
	waitFor(t, 10*time.Second, "a track playing", func() bool { return o.Status().TrackID != "" })
	st := o.Status()
	if st.TrackTitle == "" {
		t.Fatalf("playing track has no title: %+v", st)
	}
	if st.TrackNum != 1 {
		t.Fatalf("first track number = %d; want 1", st.TrackNum)
	}
	first := st.TrackID
	o.Skip()
	waitFor(t, 10*time.Second, "second track playing", func() bool {
		st = o.Status()
		return st.TrackID != "" && st.TrackID != first
	})
	if st.TrackNum != 2 {
		t.Fatalf("second track number = %d; want 2", st.TrackNum)
	}
	if st.PrevTrackID != first || st.PrevTrackTitle == "" {
		t.Fatalf("previous track fields wrong: %+v", st)
	}
}

func TestLyricsGenSwitching(t *testing.T) {
	eng := enginetest.NewMock()
	o, _ := newTestOrchestrator(t, eng, session.New())

	if st := o.Status(); st.LyricsGenerator != prompting.DefaultGeneratorName {
		t.Fatalf("default lyric writer = %q", st.LyricsGenerator)
	}
	listing := o.LyricsGen("")
	if !strings.Contains(listing, "scribe") || !strings.Contains(listing, "smoothbrain") {
		t.Fatalf("listing must name the writers: %q", listing)
	}
	if ack := o.LyricsGen("bogus"); !strings.Contains(ack, "unknown") {
		t.Fatalf("bogus writer ack = %q", ack)
	}
	ack := o.LyricsGen("smoothbrain")
	if !strings.Contains(ack, "smoothbrain") {
		t.Fatalf("switch ack = %q", ack)
	}
	if st := o.Status(); st.LyricsGenerator != "smoothbrain" {
		t.Fatalf("status after switch = %q", st.LyricsGenerator)
	}
	if ack := o.LyricsGen("smoothbrain"); !strings.Contains(ack, "already") {
		t.Fatalf("repeat switch ack = %q", ack)
	}
	// The choice survives in the persisted session.
	name := o.CurrentName()
	o.NameSession("keep-lyrics")
	loaded := o.LoadByName("keep-lyrics")
	if !strings.Contains(loaded, "keep-lyrics") {
		t.Fatalf("reload failed: %q (session %q)", loaded, name)
	}
	if st := o.Status(); st.LyricsGenerator != "smoothbrain" {
		t.Fatalf("lyric writer lost on reload: %q", st.LyricsGenerator)
	}
}

// The vocal-language catalogue is configured once, switched per session
// and persisted; every capability the phone remote drives is here.
func TestVocalLanguagesConfigureSwitchAndPersist(t *testing.T) {
	eng := enginetest.NewMock()
	sess := session.New()
	sess.Vocal = true
	o, _ := newTestOrchestrator(t, eng, sess)

	var saved, savedOff [][]string
	o.SetLanguageStore(func(names, off []string) error {
		saved = append(saved, append([]string(nil), names...))
		savedOff = append(savedOff, append([]string(nil), off...))
		return nil
	})

	// Nothing configured: the music engine keeps choosing.
	if states := o.Languages(); len(states) != 0 {
		t.Fatalf("a fresh run has no configured languages: %+v", states)
	}

	ack := o.SetLanguages([]string{"English", "Russian", "Bisaya (Cebuano)"})
	if !strings.Contains(ack, "English") || !strings.Contains(ack, "Bisaya (Cebuano)") {
		t.Fatalf("acknowledgment does not name the languages: %q", ack)
	}
	if len(saved) != 1 || strings.Join(saved[0], "|") != "English|Russian|Bisaya (Cebuano)" {
		t.Fatalf("catalogue was not persisted: %v", saved)
	}

	states := o.Languages()
	if len(states) != 3 {
		t.Fatalf("configured languages: %+v", states)
	}
	for _, l := range states {
		if !l.On {
			t.Errorf("%s should start switched on", l.Name)
		}
	}
	// Cebuano is not on the engine's published list, but it accepts the
	// tag and writes Cebuano for it, so it counts as a language the
	// engine can sing.
	if !states[2].Engine {
		t.Errorf("Cebuano should carry an engine tag: %+v", states[2])
	}

	// A switch is a standing preference, so it is written down beside
	// the list rather than left in the session: a fresh session must
	// not start singing a language that was turned off.
	o.SetLanguage("Russian", false)
	if len(savedOff) == 0 || strings.Join(savedOff[len(savedOff)-1], "|") != "Russian" {
		t.Fatalf("switched-off languages were not persisted: %v", savedOff)
	}
	o.SetLanguage("Russian", true)
	if got := savedOff[len(savedOff)-1]; len(got) != 0 {
		t.Fatalf("switching back on left it recorded as off: %v", got)
	}

	// Switching one off leaves the rest alone.
	o.SetLanguage("Russian", false)
	states = o.Languages()
	if states[0].On != true || states[1].On != false || states[2].On != true {
		t.Fatalf("switching Russian off changed the wrong ones: %+v", states)
	}
	if got := o.Status().Languages; len(got) != 3 || got[1].On {
		t.Fatalf("status does not carry the language choices: %+v", got)
	}

	// A language nobody configured cannot be switched.
	if ack := o.SetLanguage("Klingon", true); !strings.Contains(ack, "not one of the configured") {
		t.Errorf("unknown language ack: %q", ack)
	}

	// An edit that keeps a language keeps its switch too.
	o.SetLanguages([]string{"English", "Russian", "Bisaya (Cebuano)", "French"})
	if got := o.Languages(); len(got) != 4 || got[1].On {
		t.Fatalf("an edit that keeps Russian must keep it switched off: %+v", got)
	}
	// Dropping a language forgets its switch, so configuring the same
	// name again does not resurrect an old off.
	o.SetLanguages([]string{"English", "Bisaya (Cebuano)"})
	if got := o.Languages(); len(got) != 2 {
		t.Fatalf("catalogue after the edit: %+v", got)
	}
	o.SetLanguages([]string{"English", "Russian", "Bisaya (Cebuano)"})
	if got := o.Languages(); !got[1].On {
		t.Errorf("Russian's old off switch survived being dropped: %+v", got)
	}

	// Switching them all off hands the choice back to the engine.
	for _, name := range []string{"English", "Russian", "Bisaya (Cebuano)"} {
		o.SetLanguage(name, false)
	}
	if ack := o.SetLanguage("English", false); !strings.Contains(ack, "whatever language the music engine picks") {
		t.Errorf("all-off ack: %q", ack)
	}
}

// A configured catalogue that cannot be written down still works for
// the rest of the run, and says so.
func TestVocalLanguagesSurviveAFailedWrite(t *testing.T) {
	eng := enginetest.NewMock()
	o, _ := newTestOrchestrator(t, eng, session.New())
	o.SetLanguageStore(func(names, off []string) error { return errors.New("read-only file system") })
	ack := o.SetLanguages([]string{"English"})
	if !strings.Contains(ack, "this run only") || !strings.Contains(ack, "read-only file system") {
		t.Fatalf("a failed write must be reported honestly: %q", ack)
	}
	if got := o.Languages(); len(got) != 1 || !got[0].On {
		t.Fatalf("the catalogue should still be live: %+v", got)
	}
}

func TestRequestedLoopRepeatsTheTrackUntilTurnedOff(t *testing.T) {
	store := session.NewStore(t.TempDir())
	o := New(testConfig(), enginetest.NewMock(), prompting.NewBuilder(nil, testLogger()), store, session.New(), &capturePlayer{}, testLogger())

	// Nothing playable yet: the toggle refuses honestly.
	if got := o.ToggleLoop(); !strings.Contains(got, "nothing loopable") {
		t.Fatalf("toggle with nothing playing = %q", got)
	}

	banger := mkTrack("banger", 2)
	banger.Title = "Banger"
	// A library warm-up is exactly the track fallbackShouldYield wants
	// to interrupt; the loop guard must hold it in place regardless.
	banger.FromLibrary = true
	cur := newTrackSource(banger, "t1")
	o.mu.Lock()
	o.cur = cur
	o.queue = append(o.queue, mkTrack("other", 1))
	o.mu.Unlock()

	if got := o.ToggleLoop(); !strings.Contains(got, "Banger") {
		t.Fatalf("toggle-on ack = %q", got)
	}
	if !o.Status().LoopOn {
		t.Fatal("status does not report the requested loop")
	}

	// The track's end brings the same recording back; the queue is
	// untouched and the replay does not count as a play.
	next := o.chooseNext(cur)
	ts, ok := next.(*trackSource)
	if !ok || ts.track != banger {
		t.Fatalf("chooseNext under loop = %#v, want the same track back", next)
	}
	if !strings.Contains(ts.label(), "looping on request") {
		t.Fatalf("replay label = %q", ts.label())
	}
	o.mu.Lock()
	qlen, played := len(o.queue), o.playedInEpoch
	o.mu.Unlock()
	if qlen != 1 {
		t.Fatalf("the loop consumed the queue: %d left", qlen)
	}
	if played != 0 {
		t.Fatal("a replay counted toward the batch ladder's play count")
	}
	// The replay must not be interrupted as a warm-up either.
	if o.fallbackShouldYield(ts) {
		t.Fatal("a requested loop yielded to the queue")
	}

	// Toggling off moves on: the queued track plays next.
	if got := o.ToggleLoop(); !strings.Contains(got, "loop off") {
		t.Fatalf("toggle-off ack = %q", got)
	}
	after := o.chooseNext(ts)
	ts2, ok := after.(*trackSource)
	if !ok || ts2.track.Prompt != "other" {
		t.Fatalf("after loop off chooseNext = %#v, want the queued track", after)
	}
}

func TestSkipAndSteeringBreakARequestedLoop(t *testing.T) {
	store := session.NewStore(t.TempDir())
	o := New(testConfig(), enginetest.NewMock(), prompting.NewBuilder(nil, testLogger()), store, session.New(), &capturePlayer{}, testLogger())

	cur := newTrackSource(mkTrack("banger", 2), "t1")
	o.mu.Lock()
	o.cur = cur
	o.queue = append(o.queue, mkTrack("other", 1))
	o.mu.Unlock()

	// Skip means "move on", so it turns the loop off and says so.
	o.ToggleLoop()
	if got := o.Skip(); !strings.Contains(got, "loop turned off") {
		t.Fatalf("skip-under-loop ack = %q", got)
	}
	if next := o.chooseNext(cur); next.(*trackSource).track.Prompt != "other" {
		t.Fatal("skip left the loop armed: the same track came back")
	}

	// Mid-switch the playing track belongs to the setting the listener
	// just left: arming a loop then would pin the old sound inside the
	// new context, so the toggle refuses until the switch lands.
	o.mu.Lock()
	o.steerPending = true
	o.mu.Unlock()
	if got := o.ToggleLoop(); !strings.Contains(got, "switching") {
		t.Fatalf("toggle during a pending steer = %q", got)
	}
	o.mu.Lock()
	o.steerPending = false
	o.mu.Unlock()

	// A context change breaks the loop on its own: the flag is tied to
	// the steering epoch it was set in.
	o.mu.Lock()
	o.queue = append([]*engine.Track{}, mkTrack("fresh", 1))
	o.mu.Unlock()
	o.ToggleLoop()
	o.mu.Lock()
	o.epoch++
	o.mu.Unlock()
	if o.Status().LoopOn {
		t.Fatal("the loop survived a steering epoch change")
	}
	if next := o.chooseNext(cur); next.(*trackSource).track.Prompt != "fresh" {
		t.Fatal("a stale loop replayed across a context change")
	}
}
