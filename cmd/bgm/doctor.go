package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"bgm/internal/engine/acestep"
	"bgm/internal/prompting"
	"bgm/internal/remote"
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

	// Playback glitches: underruns recorded in the recent log.
	if n, total := recentUnderruns(a.paths.LogFile()); n >= 0 {
		detail := "none in the recent log"
		if n > 0 {
			detail = fmt.Sprintf("%d underrun event(s) in the recent log (last total %d); if you hear glitches, see the README troubleshooting section", n, total)
		}
		check("underruns", n == 0, detail)
	}

	// Phone remote.
	fmt.Println()
	if a.cfg.Remote.Enabled {
		check("remote", true, fmt.Sprintf("enabled on port %d (config remote.enabled)", a.cfg.Remote.Port))
	} else {
		check("remote", true, "off (enable with --remote or config remote.enabled)")
	}
	addrs, tailnetIP := remote.BindAddrs(a.cfg.Remote.Bind, a.cfg.Remote.Port)
	if a.cfg.Remote.Bind != "" {
		check("remote bind", true, fmt.Sprintf("OVERRIDDEN to %s - make sure that network is trusted", strings.Join(addrs, ", ")))
	} else {
		check("remote bind", true, "would bind "+strings.Join(addrs, ", "))
	}
	tsBin := which("tailscale")
	switch {
	case tailnetIP != "":
		check("tailnet", true, fmt.Sprintf("detected %s; on the phone open http://%s:%d", tailnetIP, tailnetIP, a.cfg.Remote.Port))
	case tsBin != "":
		check("tailnet", true, "tailscale installed but no tailnet address; run 'sudo tailscale up', then re-check")
	default:
		check("tailnet", true, "not installed; remote stays localhost-only (see the README for the phone setup)")
	}
	if a.cfg.Remote.Token != "" {
		check("remote token", true, "shared-secret token required on every request")
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

// recentUnderruns counts underrun events in the tail of the log file and
// returns the last reported running total.
func recentUnderruns(logFile string) (count, lastTotal int) {
	f, err := os.Open(logFile)
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	const tailBytes = 256 * 1024
	if fi, err := f.Stat(); err == nil && fi.Size() > tailBytes {
		f.Seek(fi.Size()-tailBytes, 0)
	}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.Contains(line, `"event":"underrun"`) {
			continue
		}
		count++
		var ev struct {
			Total int `json:"total"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err == nil {
			lastTotal = ev.Total
		}
	}
	return count, lastTotal
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
