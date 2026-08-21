package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"bgm/internal/engine/acestep"
	"bgm/internal/prompting"
	"bgm/internal/state"
)

// doctorCommand checks the environment and reports what works.
func doctorCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check the environment and engine health",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDoctor()
		},
	}
}

func runDoctor() error {
	a, err := newApp(false)
	if err != nil {
		return err
	}
	defer a.close()
	ctx := context.Background()

	fmt.Println("bgm doctor")
	fmt.Println("  data dir:   ", a.paths.DataDir)
	fmt.Println("  config file:", a.paths.ConfigFile())
	fmt.Println("  log files:  ", a.paths.LogFile(), "and", a.daemonLogFile())
	fmt.Println()

	check := func(name string, ok bool, detail string) {
		mark := "ok  "
		if !ok {
			mark = "MISS"
		}
		fmt.Printf("  [%s] %-16s %s\n", mark, name, detail)
	}

	// System tools.
	ffmpeg := which("ffmpeg")
	check("ffmpeg", ffmpeg != "", orElse(ffmpeg, "required for MP3 export (run 'make deps')"))
	if ffmpeg != "" {
		out, _ := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-encoders").Output()
		check("libmp3lame", strings.Contains(string(out), "libmp3lame"), "MP3 encoder in ffmpeg")
	}
	check("ffprobe", which("ffprobe") != "", "audio file inspection")
	check("git", which("git") != "", "required by 'bgm setup'")
	pwPlay := which("pw-play")
	pacat := which("pacat")
	check("audio output", pwPlay != "" || pacat != "", orElse(orElse(pwPlay, pacat), "pw-play or pacat needed for playback (PipeWire/PulseAudio)"))

	// GPU.
	nvsmi := which("nvidia-smi")
	if nvsmi == "" {
		check("nvidia gpu", false, "nvidia-smi not found; music generation will be very slow or unavailable (noise modes still work)")
	} else {
		out, err := exec.CommandContext(ctx, "nvidia-smi",
			"--query-gpu=name,memory.total,driver_version", "--format=csv,noheader").Output()
		check("nvidia gpu", err == nil, strings.TrimSpace(string(out)))
	}

	// Engine install.
	uvOK := which("uv") != ""
	if !uvOK {
		if home, err := os.UserHomeDir(); err == nil {
			if _, statErr := os.Stat(filepath.Join(home, ".local", "bin", "uv")); statErr == nil {
				uvOK = true
			}
		}
	}
	check("uv", uvOK, "Python environment manager (installed by 'bgm setup')")
	installed := acestep.Installed(a.paths.EngineDir())
	check("engine install", installed, a.paths.EngineDir())

	// The shared engine daemon.
	st, ok := a.stateD.ReadEngineState()
	switch {
	case !ok || !state.PIDAlive(st.PID):
		check("engine daemon", true, "not running (starts automatically with 'bgm'; stop with 'bgm engine stop')")
	default:
		detail := fmt.Sprintf("running: pid %d, port %d, up %s, last client %s ago",
			st.PID, st.Port, time.Since(st.Started).Round(time.Second), a.stateD.HeartbeatAge().Round(time.Second))
		check("engine daemon", true, detail)
		hctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		h, err := acestep.NewClient(fmt.Sprintf("http://127.0.0.1:%d", st.Port)).Health(hctx)
		cancel()
		switch {
		case err != nil:
			check("engine api", true, "not answering yet (starting up)")
		case h.ModelsInitialized:
			check("engine api", true, "ready (model "+h.LoadedModel+")")
		default:
			check("engine api", true, "loading models")
		}
	}

	// Ollama.
	oll := prompting.NewOllama(a.cfg.Ollama.URL, a.cfg.Ollama.Model)
	octx, ocancel := context.WithTimeout(ctx, 2*time.Second)
	if oll.Available(octx) {
		check("ollama", true, "reachable, model "+oll.Model()+" (optional; used only when it responds quickly)")
	} else {
		check("ollama", true, "not reachable (optional; deterministic prompt merging is used)")
	}
	ocancel()

	if !installed {
		fmt.Println("\nnext step: run 'bgm setup' (or 'make setup') to install the music engine")
	}
	return nil
}

func which(bin string) string {
	p, err := exec.LookPath(bin)
	if err != nil {
		return ""
	}
	return p
}

func orElse(v, alt string) string {
	if v != "" {
		return v
	}
	return alt
}
