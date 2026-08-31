package acestep

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"iar/internal/state"
)

// Remote is the client-side handle to the shared engine daemon. On start
// it adopts a live daemon from a previous run when one exists (making
// relaunches cheap) and spawns one otherwise. It keeps the daemon alive
// with heartbeats and tracks its phase for progress displays.
type Remote struct {
	dir       state.Dir
	log       *slog.Logger
	exe       string // player executable to spawn the daemon with
	daemonLog string // daemon log file, for Tail

	mu          sync.Mutex
	st          state.EngineState
	client      *Client
	phase       string
	adopted     bool
	lastSpawn   time.Time
	forcedCount int
	nextForced  time.Time
}

// NewRemote returns an unstarted remote handle. exe is the player binary
// path; daemonLog is where the daemon writes its output.
func NewRemote(dir state.Dir, exe, daemonLog string, log *slog.Logger) *Remote {
	return &Remote{dir: dir, log: log, exe: exe, daemonLog: daemonLog, phase: "starting engine"}
}

// Start adopts or spawns the daemon and begins the health-poll and
// heartbeat loops. It returns quickly; readiness is reported by Ready.
func (r *Remote) Start(ctx context.Context) {
	r.ensureDaemon()
	// Probe immediately so an adopted, already-ready daemon is usable
	// without waiting for the first poll tick.
	if phase := r.probe(ctx); phase != "" {
		r.mu.Lock()
		r.phase = phase
		r.mu.Unlock()
	}
	go r.pollLoop(ctx)
	go r.heartbeatLoop(ctx)
}

// Adopted reports whether an already-running daemon was reused.
func (r *Remote) Adopted() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.adopted
}

// Ready implements Backend.
func (r *Remote) Ready() bool { return r.Phase() == "ready" }

// Phase implements Backend.
func (r *Remote) Phase() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.phase
}

// Client implements Backend.
func (r *Remote) Client() *Client {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.client
}

// Port returns the daemon's port (zero when unknown).
func (r *Remote) Port() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.st.Port
}

// Tail implements Backend by reading recent lines of the daemon log.
func (r *Remote) Tail() []string {
	data, err := os.ReadFile(r.daemonLog)
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > tailLines {
		lines = lines[len(lines)-tailLines:]
	}
	return lines
}

// WaitReady blocks until the engine is ready, ctx ends, or budget elapses.
func (r *Remote) WaitReady(ctx context.Context, budget time.Duration) error {
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if r.Ready() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return context.DeadlineExceeded
}

// ensureDaemon adopts a live daemon or spawns a fresh one.
func (r *Remote) ensureDaemon() {
	lock, err := r.dir.AcquireLock("engine-client.lock", true)
	if err != nil {
		r.log.Error("engine client lock failed", "event", "engine_lock_failed", "error", err.Error())
		return
	}
	defer lock.Release()

	st, ok := r.dir.ReadEngineState()
	if ok && state.PIDAlive(st.PID) {
		r.setState(st, true)
		r.log.Info("adopted running engine daemon", "event", "engine_adopted", "pid", st.PID, "port", st.Port)
		return
	}
	if ok {
		// Retract only the dead record we just read; a daemon that
		// started in the meantime keeps its own.
		r.dir.RemoveEngineStateIf(st.PID)
	}
	r.spawnDaemon()
}

