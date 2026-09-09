package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"iar/internal/accounts"
	"iar/internal/config"
	"iar/internal/engine"
	"iar/internal/engine/acestep"
	"iar/internal/library"
	"iar/internal/logging"
	"iar/internal/mail"
	"iar/internal/prompting"
	"iar/internal/remote"
	"iar/internal/session"
	"iar/internal/state"
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
// buildEngine wires the engine backend. dormant starts the remote
// inactive: the daemon is neither spawned nor kept alive until the
// caller activates it - the phased player does this so a launch with a
// healthy disk buffer plays for free without waking the engine.
func (a *app) buildEngine(ctx context.Context, dormant bool) (engine.Engine, *acestep.Remote, string) {
	switch a.cfg.Engine {
	case "noise":
		return nil, nil, ""
	case "acestep":
		if !acestep.Installed(a.paths.EngineDir()) {
			return nil, nil, "music engine not installed; run 'iar setup' (or 'make setup') first"
		}
		exe, err := os.Executable()
		if err != nil {
			return nil, nil, fmt.Sprintf("cannot locate the iar executable: %v", err)
		}
		remote := acestep.NewRemote(a.stateD, exe, a.daemonLogFile(), a.log)
		if dormant {
			remote.SetActive(false)
		}
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
		oll = prompting.NewOllama(a.cfg.Ollama.URL, a.cfg.Ollama.Model, a.cfg.Ollama.GPULayers)
	}
	b := prompting.NewBuilder(oll, a.log)
	if name := a.cfg.LyricsGenerator; name != "" {
		if _, ok := prompting.GeneratorByName(name); ok {
			b.SetDefaultGenerator(name)
		} else {
			a.log.Warn("unknown lyrics_generator in config; using the default",
				"configured", name, "default", prompting.DefaultGeneratorName)
		}
	}
	b.SetLanguages(a.cfg.VocalLanguages)
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
		p, err := store.LookupPreset(presetName)
		if err != nil {
			return nil, err
		}
		return session.FromPreset(p), nil
	case sessionName != "":
		return store.Load(sessionName)
	default:
		// A restart picks up where the radio left off, named session or
		// not: the sound someone settled on is the sound they expect to
		// hear when the machine comes back, and every change to it is
		// branched rather than overwritten, so there is always a
		// session worth resuming.
		switch s, err := resumeSession(store, a.stateD); {
		case s != nil:
			return s, nil
		case err != nil && errors.Is(err, session.ErrNotFound):
			a.log.Info("the last session is gone; starting fresh",
				"event", "session_resume_missing", "error", err.Error())
		case err != nil:
			a.log.Warn("could not resume the last session",
				"event", "session_resume_failed", "error", err.Error())
		}
		// A cold start plays the configured preset. A preset that has
		// been hidden or renamed must not stop the radio booting, so a
		// miss falls back to the built-in sound.
		if name := a.cfg.DefaultPreset; name != "" {
			if p, err := store.LookupPreset(name); err == nil {
				return session.FromPreset(p), nil
			} else {
				a.log.Warn("unknown default_preset in config; starting from the built-in sound",
					"event", "default_preset_missing", "preset", name, "error", err.Error())
			}
		}
		return session.New(), nil
	}
}

// resumeSession loads the session the last player was playing, so a
// restart carries on with the sound someone settled on rather than
// inventing a new one. A nil session with a nil error means there is
// nothing recorded to resume - a first run, or a machine whose only
// runs could not make music.
func resumeSession(store *session.Store, stateD state.Dir) (*session.Session, error) {
	cs, _ := stateD.ReadCurrentSession()
	if cs.Name == "" {
		return nil, nil
	}
	s, err := store.Load(cs.Name)
	if err != nil {
		return nil, err
	}
	return s, nil
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
	return a.paths.SnippetsDir()
}

// accountStores opens the remote's users and login-session files.
func (a *app) accountStores() (*accounts.Store, *accounts.Sessions) {
	return accounts.NewStore(a.paths.UsersFile()),
		accounts.NewSessions(a.paths.SessionsFile(), accounts.DefaultSessionTTL)
}

// remoteConfig assembles the remote server configuration.
func (a *app) remoteConfig(snippetsDir string) remote.Config {
	users, sessions := a.accountStores()
	return remote.Config{
		Port:         a.cfg.Remote.Port,
		Binds:        a.cfg.Remote.Bind,
		AllowedHosts: a.cfg.Remote.AllowedHosts,
		Users:        users,
		Sessions:     sessions,
		Mailer:       mail.New(a.cfg.Remote.SMTP),
		SnippetsDir:  snippetsDir,
		Version:      version,
	}
}
