package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"iar/internal/export"
	"iar/internal/prompting"
	"iar/internal/session"
	"iar/internal/state"
)

// exportCommand renders an MP3 headlessly (no playback).
func exportCommand() *cobra.Command {
	var (
		minutes     int
		songs       int
		out         string
		presetName  string
		sessionName string
		language    string
		noLLM       bool
		verbose     bool
	)
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Render whole songs to an MP3 file",
		Long: `Renders music matching a preset or saved session into an MP3 file
without playing anything, on the same path the radio itself uses: the
lyric writer pens each vocal song's words first, and every song is
planned and rendered whole - its length follows its lyrics, and nothing
is cut mid-song. --minutes keeps adding songs until at least that much
music exists; --songs renders an exact count. The engine daemon is
started (or reused) as needed.`,
		Example: `  iar export --minutes 20 --preset sleep
  iar export --songs 1 --preset nu-metal --out banger.mp3
  iar export --songs 3 --preset hard-rock --language es
  iar export --minutes 30 --session gym-grind --out ~/Music/grind.mp3`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if minutes < 1 && songs < 1 {
				return fmt.Errorf("--minutes or --songs is required")
			}
			if minutes > 0 && songs > 0 {
				return fmt.Errorf("ask for --minutes or --songs, not both")
			}
			a, err := newApp(verbose)
			if err != nil {
				return err
			}
			defer a.close()

			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			sess, err := a.initialSession(presetName, sessionName)
			if err != nil {
				return err
			}
			if language != "" {
				code, err := resolveLanguage(language)
				if err != nil {
					return err
				}
				// Pinned, exactly like naming a language by hand while
				// listening: without the pin the lyric writer keeps
				// drawing from the configured language list instead.
				sess.Spec.VocalLanguage = code
				sess.Spec.LanguagePinned = true
			}

			bld := a.buildBuilder(ctx, noLLM)
			r := &export.Renderer{
				Builder:          bld,
				TrackSeconds:     a.cfg.TrackSeconds,
				CrossfadeSeconds: a.cfg.CrossfadeSeconds,
				MP3Quality:       a.cfg.MP3Quality,
				Log:              a.log,
				Progress:         func(line string) { fmt.Println(line) },
			}
			if sess.Mode == session.ModeMusic {
				// The radio's own order: the writer gets the card to
				// itself for every song's words, and only then does
				// the engine wake to plan and render them.
				if sess.Vocal {
					fmt.Println("waking the lyric writer")
					if bld.AwaitHelper(ctx, 90*time.Second) {
						want := songs
						if want == 0 {
							want = minutes*60/a.cfg.TrackSeconds + 1
						}
						fmt.Printf("writing the words for %d song(s) before the engine starts\n", want)
						if wrote := bld.StockLyrics(ctx, sess, want, nil); wrote == 0 {
							fmt.Println("the lyric writer wrote nothing; the engine will write its own words")
						}
					} else {
						fmt.Println("no lyric writer available; the engine will write its own words")
					}
				}
				eng, remote, note := a.buildEngine(ctx, false)
				if eng == nil {
					if note == "" {
						note = "music engine unavailable"
					}
					return fmt.Errorf("%s", note)
				}
				if err := waitEngineReady(ctx, remote, a.timings); err != nil {
					return err
				}
				// Any mid-render lyric top-up runs beside the engine,
				// so the helper keeps off the card from here on.
				bld.SetEngineBusy(true)
				r.Engine = eng
			}

			outPath := out
			if outPath == "" {
				outPath = export.DefaultPath(a.paths.ExportsDir(), sess.Name, minutes, songs)
			}
			req := export.Request{Minutes: minutes, Songs: songs, OutPath: outPath}
			if err := r.Render(ctx, sess, req); err != nil {
				return err
			}
			fmt.Println(outPath)
			return nil
		},
	}
	fl := cmd.Flags()
	fl.IntVarP(&minutes, "minutes", "m", 0, "render whole songs until at least this many minutes")
	fl.IntVarP(&songs, "songs", "n", 0, "render exactly this many whole songs")
	fl.StringVar(&language, "language", "", "sing in this language (a name like Spanish, or a code like es)")
	fl.StringVarP(&out, "out", "o", "", "output MP3 path (default: exports directory)")
	fl.StringVar(&presetName, "preset", "", "render a built-in preset (see 'iar presets')")
	fl.StringVar(&sessionName, "session", "", "render a saved session")
	fl.BoolVar(&noLLM, "no-llm", false, "disable Ollama-assisted prompt rewriting")
	fl.BoolVarP(&verbose, "verbose", "v", false, "mirror logs to stderr")
	return cmd
}

// resolveLanguage turns a language name or code into the engine's tag.
// Codes the engine does not list pass through as written: the tag is
// advisory and the lyric writer follows it either way.
func resolveLanguage(v string) (string, error) {
	in := strings.TrimSpace(v)
	for _, l := range prompting.EngineLanguages {
		if strings.EqualFold(l.Name, in) || strings.EqualFold(l.Code, in) {
			return l.Code, nil
		}
	}
	if len(in) > 0 && len(in) <= 3 {
		return strings.ToLower(in), nil
	}
	return "", fmt.Errorf("unknown language %q; use a name like Spanish or a code like es", v)
}

// engineWaiter is the readiness surface exports wait on.
type engineWaiter interface {
	Ready() bool
	Phase() string
	Adopted() bool
}

// waitEngineReady blocks until the engine is ready, printing progress.
func waitEngineReady(ctx context.Context, w engineWaiter, timings *state.Timings) error {
	if w.Ready() {
		fmt.Println("engine ready (reusing the running engine)")
		return nil
	}
	fmt.Println("starting the music engine (reused across runs; stop with 'iar engine stop')")
	start := time.Now()
	deadline := time.Now().Add(engineWaitBudget)
	lastLine := time.Time{}
	for time.Now().Before(deadline) {
		if w.Ready() {
			fmt.Printf("engine ready after %s\n", time.Since(start).Round(time.Second))
			return nil
		}
		if time.Since(lastLine) >= 2*time.Second {
			phase := w.Phase()
			expected := timings.Expected(phaseKey(phase))
			fmt.Printf("engine: %s... %s (usually ~%s)\n",
				phase, time.Since(start).Round(time.Second), expected.Round(time.Second))
			lastLine = time.Now()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("engine did not become ready; check 'iar doctor'")
}

// phaseKey maps a display phase to its timings key.
func phaseKey(phase string) string {
	switch phase {
	case "loading models":
		return state.PhaseModelLoad
	default:
		return state.PhaseEngineStart
	}
}
