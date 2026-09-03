package acestep

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SidecarConfig configures the supervised API server process.
type SidecarConfig struct {
	// EngineDir is the engine checkout created by the installer.
	EngineDir string
	// Port is the localhost port to serve on.
	Port int
	// LMModelPath optionally pins the planner LM checkpoint; empty lets
	// the engine choose by GPU tier.
	LMModelPath string
	// LMBackend selects the planner LM runtime: "auto" (default) picks a
	// memory-friendly setup on GPUs under 16 GB, "vllm"/"pt" force one.
	LMBackend string
	// FirstLoadBudget is how long to wait for readiness before declaring
	// startup failed. First runs download models and can take a long time.
	FirstLoadBudget time.Duration
	// InitialBackoff is the first restart delay after a crash; it doubles
	// up to a cap. Zero means five seconds.
	InitialBackoff time.Duration
	// PollInterval is how often health is polled during startup. Zero
	// means one second.
	PollInterval time.Duration
	// OffloadDIT keeps the music model in system memory between
	// tracks instead of resident on the graphics card.
	OffloadDIT bool
	// MaxTrackSeconds caps the track length the engine's planner may
	// choose when it writes the words itself. Zero means no ceiling.
	MaxTrackSeconds int
	// OffloadDITDisk runs the music model disk-backed (dit-from-disk
	// patch): never parked in system memory, streamed onto the card on
	// demand and dropped when planning work starts. Phased generation
	// requires it; it implies the CPU-offload base flags.
	OffloadDITDisk bool
}

// Sidecar supervises the ACE-Step API server as a child process: it starts
// it, drains its output into the log, polls health, and restarts it with
// backoff when it dies. Playback continues from buffered audio during
// restarts; the supervisor only reports state.
type Sidecar struct {
	cfg    SidecarConfig
	log    *slog.Logger
	client *Client

	mu       sync.Mutex
	cmd      *exec.Cmd
	ready    bool
	starting bool
	restarts int
	lastErr  error
	tail     []string // ring of recent child output lines

	// newCmd builds the child process command; tests replace it.
	newCmd func(ctx context.Context) (*exec.Cmd, error)

	done chan struct{}
}

// tailLines is how many recent sidecar output lines are kept for
// diagnostics.
const tailLines = 40

// NewSidecar returns an unstarted supervisor.
func NewSidecar(cfg SidecarConfig, client *Client, log *slog.Logger) *Sidecar {
	if cfg.FirstLoadBudget <= 0 {
		cfg.FirstLoadBudget = 45 * time.Minute
	}
	if cfg.InitialBackoff <= 0 {
		cfg.InitialBackoff = 5 * time.Second
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = time.Second
	}
	s := &Sidecar{cfg: cfg, log: log, client: client, done: make(chan struct{})}
	s.newCmd = s.serverCommand
	return s
}