// spawnDaemon launches `iar engine daemon` detached from this process.
func (r *Remote) spawnDaemon() {
	r.mu.Lock()
	if time.Since(r.lastSpawn) < 10*time.Second {
		r.mu.Unlock()
		return
	}
	r.lastSpawn = time.Now()
	r.mu.Unlock()

	logFile, err := os.OpenFile(r.daemonLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		r.log.Error("cannot open engine daemon log", "event", "engine_spawn_failed", "error", err.Error())
		return
	}
	defer logFile.Close()
	cmd := exec.Command(r.exe, "engine", "daemon")
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	detach(cmd)
	if err := cmd.Start(); err != nil {
		r.log.Error("engine daemon spawn failed", "event", "engine_spawn_failed", "error", err.Error())
		return
	}
	r.log.Info("engine daemon spawned", "event", "engine_spawned", "pid", cmd.Process.Pid)
	// Reap the daemon if it exits while we are still alive; it keeps
	// running independently otherwise.
	go cmd.Wait()

	// Learn the daemon's port from the state file it writes.
	for i := 0; i < 100; i++ {
		if st, ok := r.dir.ReadEngineState(); ok && state.PIDAlive(st.PID) {
			r.setState(st, false)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	r.log.Error("engine daemon did not report state", "event", "engine_spawn_failed")
	r.mu.Lock()
	r.phase = "unavailable"
	r.mu.Unlock()
}

func (r *Remote) setState(st state.EngineState, adopted bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.st = st
	r.client = NewClient(baseURL(st.Port))
	r.adopted = adopted
	if r.phase == "unavailable" {
		r.phase = "starting engine"
	}
}

func baseURL(port int) string {
	return fmt.Sprintf("http://127.0.0.1:%d", port)
}

// pollLoop tracks the daemon's health once a second.
func (r *Remote) pollLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var lastPhase string
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		phase := r.probe(ctx)
		r.mu.Lock()
		r.phase = phase
		r.mu.Unlock()
		if phase != lastPhase {
			r.log.Info("engine phase", "event", "engine_phase", "phase", phase)
			lastPhase = phase
		}
	}
}

// probe determines the current phase, respawning the daemon if it died.
func (r *Remote) probe(ctx context.Context) string {
	r.mu.Lock()
	client := r.client
	pid := r.st.PID
	r.mu.Unlock()
	if client == nil {
		r.ensureDaemon()
		return "starting engine"
	}
	hctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	h, err := client.Health(hctx)
	cancel()
	switch {
	case err == nil && h.ModelsInitialized:
		return "ready"
	case err == nil:
		return "loading models"
	case state.PIDAlive(pid):
		// Daemon alive but the API server is not answering yet (or is
		// restarting after a crash).
		return "starting engine"
	default:
		// Daemon died; try to bring a fresh one up.
		r.log.Warn("engine daemon gone, restarting", "event", "engine_gone")
		r.ensureDaemon()
		return "starting engine"
	}
}

// heartbeatLoop keeps the daemon alive while this client runs.
func (r *Remote) heartbeatLoop(ctx context.Context) {
	r.dir.Heartbeat()
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.dir.Heartbeat()
		}
	}
}

// forcedRestartBackoff schedules how soon another forced restart may
// happen after each one; poisoned-engine recovery must not turn into a
// restart storm.
var forcedRestartBackoff = []time.Duration{0, time.Minute, 2 * time.Minute, 5 * time.Minute}

// RestartEngine force-restarts the engine daemon (used when generations
// keep failing while health still reports ok). It returns false when a
// restart is suppressed by the cooldown.
func (r *Remote) RestartEngine(reason string) bool {
	r.mu.Lock()
	now := time.Now()
	if now.Before(r.nextForced) {
		r.mu.Unlock()
		return false
	}
	idx := r.forcedCount
	if idx >= len(forcedRestartBackoff) {
		idx = len(forcedRestartBackoff) - 1
	}
	r.forcedCount++
	r.nextForced = now.Add(forcedRestartBackoff[idx])
	pid := r.st.PID
	r.phase = "starting engine"
	r.mu.Unlock()

	r.log.Warn("forcing engine daemon restart", "event", "engine_forced_restart",
		"reason", reason, "pid", pid, "restart_number", idx+1)
	if state.PIDAlive(pid) {
		state.Terminate(pid)
		for i := 0; i < 50 && state.PIDAlive(pid); i++ {
			time.Sleep(200 * time.Millisecond)
		}
		if state.PIDAlive(pid) {
			r.log.Error("engine daemon ignored SIGTERM", "event", "engine_forced_restart_stuck", "pid", pid)
		}
	}
	// The record belongs to the daemon just terminated; anything else
	// there now is a live successor and must survive.
	r.dir.RemoveEngineStateIf(pid)
	r.ensureDaemon()
	return true
}

// NoteGenerationOK resets the forced-restart backoff after recovery.
func (r *Remote) NoteGenerationOK() {
	r.mu.Lock()
	r.forcedCount = 0
	r.nextForced = time.Time{}
	r.mu.Unlock()
}
