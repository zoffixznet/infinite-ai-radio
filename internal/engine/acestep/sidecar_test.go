package acestep

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
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
