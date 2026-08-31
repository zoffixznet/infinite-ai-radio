package acestep

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// InstallConfig describes where and what to install.
type InstallConfig struct {
	// EngineDir is the target directory for the engine checkout.
	EngineDir string
	// RepoURL and Tag pin the engine version.
	RepoURL string
	Tag     string
	// LMModelPath optionally names an extra planner LM checkpoint to
	// pre-download (the main weights bundle is always fetched).
	LMModelPath string
}

// Installer performs first-run setup: user-level uv install, engine
// checkout at the pinned tag, Python environment sync, and model weight
// pre-download. Every step is idempotent, so an interrupted setup resumes
// where it left off. It never uses sudo.
type Installer struct {
	cfg InstallConfig
	// Progress receives human-readable progress lines. Never nil.
	progress func(string)
}

// NewInstaller returns an installer reporting progress through report
// (which may be nil).
func NewInstaller(cfg InstallConfig, report func(string)) *Installer {
	if report == nil {
		report = func(string) {}
	}
	return &Installer{cfg: cfg, progress: report}
}

// Installed reports whether the engine looks fully installed (checkout,
// synced environment and main weights present).
func Installed(engineDir string) bool {
	if _, err := os.Stat(filepath.Join(engineDir, "pyproject.toml")); err != nil {
		return false
	}
	if _, err := os.Stat(filepath.Join(engineDir, ".venv")); err != nil {
		return false
	}
	turbo := filepath.Join(engineDir, "checkpoints", "acestep-v15-turbo")
	entries, err := os.ReadDir(turbo)
	return err == nil && len(entries) > 0
}

// Run performs all setup steps in order.
func (ins *Installer) Run(ctx context.Context) error {
	steps := []struct {
		name string
		fn   func(context.Context) error
	}{
		{"install uv", ins.ensureUV},
		{"fetch engine source", ins.ensureCheckout},
		{"patch engine source", ins.ensurePatches},
		{"sync Python environment", ins.ensureEnv},
		{"download model weights", ins.ensureWeights},
	}
	for i, step := range steps {
		ins.progress(fmt.Sprintf("[%d/%d] %s", i+1, len(steps), step.name))
		if err := step.fn(ctx); err != nil {
			return fmt.Errorf("%s: %w", step.name, err)
		}
	}
	ins.progress("setup complete")
	return nil
}

// ensureUV installs uv into ~/.local/bin when missing, via the official
// standalone installer.
func (ins *Installer) ensureUV(ctx context.Context) error {
	if p, err := findUV(); err == nil {
		ins.progress("uv already installed: " + p)
		return nil
	}
	ins.progress("installing uv (user-level, no sudo)")
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	script := exec.CommandContext(ctx, "sh", "-c",
		"curl -LsSf https://astral.sh/uv/install.sh | sh")
	script.Env = append(os.Environ(),
		"UV_INSTALL_DIR="+filepath.Join(home, ".local", "bin"),
		"UV_NO_MODIFY_PATH=1",
	)
	if err := ins.runStreaming(script); err != nil {
		return fmt.Errorf("uv installer failed: %w", err)
	}
	if _, err := findUV(); err != nil {
		return fmt.Errorf("uv still not found after install")
	}
	return nil
}

