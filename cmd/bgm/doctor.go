package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"bgm/internal/engine/acestep"
	"bgm/internal/prompting"
)

// cmdDoctor checks the environment and reports what works.
func cmdDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	fs.Parse(args)

	a, err := newApp(false)
	if err != nil {
		return err
	}
	defer a.close()
	ctx := context.Background()

	fmt.Println("bgm doctor")
	fmt.Println("  data dir:   ", a.paths.DataDir)
	fmt.Println("  config file:", a.paths.ConfigFile())
	fmt.Println("  log file:   ", a.paths.LogFile())
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

	// A running engine API (from another bgm instance)?
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", a.cfg.ACEStep.Port)
	hctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	h, err := acestep.NewClient(baseURL).Health(hctx)
	cancel()
	switch {
	case err == nil && h.ModelsInitialized:
		check("engine api", true, fmt.Sprintf("responding on port %d (model %s)", a.cfg.ACEStep.Port, h.LoadedModel))
	case err == nil:
		check("engine api", true, fmt.Sprintf("starting up on port %d", a.cfg.ACEStep.Port))
	default:
		check("engine api", true, "not running (starts automatically with 'bgm')")
	}

	// Ollama.
	oll := prompting.NewOllama(a.cfg.Ollama.URL, a.cfg.Ollama.Model)
	octx, ocancel := context.WithTimeout(ctx, 2*time.Second)
	if oll.Available(octx) {
		check("ollama", true, "reachable, model "+oll.Model()+" (optional)")
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
