package main

import (
	"context"
	"fmt"
	"iar/internal/config"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"iar/internal/audio"
	"iar/internal/mpris"
	"iar/internal/player"
	"iar/internal/remote"
	"iar/internal/session"
	"iar/internal/snippets"
	"iar/internal/state"
	"iar/internal/trackbuffer"
	"iar/internal/ui"
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
	// An explicit --remote means this machine is the station, not the
	// listening room: start the local speakers at zero instead of
	// blasting the first track into whatever room the server sits in.
	// Turn them up any time with the `volume` command; remote listeners
	// are unaffected (the stream taps the audio before the volume
	// control). Remote enabled via the config file does not silence
	// anything - plain `iar` stays a normal local player.
	if pf.remote {
		a.cfg.Volume = 0
	}

	// Only one player instance at a time; a second one would fight over
	// the session and the stream.
	playerLock, err := a.stateD.AcquireLock("player.lock", false)
	if err != nil {
		if err == state.ErrLocked {
			return fmt.Errorf("another Infinite AI Radio player is already running; steer the music there, or stop it first")
		}
		return err
	}
	defer playerLock.Release()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sess, err := a.initialSessionPrompt(pf.preset, pf.session, pf.prompt)
	if err != nil {
		return err
	}
	// A language switched off on the remote is a standing preference,
	// not a property of one session: a fresh session must not quietly
	// start singing in a language that was turned off. A saved session
	// that says otherwise keeps its own answer.
	if sess.Languages == nil && len(a.cfg.VocalLanguagesOff) > 0 {
		sess.Languages = map[string]bool{}
		for _, name := range a.cfg.VocalLanguagesOff {
			sess.Languages[name] = false
		}
	}
	if a.cfg.Engine == "noise" && sess.Mode != session.ModeNoise {
		sess.Mode = session.ModeNoise
		sess.NoiseColor = sess.NoiseBed
	}

	eng, _, engineNote := a.buildEngine(ctx)
	builder := a.buildBuilder(ctx, pf.noLLM)

	pl, err := audio.NewPlayer(audio.PlayerOptions{
		Kind:      a.cfg.Player,
		FilePath:  pf.playerFile,
		LatencyMS: a.cfg.PipeLatencyMS,
	})
	if err != nil {
		return err
	}
	a.log.Info("player selected", "event", "player_selected", "backend", pl.Name())

	store := session.NewStore(a.paths.SessionsDir())
	orch := player.New(a.cfg, eng, builder, store, sess, pl, a.log)
	orch.Timings = a.timings
	orch.Library = a.library()
	if a.cfg.Buffer.Phased {
		orch.Buffer = trackbuffer.New(filepath.Join(a.paths.DataDir, "buffer"), a.cfg.MP3Quality, a.log)
	}
	orch.SnippetsDir = a.snippetsDir()
	// Saves land here; say so up front instead of making the listener
	// dig the path out of a save acknowledgment or the docs.
	fmt.Fprintf(os.Stderr, "iar: saved tracks go to %s\n", orch.SnippetsDir)
	orch.Retention = time.Duration(a.cfg.Sessions.AutoRetentionDays) * 24 * time.Hour
	orch.StateDir = &a.stateD
	orch.SetLanguageStore(func(names, off []string) error {
		if err := config.SetVocalLanguages(a.paths, names); err != nil {
			return err
		}
		return config.SetVocalLanguagesOff(a.paths, off)
	})
	// Saved tracks from before tags existed move into the untagged
	// folder once.
	if moved, err := snippets.Migrate(orch.SnippetsDir); err != nil {
		a.log.Warn("snippet migration failed", "event", "snippets_migrate_failed", "error", err.Error())
	} else if moved > 0 {
		a.log.Info("snippets migrated", "event", "snippets_migrated", "moved", moved)
		fmt.Fprintf(os.Stderr, "iar: moved %d saved track(s) into %s\n", moved, filepath.Join(orch.SnippetsDir, snippets.Untagged))
	}

	// The phone remote taps the mastered PCM, so it must be wired
	// before the stream starts.
	var streamer *remote.Streamer
	if pf.remote || a.cfg.Remote.Enabled {
		streamer = remote.NewStreamer(a.log)
		orch.Tap = streamer
	}
	orch.Start(ctx)
	defer orch.Close()

	if streamer != nil {
		rs, err := remote.Start(ctx, a.remoteConfig(orch.SnippetsDir), orch, streamer, a.log)
		switch {
		case err != nil:
			fmt.Fprintln(os.Stderr, "iar: remote could not start:", err)
		case rs.TailnetIP != "":
			fmt.Fprintf(os.Stderr, "iar: remote at http://%s:%d (phone) and http://127.0.0.1:%d\n",
				rs.TailnetIP, a.cfg.Remote.Port, a.cfg.Remote.Port)
		default:
			fmt.Fprintf(os.Stderr, "iar: remote at http://127.0.0.1:%d (no Tailscale interface found; see 'iar doctor')\n",
				a.cfg.Remote.Port)
		}
		if err == nil && rs.NeedsSetup() {
			fmt.Fprintln(os.Stderr, "iar: the remote has no accounts yet; run 'iar remote setup' to create the first admin")
		}
	}

	// Desktop integration: optional, never fatal (headless setups have
	// no session bus).
	if mp, err := mpris.Start(ctx, orch, a.log); err != nil {
		a.log.Info("mpris unavailable", "event", "mpris_unavailable", "error", err.Error())
	} else {
		defer mp.Close()
	}

	ctrl := &ui.Controller{O: orch, ExportsDir: a.paths.ExportsDir()}
	if engineNote != "" {
		fmt.Fprintln(os.Stderr, "iar:", engineNote)
	}
	if pf.plain || !isTerminal(os.Stdin) || !isTerminal(os.Stdout) {
		return ui.RunPlain(ctx, ctrl, os.Stdin, os.Stdout)
	}
	return ui.RunTUI(ctx, ctrl)
}
