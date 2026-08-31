package acestep

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"iar/internal/state"
)

// DaemonConfig configures the shared engine daemon process.
type DaemonConfig struct {
	// StateDir holds the daemon's state file and the client heartbeat.
	StateDir state.Dir
	// Sidecar configures the supervised API server. Port zero means
	// allocate a free port.
	Sidecar SidecarConfig
	// IdleTimeout shuts the daemon down after this long without any
	// client heartbeat, freeing GPU memory. Zero means 15 minutes.
	IdleTimeout time.Duration
}

// StrayDaemons lists engine-daemon processes other than the one the
// state file names, and only those serving the same data directory as
// this process - a sandbox and a real instance share a machine happily
// and neither is stray to the other. A stray holds several gigabytes of
// graphics memory while being unreachable by every client and every
// command, so it is worth reporting.
//
// dataDirEnv is this process's IAR_DATA_DIR value, empty when it uses
// the default location.
func StrayDaemons(serving int, dataDirEnv string) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var out []int
	self := os.Getpid()
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == serving || pid == self {
			continue
		}
		raw, err := os.ReadFile("/proc/" + e.Name() + "/cmdline")
		if err != nil {
			continue
		}
		args := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
		if len(args) < 3 || !strings.HasSuffix(args[0], "iar") ||
			args[1] != "engine" || args[2] != "daemon" {
			continue
		}
		if processDataDir(pid) != dataDirEnv {
			continue
		}
		out = append(out, pid)
	}
	return out
}

// processDataDir reports another process's IAR_DATA_DIR, empty when it
// is unset (the default location) or unreadable.
func processDataDir(pid int) string {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/environ")
	if err != nil {
		return ""
	}
	for _, kv := range strings.Split(string(raw), "\x00") {
		if v, ok := strings.CutPrefix(kv, "IAR_DATA_DIR="); ok {
			return v
		}
	}
	return ""
}

// idleExit confirms an idle shutdown under the client lock - the same
// lock ensureDaemon holds while adopting - so reaping cannot race a
// player that is starting up at this very moment (waking a laptop and
// starting the radio does exactly that: the resume-time idle check and
// the first client heartbeat land together). The client heartbeats
// under the lock before adopting, so by the time the lock is ours a
// just-arrived client is visible as a fresh heartbeat and the shutdown
// is called off. When the shutdown stands, the engine state record is
// retracted before the lock is released, so the next client finds no
// record and spawns a fresh daemon instead of adopting a dying one.
func idleExit(dir state.Dir, pid int, timeout time.Duration) bool {
	lock, err := dir.AcquireLock("engine-client.lock", true)
	if err != nil {
		// Cannot order against clients this tick; try again next tick
		// rather than reaping blind.
		return false
	}
	defer lock.Release()
	if dir.HeartbeatAge() <= timeout {
		return false
	}
	dir.RemoveEngineStateIf(pid)
	return true
}

// waitForPredecessors blocks until list reports no other engine daemons,
// ctx ends, or timeout elapses. It logs once when a wait begins and once
// when it resolves, so the log tells restart-overlap stories honestly.
func waitForPredecessors(ctx context.Context, timeout, poll time.Duration, log *slog.Logger, list func() []int) {
	pids := list()
	if len(pids) == 0 {
		return
	}
	log.Info("waiting for previous engine daemon to exit",
		"event", "daemon_predecessor_wait", "pids", fmt.Sprint(pids))
	start := time.Now()
	for time.Since(start) < timeout {
		select {
		case <-ctx.Done():
			return
		case <-time.After(poll):
		}
		if pids = list(); len(pids) == 0 {
			log.Info("previous engine daemon gone",
				"event", "daemon_predecessor_gone", "waited_seconds", time.Since(start).Seconds())
			return
		}
	}
	log.Warn("previous engine daemon still alive, proceeding anyway",
		"event", "daemon_predecessor_stuck", "pids", fmt.Sprint(pids),
		"waited_seconds", time.Since(start).Seconds())
}