// ensureCheckout clones the engine repository at the pinned tag, or fixes
// up an existing checkout to that tag.
func (ins *Installer) ensureCheckout(ctx context.Context) error {
	dir := ins.cfg.EngineDir
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			return err
		}
		ins.progress(fmt.Sprintf("cloning %s at %s", ins.cfg.RepoURL, ins.cfg.Tag))
		cmd := exec.CommandContext(ctx, "git", "clone", "--depth", "1",
			"--branch", ins.cfg.Tag, ins.cfg.RepoURL, dir)
		return ins.runStreaming(cmd)
	}
	// Existing checkout: make sure it sits at the pinned tag.
	current, err := exec.CommandContext(ctx, "git", "-C", dir, "describe", "--tags", "--exact-match").Output()
	if err == nil && strings.TrimSpace(string(current)) == ins.cfg.Tag {
		ins.progress("engine source already at " + ins.cfg.Tag)
		return nil
	}
	ins.progress("updating engine source to " + ins.cfg.Tag)
	fetch := exec.CommandContext(ctx, "git", "-C", dir, "fetch", "--depth", "1", "origin", "tag", ins.cfg.Tag)
	if err := ins.runStreaming(fetch); err != nil {
		return err
	}
	// Drop the applied engine patches only once the fetch has
	// succeeded: they are the only local modifications ever made to
	// the checkout, and a dirty tree would make the tag switch fail -
	// but resetting before a fetch that then fails would leave the
	// old tag unpatched. The patch step re-applies them against the
	// new tag right after (or fails loudly if they no longer fit).
	reset := exec.CommandContext(ctx, "git", "-C", dir, "checkout", "--", ".")
	if err := ins.runStreaming(reset); err != nil {
		return err
	}
	checkout := exec.CommandContext(ctx, "git", "-C", dir, "checkout", ins.cfg.Tag)
	return ins.runStreaming(checkout)
}

// ensurePatches applies iar's targeted engine fixes to the checkout
// (see patches.go). Idempotent: files already carrying a patch are left
// alone.
func (ins *Installer) ensurePatches(context.Context) error {
	applied, err := ApplyEnginePatches(ins.cfg.EngineDir)
	if err != nil {
		return err
	}
	if len(applied) == 0 {
		ins.progress("engine patches already present")
	} else {
		ins.progress("applied engine patches: " + strings.Join(applied, ", "))
	}
	return nil
}

// ensureEnv runs uv sync, which creates or repairs the .venv. uv sync is
// naturally idempotent and resumable.
func (ins *Installer) ensureEnv(ctx context.Context) error {
	uv, err := findUV()
	if err != nil {
		return err
	}
	ins.progress("running uv sync (first run downloads several GB of Python packages)")
	cmd := exec.CommandContext(ctx, uv, "sync")
	cmd.Dir = ins.cfg.EngineDir
	return ins.runStreaming(cmd)
}

// ensureWeights pre-downloads the model weights with the engine's own
// downloader, which resumes partial downloads and skips completed models.
func (ins *Installer) ensureWeights(ctx context.Context) error {
	uv, err := findUV()
	if err != nil {
		return err
	}
	ins.progress("downloading model weights (about 10 GB on first run)")
	cmd := exec.CommandContext(ctx, uv, "run", "acestep-download")
	cmd.Dir = ins.cfg.EngineDir
	cmd.Env = append(os.Environ(), "ACESTEP_DOWNLOAD_SOURCE=huggingface")
	if err := ins.runStreaming(cmd); err != nil {
		return err
	}
	// The engine auto-selects the small planner LM on GPUs with less
	// memory; pre-fetch it so the first play never stalls on a download.
	lm := ins.cfg.LMModelPath
	if lm == "" {
		lm = "acestep-5Hz-lm-0.6B"
	}
	if lm != "acestep-5Hz-lm-1.7B" { // 1.7B ships in the main bundle
		ins.progress("downloading planner LM " + lm)
		cmd := exec.CommandContext(ctx, uv, "run", "acestep-download", "--model", lm)
		cmd.Dir = ins.cfg.EngineDir
		cmd.Env = append(os.Environ(), "ACESTEP_DOWNLOAD_SOURCE=huggingface")
		if err := ins.runStreaming(cmd); err != nil {
			return err
		}
	}
	return nil
}

// runStreaming runs cmd, forwarding its combined output line by line to the
// progress callback.
func (ins *Installer) runStreaming(cmd *exec.Cmd) error {
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw
	if err := cmd.Start(); err != nil {
		pw.Close()
		return err
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		scanner := bufio.NewScanner(pr)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		// Split on \n and \r so download progress bars (which redraw
		// with carriage returns) still stream as they happen.
		scanner.Split(scanLinesCR)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line != "" {
				ins.progress("  " + line)
			}
		}
	}()
	err := cmd.Wait()
	pw.Close()
	<-done
	return err
}

// scanLinesCR is a bufio.SplitFunc splitting on \n or \r.
func scanLinesCR(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := strings.IndexAny(string(data), "\r\n"); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}
