package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"iar/internal/engine/acestep"
	"iar/internal/logging"
	"iar/internal/state"
)

// engineCommand groups the shared engine daemon controls.
func engineCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "engine",
		Short: "Manage the shared music engine daemon",
		Long: `The music engine runs as a shared background daemon so restarting it
does not reload models. It shuts down on its own after being idle.`,
	}
	cmd.AddCommand(engineStatusCommand(), engineStopCommand(), engineDaemonCommand())
	return cmd
}

func engineStatusCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show whether the engine daemon is running and ready",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := newApp(false)
			if err != nil {
				return err
			}
			defer a.close()
			st, ok := a.stateD.ReadEngineState()
			if !ok || !state.PIDAlive(st.PID) {
				fmt.Println("engine daemon: not running (starts automatically with 'iar')")
				return nil
			}
			fmt.Printf("engine daemon: running (pid %d, port %d, up %s)\n",
				st.PID, st.Port, time.Since(st.Started).Round(time.Second))
			hctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Second)
			defer cancel()
			h, err := acestep.NewClient(fmt.Sprintf("http://127.0.0.1:%d", st.Port)).Health(hctx)
			switch {
			case err != nil:
				fmt.Println("engine api:    starting (not answering yet)")
			case h.ModelsInitialized:
				fmt.Printf("engine api:    ready (model %s)\n", h.LoadedModel)
			default:
				fmt.Println("engine api:    loading models")
			}
			fmt.Printf("last client:   %s ago\n", a.stateD.HeartbeatAge().Round(time.Second))
			return nil
		},
	}
}

func engineStopCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the engine daemon and free GPU memory",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := newApp(false)
			if err != nil {
				return err
			}
			defer a.close()
			st, ok := a.stateD.ReadEngineState()
			if !ok || !state.PIDAlive(st.PID) {
				if ok {
					a.stateD.RemoveEngineStateIf(st.PID)
				}
				fmt.Println("engine daemon is not running")
				return nil
			}
			if err := state.Terminate(st.PID); err != nil {
				return fmt.Errorf("stopping daemon pid %d: %w", st.PID, err)
			}
			for i := 0; i < 30; i++ {
				if !state.PIDAlive(st.PID) {
					fmt.Println("engine daemon stopped")
					return nil
				}
				time.Sleep(500 * time.Millisecond)
			}
			return fmt.Errorf("daemon pid %d did not exit; check 'iar doctor'", st.PID)
		},
	}
}

// engineDaemonCommand is the hidden entry point the player spawns; it runs
// the supervised engine in the foreground of a detached process.
func engineDaemonCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "daemon",
		Short:  "Run the engine daemon in the foreground (started automatically)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runEngineDaemon()
		},
	}
}

func runEngineDaemon() error {
	paths, err := resolvePathsEnsured()
	if err != nil {
		return err
	}
	cfg, err := loadConfig(paths)
	if err != nil {
		return err
	}
	// The daemon logs JSON to stdout, which the spawner redirects into
	// the engine daemon log file; there it writes through a rotating
	// file instead, so the log stays under its cap.
	out, closeLog := daemonLogOutput(daemonLogPath(paths), cfg.EngineLogMaxBytes())
	defer closeLog()
	logger := slog.New(slog.NewJSONHandler(out, &slog.HandlerOptions{Level: slog.LevelDebug}))

	stateD, err := state.NewDir(paths.DataDir)
	if err != nil {
		return err
	}
	if !acestep.Installed(paths.EngineDir()) {
		logger.Error("engine not installed", "event", "daemon_no_install")
		return fmt.Errorf("music engine not installed; run 'iar setup' first")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return acestep.RunDaemon(ctx, acestep.DaemonConfig{
		StateDir: stateD,
		Sidecar: acestep.SidecarConfig{
			EngineDir:       paths.EngineDir(),
			Port:            cfg.ACEStep.Port,
			LMModelPath:     cfg.ACEStep.LMModelPath,
			LMBackend:       cfg.ACEStep.LMBackend,
			OffloadDIT:      cfg.ACEStep.OffloadDIT,
			OffloadDITDisk:  true,
			MaxTrackSeconds: cfg.ACEStep.MaxTrackSeconds,
		},
		IdleTimeout: time.Duration(cfg.ACEStep.IdleMinutes) * time.Minute,
	}, logger)
}

// daemonLogOutput is where the engine daemon writes its log. Spawned by
// the radio, its stdout is the engine daemon log itself; then the log is
// opened as a rotating file, and every rotation also moves stdout and
// stderr onto the fresh file, so a crash report lands next to the lines
// before it. Run by hand with stdout elsewhere, it logs to stdout as
// before.
func daemonLogOutput(path string, maxBytes int64) (io.Writer, func()) {
	plain := func() {}
	out, err := os.Stdout.Stat()
	if err != nil || !out.Mode().IsRegular() {
		return os.Stdout, plain
	}
	onDisk, err := os.Stat(path)
	if err != nil || !os.SameFile(out, onDisk) {
		return os.Stdout, plain
	}
	r, err := logging.OpenRotating(path, maxBytes)
	if err != nil {
		return os.Stdout, plain
	}
	r.OnRotate = redirectStdio
	// Opening may have rotated a log already over its cap.
	redirectStdio(r.File())
	return r, func() { r.Close() }
}
