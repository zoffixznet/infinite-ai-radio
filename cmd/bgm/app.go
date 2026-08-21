package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"bgm/internal/config"
	"bgm/internal/engine"
	"bgm/internal/engine/acestep"
	"bgm/internal/logging"
	"bgm/internal/prompting"
	"bgm/internal/session"
)

// app bundles everything the subcommands need.
type app struct {
	paths  config.Paths
	cfg    config.Config
	log    *slog.Logger
	closer io.Closer
}

// newApp resolves paths, loads config and opens the log.
func newApp(verbose bool) (*app, error) {
	paths, err := config.ResolvePaths()
	if err != nil {
		return nil, err
	}
	if err := paths.EnsureDirs(); err != nil {
		return nil, err
	}
	cfg, err := config.Load(paths)
	if err != nil {
		return nil, err
	}
	logger, closer, err := logging.Setup(paths.LogFile(), verbose)
	if err != nil {
		return nil, err
	}
	return &app{paths: paths, cfg: cfg, log: logger, closer: closer}, nil
}

func (a *app) close() {
	if a.closer != nil {
		a.closer.Close()
	}
}

// buildEngine constructs the configured music engine, or returns nil (with
// a user-facing explanation) when it cannot run.
func (a *app) buildEngine(ctx context.Context) (engine.Engine, string) {
	switch a.cfg.Engine {
	case "noise":
		return nil, ""
	case "acestep":
		if !acestep.Installed(a.paths.EngineDir()) {
			return nil, "music engine not installed; run 'bgm setup' (or 'make setup') first"
		}
		baseURL := fmt.Sprintf("http://127.0.0.1:%d", a.cfg.ACEStep.Port)
		client := acestep.NewClient(baseURL)
		sidecar := acestep.NewSidecar(acestep.SidecarConfig{
			EngineDir:   a.paths.EngineDir(),
			Port:        a.cfg.ACEStep.Port,
			LMModelPath: a.cfg.ACEStep.LMModelPath,
		}, client, a.log)
		sidecar.Start(ctx)
		eng := acestep.NewEngine(client, sidecar, acestep.Options{
			InferenceSteps: a.cfg.ACEStep.InferenceSteps,
			Thinking:       a.cfg.ACEStep.Thinking,
		})
		return eng, ""
	default:
		return nil, fmt.Sprintf("unknown engine %q in config; running noise only", a.cfg.Engine)
	}
}

// buildBuilder wires the prompt builder, probing Ollama briefly when
// enabled.
func (a *app) buildBuilder(ctx context.Context, noLLM bool) *prompting.Builder {
	var oll *prompting.Ollama
	if a.cfg.Ollama.Enabled && !noLLM {
		candidate := prompting.NewOllama(a.cfg.Ollama.URL, a.cfg.Ollama.Model)
		probe, cancel := context.WithTimeout(ctx, 2*time.Second)
		if candidate.Available(probe) {
			oll = candidate
			a.log.Info("ollama available", "event", "ollama_available", "model", candidate.Model())
		} else {
			a.log.Info("ollama unavailable, using deterministic prompts", "event", "ollama_unavailable")
		}
		cancel()
	}
	return prompting.NewBuilder(oll, a.log)
}

// initialSession picks the starting session from flags.
func (a *app) initialSession(presetName, sessionName string) (*session.Session, error) {
	store := session.NewStore(a.paths.SessionsDir())
	switch {
	case presetName != "" && sessionName != "":
		return nil, fmt.Errorf("use either --preset or --session, not both")
	case presetName != "":
		p, err := session.LookupPreset(presetName)
		if err != nil {
			return nil, err
		}
		return session.FromPreset(p), nil
	case sessionName != "":
		return store.Load(sessionName)
	default:
		return session.New(), nil
	}
}

// isTerminal reports whether f is an interactive terminal.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
