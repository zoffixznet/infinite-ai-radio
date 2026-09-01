package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"iar/internal/export"
	"iar/internal/session"
	"iar/internal/state"
)

// exportCommand renders an MP3 headlessly (no playback).
func exportCommand() *cobra.Command {
	var (
		minutes     int
		out         string
		presetName  string
		sessionName string
		noLLM       bool
		verbose     bool
	)
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Render minutes of music to an MP3 file",
		Long: `Renders music matching a preset or saved session into an MP3 file
without playing anything. The engine daemon is started (or reused) as
needed.`,
		Example: `  iar export --minutes 20 --preset sleep
  iar export --minutes 30 --session gym-grind --out ~/Music/grind.mp3`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if minutes < 1 {
				return fmt.Errorf("--minutes is required (1-%d)", export.MaxMinutes)
			}
			a, err := newApp(verbose)
			if err != nil {
				return err
			}
			defer a.close()

			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			sess, err := a.initialSession(presetName, sessionName)
			if err != nil {
				return err
			}

			r := &export.Renderer{
				Builder:          a.buildBuilder(ctx, noLLM),
				TrackSeconds:     a.cfg.TrackSeconds,
				CrossfadeSeconds: a.cfg.CrossfadeSeconds,
				MP3Quality:       a.cfg.MP3Quality,
				Log:              a.log,
				Progress:         func(line string) { fmt.Println(line) },
			}
			if sess.Mode == session.ModeMusic {
				eng, remote, note := a.buildEngine(ctx, false)
				if eng == nil {
					if note == "" {
						note = "music engine unavailable"
					}
					return fmt.Errorf("%s", note)
				}
				if err := waitEngineReady(ctx, remote, a.timings); err != nil {
					return err
				}
				r.Engine = eng
			}

			outPath := out
			if outPath == "" {
				outPath = export.DefaultPath(a.paths.ExportsDir(), sess.Name, minutes)
			}
			if err := r.Render(ctx, sess, minutes, outPath); err != nil {
				return err
			}
			fmt.Println(outPath)
			return nil
		},
	}
	fl := cmd.Flags()
	fl.IntVarP(&minutes, "minutes", "m", 0, "how many minutes to render (required)")
	fl.StringVarP(&out, "out", "o", "", "output MP3 path (default: exports directory)")
	fl.StringVar(&presetName, "preset", "", "render a built-in preset (see 'iar presets')")
	fl.StringVar(&sessionName, "session", "", "render a saved session")
	fl.BoolVar(&noLLM, "no-llm", false, "disable Ollama-assisted prompt rewriting")
	fl.BoolVarP(&verbose, "verbose", "v", false, "mirror logs to stderr")
	cmd.MarkFlagRequired("minutes")
	return cmd
}

// engineWaiter is the readiness surface exports wait on.
type engineWaiter interface {
	Ready() bool
	Phase() string
	Adopted() bool
}

// waitEngineReady blocks until the engine is ready, printing progress.
func waitEngineReady(ctx context.Context, w engineWaiter, timings *state.Timings) error {
	if w.Ready() {
		fmt.Println("engine ready (reusing the running engine)")
		return nil
	}
	fmt.Println("starting the music engine (reused across runs; stop with 'iar engine stop')")
	start := time.Now()
	deadline := time.Now().Add(engineWaitBudget)
	lastLine := time.Time{}
	for time.Now().Before(deadline) {
		if w.Ready() {
			fmt.Printf("engine ready after %s\n", time.Since(start).Round(time.Second))
			return nil
		}
		if time.Since(lastLine) >= 2*time.Second {
			phase := w.Phase()
			expected := timings.Expected(phaseKey(phase))
			fmt.Printf("engine: %s... %s (usually ~%s)\n",
				phase, time.Since(start).Round(time.Second), expected.Round(time.Second))
			lastLine = time.Now()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("engine did not become ready; check 'iar doctor'")
}

// phaseKey maps a display phase to its timings key.
func phaseKey(phase string) string {
	switch phase {
	case "loading models":
		return state.PhaseModelLoad
	default:
		return state.PhaseEngineStart
	}
}