// serverCommand builds the real API server child process.
func (s *Sidecar) serverCommand(ctx context.Context) (*exec.Cmd, error) {
	// The engine server is what holds the graphics card, so it must be
	// THIS process's direct child: the kill-on-parent-death the
	// sidecar sets reaches a child, never a grandchild. Launched
	// through 'uv run' the server is uv's child, and a daemon killed
	// outright left it behind holding gigabytes of video memory with
	// nothing supervising it. The environment setup built it a
	// launcher of its own, so use that when it is there.
	venvDir := filepath.Join(s.cfg.EngineDir, ".venv")
	entry := filepath.Join(venvDir, "bin", "acestep-api")
	var cmd *exec.Cmd
	if fi, err := os.Stat(entry); err == nil && !fi.IsDir() {
		cmd = exec.CommandContext(ctx, entry)
	} else {
		uv, err := findUV()
		if err != nil {
			return nil, err
		}
		// --no-sync: the environment was built by setup; skipping the
		// sync check keeps startup fast and independent of the network.
		cmd = exec.CommandContext(ctx, uv, "run", "--no-sync", "--project", s.cfg.EngineDir, "acestep-api")
	}
	cmd.Dir = s.cfg.EngineDir
	cmd.Env = append(os.Environ(),
		// What 'uv run' would have set up for the interpreter.
		"VIRTUAL_ENV="+venvDir,
		"PATH="+filepath.Join(venvDir, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"),
		"ACESTEP_API_HOST=127.0.0.1",
		fmt.Sprintf("ACESTEP_API_PORT=%d", s.cfg.Port),
		// Load models eagerly at startup so readiness means ready;
		// the server otherwise lazy-loads on the first request.
		"ACESTEP_NO_INIT=false",
		"PYTORCH_CUDA_ALLOC_CONF=expandable_segments:True",
	)
	if s.cfg.LMModelPath != "" {
		cmd.Env = append(cmd.Env, "ACESTEP_LM_MODEL_PATH="+s.cfg.LMModelPath)
	}
	if s.cfg.OffloadDIT {
		// The engine implements this but its API server never reads the
		// GPU-tier default that would switch it on, so it stays
		// resident unless we say otherwise.
		cmd.Env = append(cmd.Env, "ACESTEP_OFFLOAD_DIT_TO_CPU=true")
	}
	if s.cfg.OffloadDITDisk {
		// dit-from-disk builds on the CPU-offload machinery, so both
		// flags travel together; the disk flag wins inside the engine
		// (weights are dropped and re-streamed, never parked in RAM).
		cmd.Env = append(cmd.Env,
			"ACESTEP_OFFLOAD_DIT_TO_CPU=true",
			"ACESTEP_OFFLOAD_DIT_TO_DISK=true")
	}
	if s.cfg.MaxTrackSeconds > 0 {
		// Read by the sample-duration-cap engine patch: a ceiling on
		// the track length the planner picks for tracks it writes the
		// words for. Shorter picks pass through untouched.
		cmd.Env = append(cmd.Env, fmt.Sprintf("ACESTEP_SAMPLE_DURATION_CAP=%d", s.cfg.MaxTrackSeconds))
	}
	cmd.Env = append(cmd.Env, s.lmBackendEnv()...)
	return cmd, nil
}

// lmBackendEnv resolves the planner LM runtime settings. On GPUs under
// 16 GB the vLLM backend's memory reservation competes badly with the
// diffusion model (and with anything else using the GPU), so "auto" runs
// the LM on the plain PyTorch backend with CPU offload there: slightly
// slower planning, far smaller and fully released between generations.
func (s *Sidecar) lmBackendEnv() []string {
	backend := s.cfg.LMBackend
	if backend == "" || backend == "auto" {
		vram, err := totalVRAMGB()
		if err != nil || vram < 15.5 {
			return []string{"ACESTEP_LM_BACKEND=pt", "ACESTEP_LM_OFFLOAD_TO_CPU=true"}
		}
		return nil // engine defaults
	}
	return []string{"ACESTEP_LM_BACKEND=" + backend}
}

// totalVRAMGB reports the GPU's total memory via nvidia-smi.
func totalVRAMGB() (float64, error) {
	out, err := exec.Command("nvidia-smi", "--query-gpu=memory.total", "--format=csv,noheader,nounits").Output()
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return 0, fmt.Errorf("no output from nvidia-smi")
	}
	mib, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, err
	}
	return mib / 1024, nil
}

// Start launches the supervision loop. It returns immediately; readiness is
// reported by Ready and WaitReady. The loop exits when ctx is canceled.
func (s *Sidecar) Start(ctx context.Context) {
	go s.superviseLoop(ctx)
}

// Ready reports whether the API server currently answers health checks with
// models loaded.
func (s *Sidecar) Ready() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ready
}

// Starting reports whether the server is booting (including first-run model
// downloads).
func (s *Sidecar) Starting() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.starting
}

// Restarts reports how many times the child was restarted after a failure.
func (s *Sidecar) Restarts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.restarts
}

// LastError returns the most recent startup or crash error, if any.
func (s *Sidecar) LastError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastErr
}

// Client returns the REST client for this server.
func (s *Sidecar) Client() *Client { return s.client }

// Phase implements Backend for progress displays.
func (s *Sidecar) Phase() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.ready:
		return "ready"
	case s.starting:
		return "loading models"
	default:
		return "starting engine"
	}
}

// RestartEngine kills the supervised child; the supervision loop starts
// a fresh one. Always initiates (the loop has its own backoff).
func (s *Sidecar) RestartEngine(reason string) bool {
	s.log.Warn("forcing sidecar restart", "event", "engine_forced_restart", "reason", reason)
	s.kill()
	return true
}

// Tail returns the most recent lines of the child's output, newest last.
func (s *Sidecar) Tail() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.tail))
	copy(out, s.tail)
	return out
}

// WaitReady blocks until the server is ready, the budget elapses, or ctx is
// canceled.
func (s *Sidecar) WaitReady(ctx context.Context) error {
	deadline := time.NewTimer(s.cfg.FirstLoadBudget)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("engine did not become ready within %s", s.cfg.FirstLoadBudget)
		case <-ticker.C:
			if s.Ready() {
				return nil
			}
		}
	}
}

