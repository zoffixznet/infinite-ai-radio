package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"bgm/internal/audio"
	"bgm/internal/player"
	"bgm/internal/session"
	"bgm/internal/ui"
)

// cmdPlay starts the stream and the interactive interface.
func cmdPlay(args []string) error {
	fs := flag.NewFlagSet("play", flag.ExitOnError)
	presetName := fs.String("preset", "", "start from a built-in preset")
	sessionName := fs.String("session", "", "resume a saved session by name")
	engineFlag := fs.String("engine", "", "generation engine: acestep or noise")
	playerFlag := fs.String("player", "", "audio backend: auto, pipe, null or file")
	playerFile := fs.String("player-file", "", "output path for the file backend")
	plain := fs.Bool("plain", false, "plain line-based interface (no full-screen UI)")
	noLLM := fs.Bool("no-llm", false, "disable Ollama-assisted prompt rewriting")
	verbose := fs.Bool("verbose", false, "mirror logs to stderr")
	fs.Parse(args)

	a, err := newApp(*verbose)
	if err != nil {
		return err
	}
	defer a.close()
	if *engineFlag != "" {
		a.cfg.Engine = *engineFlag
	}
	if *playerFlag != "" {
		a.cfg.Player = *playerFlag
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sess, err := a.initialSession(*presetName, *sessionName)
	if err != nil {
		return err
	}
	if a.cfg.Engine == "noise" && sess.Mode != session.ModeNoise {
		sess.Mode = session.ModeNoise
		sess.NoiseColor = sess.NoiseBed
	}

	eng, engineNote := a.buildEngine(ctx)
	builder := a.buildBuilder(ctx, *noLLM)

	pl, err := audio.NewPlayer(a.cfg.Player, *playerFile)
	if err != nil {
		return err
	}
	a.log.Info("player selected", "event", "player_selected", "backend", pl.Name())

	store := session.NewStore(a.paths.SessionsDir())
	orch := player.New(a.cfg, eng, builder, store, sess, pl, a.log)
	orch.Start(ctx)
	defer orch.Close()

	ctrl := &ui.Controller{O: orch, ExportsDir: a.paths.ExportsDir()}
	if engineNote != "" {
		fmt.Fprintln(os.Stderr, "bgm:", engineNote)
	}
	if *plain || !isTerminal(os.Stdin) || !isTerminal(os.Stdout) {
		return ui.RunPlain(ctx, ctrl, os.Stdin, os.Stdout)
	}
	return ui.RunTUI(ctx, ctrl)
}
