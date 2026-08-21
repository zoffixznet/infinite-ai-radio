// Command bgm plays a continuous stream of AI-generated background music
// using models that run entirely on the local machine.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"bgm/internal/session"
)

// version is stamped at build time via -ldflags.
var version = "dev"

// playFlags are shared by the root (play) command.
type playFlags struct {
	preset     string
	session    string
	engine     string
	player     string
	playerFile string
	plain      bool
	noLLM      bool
	verbose    bool
}

func main() {
	if err := rootCommand().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "bgm:", err)
		os.Exit(1)
	}
}

func rootCommand() *cobra.Command {
	var pf playFlags
	root := &cobra.Command{
		Use:   "bgm",
		Short: "Endless AI background music, generated locally",
		Long: `bgm plays a continuous stream of AI-generated background music using
models running entirely on your machine. Run it with no arguments to
start playing; type plain English while it plays to steer the stream.

Built-in presets (start with --preset, list with 'bgm presets'):
` + presetLines(),
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPlay(pf)
		},
	}
	fl := root.Flags()
	fl.StringVar(&pf.preset, "preset", "", "start from a built-in preset (see 'bgm presets')")
	fl.StringVar(&pf.session, "session", "", "resume a saved session by name")
	fl.StringVar(&pf.engine, "engine", "", "generation engine: acestep or noise")
	fl.StringVar(&pf.player, "player", "", "audio backend: auto, pipe, null or file")
	fl.StringVar(&pf.playerFile, "player-file", "", "output path for the file backend")
	fl.BoolVar(&pf.plain, "plain", false, "plain line-based interface (no full-screen UI)")
	fl.BoolVar(&pf.noLLM, "no-llm", false, "disable Ollama-assisted prompt rewriting")
	fl.BoolVarP(&pf.verbose, "verbose", "v", false, "mirror logs to stderr")

	root.AddCommand(
		setupCommand(),
		exportCommand(),
		sessionsCommand(),
		presetsCommand(),
		doctorCommand(),
		engineCommand(),
		versionCommand(),
	)
	return root
}

func versionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the bgm version",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Println("bgm", version)
		},
	}
}

// presetLines renders the embedded presets for help output.
func presetLines() string {
	out := ""
	for _, p := range session.Presets() {
		out += fmt.Sprintf("  %-12s %s\n", p.Name, p.Description)
	}
	return out
}