// superviseLoop keeps one child process alive until ctx ends.
func (s *Sidecar) superviseLoop(ctx context.Context) {
	defer close(s.done)
	backoff := s.cfg.InitialBackoff
	const maxBackoff = 5 * time.Minute
	for ctx.Err() == nil {
		err := s.runOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		s.mu.Lock()
		s.ready = false
		s.starting = false
		s.restarts++
		s.lastErr = err
		restarts := s.restarts
		s.mu.Unlock()
		s.log.Error("sidecar exited", "event", "sidecar_exit", "error", fmt.Sprint(err), "restarts", restarts, "backoff", backoff.String())
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// runOnce starts the child, polls health until ready, then waits for the
// child to exit. It returns the reason the child is no longer usable.
func (s *Sidecar) runOnce(ctx context.Context) error {
	cmd, err := s.newCmd(ctx)
	if err != nil {
		return err
	}
	configureSidecar(cmd)
	cmd.Cancel = func() error {
		killProcessGroup(cmd)
		return nil
	}
	cmd.WaitDelay = 10 * time.Second

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting sidecar: %w", err)
	}
	s.mu.Lock()
	s.cmd = cmd
	s.starting = true
	s.mu.Unlock()
	s.log.Info("sidecar started", "event", "sidecar_start", "pid", cmd.Process.Pid, "port", s.cfg.Port)

	// Drain both pipes; an undrained pipe would hang the child.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); s.drain(stdout, "stdout") }()
	go func() { defer wg.Done(); s.drain(stderr, "stderr") }()

	exited := make(chan error, 1)
	go func() {
		wg.Wait()
		exited <- cmd.Wait()
	}()

	// Poll health until ready, while watching for early exit.
	budget := time.NewTimer(s.cfg.FirstLoadBudget)
	defer budget.Stop()
	poll := time.NewTicker(s.cfg.PollInterval)
	defer poll.Stop()
waitReady:
	for {
		select {
		case <-ctx.Done():
			<-exited
			return ctx.Err()
		case err := <-exited:
			return fmt.Errorf("sidecar exited during startup: %w", err)
		case <-budget.C:
			s.kill()
			<-exited
			return fmt.Errorf("sidecar not ready within %s, killed", s.cfg.FirstLoadBudget)
		case <-poll.C:
			hctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			h, err := s.client.Health(hctx)
			cancel()
			if err == nil && h.ModelsInitialized {
				s.mu.Lock()
				s.ready = true
				s.starting = false
				s.mu.Unlock()
				s.log.Info("sidecar ready", "event", "sidecar_ready", "model", h.LoadedModel, "lm_model", h.LoadedLMModel, "llm", h.LLMInitialized)
				break waitReady
			}
		}
	}

	select {
	case <-ctx.Done():
		<-exited
		return ctx.Err()
	case err := <-exited:
		return fmt.Errorf("sidecar process exited: %w", err)
	}
}

// kill force-terminates the current child process group.
func (s *Sidecar) kill() {
	s.mu.Lock()
	cmd := s.cmd
	s.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		killProcessGroup(cmd)
	}
}

// drain reads child output line by line into the log and the tail ring.
func (s *Sidecar) drain(r io.Reader, stream string) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), " \t")
		if line == "" {
			continue
		}
		s.mu.Lock()
		s.tail = append(s.tail, line)
		if len(s.tail) > tailLines {
			s.tail = s.tail[len(s.tail)-tailLines:]
		}
		s.mu.Unlock()
		s.log.Debug("sidecar output", "event", "sidecar_output", "stream", stream, "line", line)
		s.noteMemoryPressure(line)
	}
}

// memoryPressureNeedles are the engine's own wording for a graphics
// card with no room left, in the order of how bad the news is.
var memoryPressureNeedles = []struct{ needle, event, msg string }{
	{"CUDA out of memory", "engine_out_of_memory",
		"the graphics card ran out of memory during generation"},
	{"auto-enabling CPU VAE decode", "engine_cpu_fallback",
		"low graphics memory: this track is being finished on the processor, which is much slower"},
}

// noteMemoryPressure lifts the two lines that matter out of the
// sidecar's very chatty output into the app's own log. Both mean the
// graphics card is short, and both are otherwise buried in tens of
// megabytes of engine output.
func (s *Sidecar) noteMemoryPressure(line string) {
	for _, n := range memoryPressureNeedles {
		if strings.Contains(line, n.needle) {
			s.log.Warn(n.msg, "event", n.event, "line", line)
			return
		}
	}
}

// findUV locates the uv binary in PATH or the user-level install location.
func findUV() (string, error) {
	if p, err := exec.LookPath("uv"); err == nil {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err == nil {
		p := filepath.Join(home, ".local", "bin", "uv")
		if _, statErr := os.Stat(p); statErr == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("uv not found (run setup first)")
}
