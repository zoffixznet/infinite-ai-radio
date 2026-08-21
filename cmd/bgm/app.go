package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"bgm/internal/config"
	"bgm/internal/engine"
	"bgm/internal/engine/acestep"
	"bgm/internal/library"
	"bgm/internal/logging"
	"bgm/internal/prompting"
	"bgm/internal/session"
	"bgm/internal/state"
)

// app bundles everything the subcommands need.
type app struct {
	paths   config.Paths
	cfg     config.Config
	log     *slog.Logger
	closer  io.Closer
	stateD  state.Dir
	timings *state.Timings
}

// newApp resolves paths, loads config, opens the log and the state dir.
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
	stateD, err := state.NewDir(paths.DataDir)
	if err != nil {
		return nil, err
	}
	return &app{
		paths:   paths,
		cfg:     cfg,
		log:     logger,
		closer:  closer,
		stateD:  stateD,
		timings: state.NewTimings(stateD),
	}, nil
}

func (a *app) close() {
	if a.closer != nil {
		a.closer.Close()
	}
}

// daemonLogFile is where the shared engine daemon writes its output.
func (a *app) daemonLogFile() string {
	return filepath.Join(a.paths.LogsDir(), "engine-daemon.log")
}

// buildEngine constructs the configured music engine backed by the shared
// engine daemon (adopting a running one when possible), or returns nil
// with a user-facing explanation when it cannot run.
func (a *app) buildEngine(ctx context.Context) (engine.Engine, *acestep.Remote, string) {
	switch a.cfg.Engine {
	case "noise":
		return nil, nil, ""
	case "acestep":
		if !acestep.Installed(a.paths.EngineDir()) {
			return nil, nil, "music engine not installed; run 'bgm setup' (or 'make setup') first"
		}
		exe, err := os.Executable()
		if err != nil {
			return nil, nil, fmt.Sprintf("cannot locate the bgm executable: %v", err)
		}
		remote := acestep.NewRemote(a.stateD, exe, a.daemonLogFile(), a.log)
		remote.Start(ctx)
		eng := acestep.NewEngine(remote, acestep.Options{
			InferenceSteps: a.cfg.ACEStep.InferenceSteps,
			Thinking:       a.cfg.ACEStep.Thinking,
		})
		return eng, remote, ""
	default:
		return nil, nil, fmt.Sprintf("unknown engine %q in config; running noise only", a.cfg.Engine)
	}
}

// buildBuilder wires the prompt builder. Ollama usability is probed in
// the background; nothing ever waits for it.
func (a *app) buildBuilder(ctx context.Context, noLLM bool) *prompting.Builder {
	var oll *prompting.Ollama
	if a.cfg.Ollama.Enabled && !noLLM {
		oll = prompting.NewOllama(a.cfg.Ollama.URL, a.cfg.Ollama.Model)
	}
	b := prompting.NewBuilder(oll, a.log)
	b.ProbeAsync(ctx)
	return b
}

// initialSession picks the starting session from flags.
func (a *app) initialSession(presetName, sessionName string) (*session.Session, error) {
	return a.initialSessionPrompt(presetName, sessionName, "")
}

// initialSessionPrompt picks the starting session from flags, including
// prompt-first startup.
func (a *app) initialSessionPrompt(presetName, sessionName, prompt string) (*session.Session, error) {
	store := session.NewStore(a.paths.SessionsDir())
	picked := 0
	for _, v := range []string{presetName, sessionName, prompt} {
		if v != "" {
			picked++
		}
	}
	if picked > 1 {
		return nil, fmt.Errorf("use only one of --preset, --session, or a prompt")
	}
	switch {
	case prompt != "":
		return prompting.SessionFromPrompt(prompt), nil
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

// engineWaitBudget is how long commands wait for engine readiness before
// giving up with a pointer to doctor.
const engineWaitBudget = 45 * time.Minute

// resolvePathsEnsured resolves and creates the application directories.
func resolvePathsEnsured() (config.Paths, error) {
	paths, err := config.ResolvePaths()
	if err != nil {
		return config.Paths{}, err
	}
	return paths, paths.EnsureDirs()
}

// loadConfig loads the configuration for processes that do not need the
// full app wiring (the engine daemon).
func loadConfig(paths config.Paths) (config.Config, error) {
	return config.Load(paths)
}

// library returns the on-disk track cache (nil when disabled).
func (a *app) library() *library.Library {
	return library.New(filepath.Join(a.paths.DataDir, "library"), a.cfg.LibraryMaxMB, a.log)
}

// snippetsDir resolves where saved tracks land.
func (a *app) snippetsDir() string {
	if a.cfg.SnippetsDir != "" {
		return a.cfg.SnippetsDir
	}
	return filepath.Join(a.paths.DataDir, "snippets")
}
