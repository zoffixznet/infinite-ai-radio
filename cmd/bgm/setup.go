package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"bgm/internal/config"
	"bgm/internal/engine/acestep"
)

// cmdSetup performs first-run setup: engine checkout, Python environment
// and model weights. It is safe to re-run and resumes interrupted work.
func cmdSetup(args []string) error {
	fs := flag.NewFlagSet("setup", flag.ExitOnError)
	fs.Parse(args)

	paths, err := config.ResolvePaths()
	if err != nil {
		return err
	}
	if err := paths.EnsureDirs(); err != nil {
		return err
	}
	cfg, err := config.Load(paths)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Println("Setting up the music engine in", paths.EngineDir())
	fmt.Println("This is resumable: re-run 'bgm setup' if it gets interrupted.")
	ins := acestep.NewInstaller(acestep.InstallConfig{
		EngineDir:   paths.EngineDir(),
		RepoURL:     cfg.ACEStep.RepoURL,
		Tag:         cfg.ACEStep.Tag,
		LMModelPath: cfg.ACEStep.LMModelPath,
	}, func(line string) { fmt.Println(line) })
	if err := ins.Run(ctx); err != nil {
		return err
	}
	fmt.Println("Setup finished. Run 'bgm' to start playing.")
	return nil
}
