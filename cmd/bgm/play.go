package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"bgm/internal/audio"
	"bgm/internal/player"
	"bgm/internal/session"
	"bgm/internal/state"
	"bgm/internal/ui"
)

// runPlay starts the stream and the interactive interface.
func runPlay(pf playFlags) error {
	a, err := newApp(pf.verbose)
	if err != nil {
		return err
	}
	defer a.close()
	if pf.engine != "" {
		a.cfg.Engine = pf.engine
	}
	if pf.player != "" {
		a.cfg.Player = pf.player
	}

	// Only one player instance at a time; a second one would fight over
	// the session and the stream.
	playerLock, err := a.stateD.AcquireLock("player.lock", false)
	if err != nil {
		if err == state.ErrLocked {
			return fmt.Errorf("another bgm player is already running; steer the music there, or stop it first")
		}
		return err
	}
	defer playerLock.Release()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sess, err := a.initialSession(pf.preset, pf.session)
	if err != nil {
		return err
	}
	if a.cfg.Engine == "noise" && sess.Mode != session.ModeNoise {
		sess.Mode = session.ModeNoise
		sess.NoiseColor = sess.NoiseBed
	}

	eng, _, engineNote := a.buildEngine(ctx)
	builder := a.buildBuilder(ctx, pf.noLLM)

	pl, err := audio.NewPlayer(a.cfg.Player, pf.playerFile)
	if err != nil {
		return err
	}
	a.log.Info("player selected", "event", "player_selected", "backend", pl.Name())

	store := session.NewStore(a.paths.SessionsDir())
	orch := player.New(a.cfg, eng, builder, store, sess, pl, a.log)
	orch.Timings = a.timings
	orch.Start(ctx)
	defer orch.Close()

	ctrl := &ui.Controller{O: orch, ExportsDir: a.paths.ExportsDir()}
	if engineNote != "" {
		fmt.Fprintln(os.Stderr, "bgm:", engineNote)
	}
	if pf.plain || !isTerminal(os.Stdin) || !isTerminal(os.Stdout) {
		return ui.RunPlain(ctx, ctrl, os.Stdin, os.Stdout)
	}
	return ui.RunTUI(ctx, ctrl)
}
