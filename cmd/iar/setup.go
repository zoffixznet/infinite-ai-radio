package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"iar/internal/audio"
	"iar/internal/engine/acestep"
	"iar/internal/session"
)

// setupCommand performs first-run setup: engine checkout, Python
// environment and model weights. Safe to re-run; resumes interrupted work.
func setupCommand() *cobra.Command {
	var verbose bool
	var noBank bool
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "First-run setup: install the music engine and download models",
		Long: `Installs everything music generation needs: the uv Python manager
(user-level), the engine source at a pinned version, its Python
environment, and the model weights (about 18 GB on first run). Safe to
re-run at any time; an interrupted setup resumes where it stopped.
Never uses sudo.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := newApp(verbose)
			if err != nil {
				return err
			}
			defer a.close()

			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			fmt.Println("Setting up the music engine in", a.paths.EngineDir())
			fmt.Println("This is resumable: re-run 'iar setup' if it gets interrupted.")
			ins := acestep.NewInstaller(acestep.InstallConfig{
				EngineDir:   a.paths.EngineDir(),
				RepoURL:     a.cfg.ACEStep.RepoURL,
				Tag:         a.cfg.ACEStep.Tag,
				LMModelPath: a.cfg.ACEStep.LMModelPath,
			}, func(line string) { fmt.Println(line) })
			if err := ins.Run(ctx); err != nil {
				return err
			}
			if !noBank {
				if err := bankPresetTracks(ctx, a); err != nil {
					fmt.Println("note: pre-generating starter tracks failed:", err)
					fmt.Println("Infinite AI Radio still works; it will bank tracks while you listen.")
				}
			}
			fmt.Println("Setup finished. Run 'iar' to start playing.")
			return nil
		},
	}
	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "mirror logs to stderr")
	cmd.Flags().BoolVar(&noBank, "no-bank", false, "skip pre-generating starter tracks for the presets")
	return cmd
}

// bankPresetTracks pre-generates a couple of tracks per built-in music
// preset into the library, so first launches have instant audio.
func bankPresetTracks(ctx context.Context, a *app) error {
	lib := a.library()
	if lib == nil {
		return nil // library disabled in config
	}
	const perPreset = 2
	var todo []*session.Preset
	for _, p := range session.NewStore(a.paths.SessionsDir()).Presets() {
		if p.Mode == session.ModeMusic && lib.Count(p.Name) < perPreset {
			todo = append(todo, p)
		}
	}
	if len(todo) == 0 {
		fmt.Println("Starter track library already filled.")
		return nil
	}
	fmt.Println("Pre-generating starter tracks so launches begin playing instantly...")
	eng, remote, note := a.buildEngine(ctx, false)
	if eng == nil {
		return fmt.Errorf("%s", note)
	}
	if err := waitEngineReady(ctx, remote, a.timings); err != nil {
		return err
	}
	builder := a.buildBuilder(ctx, true) // deterministic prompts for banking
	done := 0
	total := 0
	for _, p := range todo {
		total += perPreset - lib.Count(p.Name)
	}
	for _, p := range todo {
		for lib.Count(p.Name) < perPreset {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			done++
			fmt.Printf("[%d/%d] generating a %s starter track...\n", done, total, p.Name)
			sess := session.FromPreset(p)
			spec := builder.BuildSpec(ctx, sess, 90)
			track, err := eng.Generate(ctx, spec)
			if err != nil {
				return fmt.Errorf("generating for %s: %w", p.Name, err)
			}
			if a.cfg.NormalizeLoudness {
				audio.NormalizeLoudness(track.Samples, audio.DefaultTargetRMS)
			}
			if err := lib.Put(p.Name, track); err != nil {
				return err
			}
		}
	}
	fmt.Println("Starter track library ready.")
	return nil
}
