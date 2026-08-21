// Command bgm plays a continuous stream of AI-generated background music
// using models that run entirely on the local machine.
package main

import (
	"fmt"
	"os"
)

// version is stamped at build time via -ldflags.
var version = "dev"

func usage() {
	fmt.Fprint(os.Stderr, `bgm - endless AI background music, generated locally

Usage:
  bgm [play] [flags]     start playback (default command)
  bgm setup              first-run setup: install engine and models
  bgm export [flags]     render minutes of music to an MP3 file
  bgm sessions           list saved sessions and presets
  bgm doctor             check the environment and engine health
  bgm version            print the version

Play flags:
  -preset NAME    start from a built-in preset
  -session NAME   resume a saved session
  -engine NAME    generation engine: acestep or noise
  -player NAME    audio backend: auto, pipe, null, file
  -plain          plain line-based interface (no full-screen UI)
  -no-llm         disable Ollama-assisted prompt rewriting
  -verbose        mirror logs to stderr

Run "bgm <command> -h" for command-specific flags.
`)
}

func main() {
	args := os.Args[1:]
	cmd := "play"
	if len(args) > 0 && !isFlag(args[0]) {
		cmd = args[0]
		args = args[1:]
	}
	var err error
	switch cmd {
	case "play":
		err = cmdPlay(args)
	case "setup":
		err = cmdSetup(args)
	case "export":
		err = cmdExport(args)
	case "sessions":
		err = cmdSessions(args)
	case "doctor":
		err = cmdDoctor(args)
	case "version":
		fmt.Println("bgm", version)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "bgm: unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "bgm:", err)
		os.Exit(1)
	}
}

func isFlag(s string) bool {
	return len(s) > 0 && s[0] == '-'
}