// RunDaemon runs the engine as a long-lived daemon: it supervises the API
// server, records its address in the state file so player processes can
// adopt it across launches, and exits after IdleTimeout without clients.
// It returns nil immediately when a live daemon already exists.
func RunDaemon(ctx context.Context, cfg DaemonConfig, log *slog.Logger) error {
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 15 * time.Minute
	}

	// One daemon at a time: decide under the spawn lock.
	lock, err := cfg.StateDir.AcquireLock("engine-spawn.lock", false)
	if err != nil {
		if err == state.ErrLocked {
			log.Info("another engine daemon is starting, exiting", "event", "daemon_duplicate")
			return nil
		}
		return err
	}
	if st, ok := cfg.StateDir.ReadEngineState(); ok && state.PIDAlive(st.PID) {
		lock.Release()
		log.Info("engine daemon already running, exiting", "event", "daemon_duplicate", "pid", st.PID)
		return nil
	}
	port := cfg.Sidecar.Port
	if port == 0 {
		port, err = state.FreePort()
		if err != nil {
			lock.Release()
			return fmt.Errorf("allocating port: %w", err)
		}
		cfg.Sidecar.Port = port
	}
	st := state.EngineState{
		PID:       os.Getpid(),
		Port:      port,
		EngineDir: cfg.Sidecar.EngineDir,
		Started:   time.Now(),
	}
	if err := cfg.StateDir.WriteEngineState(st); err != nil {
		lock.Release()
		return fmt.Errorf("writing engine state: %w", err)
	}
	// Grace period before the first client heartbeat arrives.
	cfg.StateDir.Heartbeat()
	lock.Release()
	// Only ever retract OUR OWN record: a successor may already have
	// claimed the file by the time this daemon exits.
	defer cfg.StateDir.RemoveEngineStateIf(st.PID)

	log.Info("engine daemon starting", "event", "daemon_start", "pid", st.PID, "port", port, "idle_timeout", cfg.IdleTimeout.String())

	// A predecessor daemon may still be alive - superseded but not yet
	// exited, or terminated but slow tearing down its models. Loading
	// our models while it still holds several gigabytes of the card
	// turns a restart into a crash-loop of out-of-memory failures, so
	// wait for it to actually go away first. The wait is bounded: a
	// truly stuck predecessor should not keep the radio silent forever,
	// and the sidecar's own restart backoff copes if memory is still
	// short when we proceed.
	waitForPredecessors(ctx, 2*time.Minute, time.Second, log, func() []int {
		return StrayDaemons(os.Getpid(), os.Getenv("IAR_DATA_DIR"))
	})

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	client := NewClient(fmt.Sprintf("http://127.0.0.1:%d", port))
	sc := NewSidecar(cfg.Sidecar, client, log)
	sc.Start(runCtx)

	idle := time.NewTicker(30 * time.Second)
	defer idle.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Info("engine daemon stopping (signal)", "event", "daemon_stop", "reason", "signal")
			return nil
		case <-idle.C:
			// A daemon whose record now names a different, living
			// daemon has been superseded: nothing can reach it, so it
			// would otherwise hold its GPU memory forever. The client
			// heartbeat cannot catch this - it is one shared file that
			// every player refreshes, so an orphan looks busy for as
			// long as ANY player runs.
			if cur, ok := cfg.StateDir.ReadEngineState(); ok && cur.PID != st.PID && state.PIDAlive(cur.PID) {
				log.Info("engine daemon stopping (superseded)", "event", "daemon_stop",
					"reason", "superseded", "pid", st.PID, "now_serving", cur.PID)
				return nil
			}
			if age := cfg.StateDir.HeartbeatAge(); age > cfg.IdleTimeout {
				if !idleExit(cfg.StateDir, st.PID, cfg.IdleTimeout) {
					continue // a client arrived between the check and the lock
				}
				log.Info("engine daemon stopping (idle)", "event", "daemon_stop", "reason", "idle", "idle_seconds", age.Seconds())
				return nil
			}
		}
	}
}
