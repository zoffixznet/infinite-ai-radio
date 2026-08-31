package acestep

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// healthServer answers health checks; ready can be toggled.
func healthServer(ready *atomic.Bool) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(wrap(map[string]any{
			"status": "ok", "models_initialized": ready.Load(),
		}))
	}))
}

func TestSidecarBecomesReadyAndRestartsOnExit(t *testing.T) {
	var ready atomic.Bool
	ready.Store(true)
	srv := healthServer(&ready)
	defer srv.Close()

	client := NewClient(srv.URL)
	s := NewSidecar(SidecarConfig{
		FirstLoadBudget: 10 * time.Second,
		InitialBackoff:  50 * time.Millisecond,
		PollInterval:    30 * time.Millisecond,
	}, client, discardLogger())

	// The "sidecar" is a short-lived sleep so we can watch a restart.
	var starts atomic.Int32
	s.newCmd = func(ctx context.Context) (*exec.Cmd, error) {
		starts.Add(1)
		return exec.CommandContext(ctx, "sh", "-c", "echo booting; sleep 0.5"), nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)

	waitFor(t, 5*time.Second, "sidecar ready", s.Ready)
	if got := s.Tail(); len(got) == 0 || got[0] != "booting" {
		t.Fatalf("tail = %v; want drained child output", got)
	}
	// After the child exits, the supervisor must restart it.
	waitFor(t, 5*time.Second, "restart counted", func() bool { return s.Restarts() >= 1 })
	waitFor(t, 5*time.Second, "second start", func() bool { return starts.Load() >= 2 })
}

func TestSidecarNotReadyWhileModelsLoad(t *testing.T) {
	var ready atomic.Bool
	srv := healthServer(&ready)
	defer srv.Close()
	client := NewClient(srv.URL)
	s := NewSidecar(SidecarConfig{
		FirstLoadBudget: 10 * time.Second,
		InitialBackoff:  50 * time.Millisecond,
		PollInterval:    30 * time.Millisecond,
	}, client, discardLogger())
	s.newCmd = func(ctx context.Context) (*exec.Cmd, error) {
		return exec.CommandContext(ctx, "sleep", "30"), nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)

	time.Sleep(300 * time.Millisecond)
	if s.Ready() {
		t.Fatal("ready before models initialized")
	}
	if !s.Starting() {
		t.Fatal("should report starting")
	}
	ready.Store(true)
	waitFor(t, 5*time.Second, "ready after models load", s.Ready)
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// envOf returns the child environment serverCommand would launch with,
// skipping the test when uv is not installed on this machine.
func envOf(t *testing.T, cfg SidecarConfig) []string {
	t.Helper()
	if _, err := findUV(); err != nil {
		t.Skip("uv not installed")
	}
	s := NewSidecar(cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	cmd, err := s.serverCommand(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return cmd.Env
}

func hasEnv(env []string, kv string) bool {
	for _, e := range env {
		if e == kv {
			return true
		}
	}
	return false
}

func TestServerCommandMemoryEnv(t *testing.T) {
	base := SidecarConfig{EngineDir: t.TempDir(), Port: 1234}

	env := envOf(t, base)
	if hasEnv(env, "ACESTEP_OFFLOAD_DIT_TO_CPU=true") {
		t.Error("offload env set without OffloadDIT")
	}
	for _, e := range env {
		if strings.HasPrefix(e, "ACESTEP_SAMPLE_DURATION_CAP=") {
			t.Errorf("duration cap set without MaxTrackSeconds: %s", e)
		}
	}

	withKnobs := base
	withKnobs.OffloadDIT = true
	withKnobs.MaxTrackSeconds = 300
	env = envOf(t, withKnobs)
	if !hasEnv(env, "ACESTEP_OFFLOAD_DIT_TO_CPU=true") {
		t.Error("OffloadDIT did not reach the child env")
	}
	if !hasEnv(env, "ACESTEP_SAMPLE_DURATION_CAP=300") {
		t.Error("MaxTrackSeconds did not reach the child env")
	}
}
