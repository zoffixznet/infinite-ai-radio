package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"bgm/internal/engine/acestep"
	"bgm/internal/export"
	"bgm/internal/session"
)

// cmdExport renders an MP3 headlessly (no playback).
func cmdExport(args []string) error {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	minutes := fs.Int("minutes", 0, "how many minutes to render (required)")
	out := fs.String("out", "", "output MP3 path (default: exports directory)")
	presetName := fs.String("preset", "", "render a built-in preset")
	sessionName := fs.String("session", "", "render a saved session")
	noLLM := fs.Bool("no-llm", false, "disable Ollama-assisted prompt rewriting")
	verbose := fs.Bool("verbose", false, "mirror logs to stderr")
	fs.Parse(args)

	if *minutes < 1 {
		return fmt.Errorf("--minutes is required (1-%d)", export.MaxMinutes)
	}
	a, err := newApp(*verbose)
	if err != nil {
		return err
	}
	defer a.close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sess, err := a.initialSession(*presetName, *sessionName)
	if err != nil {
		return err
	}

	var eng *acestep.Engine
	if sess.Mode == session.ModeMusic {
		e, note := a.buildEngine(ctx)
		if e == nil {
			if note == "" {
				note = "music engine unavailable"
			}
			return fmt.Errorf("%s", note)
		}
		eng = e.(*acestep.Engine)
		fmt.Println("starting the music engine (first start loads models; this can take a few minutes)...")
		if err := eng.Sidecar().WaitReady(ctx); err != nil {
			return err
		}
	}

	outPath := *out
	if outPath == "" {
		outPath = export.DefaultPath(a.paths.ExportsDir(), sess.Name, *minutes)
	}
	r := &export.Renderer{
		Builder:          a.buildBuilder(ctx, *noLLM),
		TrackSeconds:     a.cfg.TrackSeconds,
		CrossfadeSeconds: a.cfg.CrossfadeSeconds,
		Log:              a.log,
		Progress:         func(line string) { fmt.Println(line) },
	}
	if eng != nil {
		r.Engine = eng
	}
	if err := r.Render(ctx, sess, *minutes, outPath); err != nil {
		return err
	}
	fmt.Println(outPath)
	return nil
}
