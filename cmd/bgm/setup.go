package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"bgm/internal/engine/acestep"
)

// setupCommand performs first-run setup: engine checkout, Python
// environment and model weights. Safe to re-run; resumes interrupted work.
func setupCommand() *cobra.Command {
	var verbose bool
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
			fmt.Println("This is resumable: re-run 'bgm setup' if it gets interrupted.")
			ins := acestep.NewInstaller(acestep.InstallConfig{
				EngineDir:   a.paths.EngineDir(),
				RepoURL:     a.cfg.ACEStep.RepoURL,
				Tag:         a.cfg.ACEStep.Tag,
				LMModelPath: a.cfg.ACEStep.LMModelPath,
			}, func(line string) { fmt.Println(line) })
			if err := ins.Run(ctx); err != nil {
				return err
			}
			fmt.Println("Setup finished. Run 'bgm' to start playing.")
			return nil
		},
	}
	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "mirror logs to stderr")
	return cmd
}
