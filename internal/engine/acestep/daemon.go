package acestep

import (
	"context"
	"fmt"
	"log/slog"
	"os"
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
	defer cfg.StateDir.RemoveEngineState()

	log.Info("engine daemon starting", "event", "daemon_start", "pid", st.PID, "port", port, "idle_timeout", cfg.IdleTimeout.String())

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
			if age := cfg.StateDir.HeartbeatAge(); age > cfg.IdleTimeout {
				log.Info("engine daemon stopping (idle)", "event", "daemon_stop", "reason", "idle", "idle_seconds", age.Seconds())
				return nil
			}
		}
	}
}
