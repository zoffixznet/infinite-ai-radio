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
	// FirstLoadBudget is how long to wait for readiness before declaring
	// startup failed. First runs download models and can take a long time.
	FirstLoadBudget time.Duration
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
	stopped  bool
	restarts int
	lastErr  error
	tail     []string // ring of recent child output lines

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
	return &Sidecar{cfg: cfg, log: log, client: client, done: make(chan struct{})}
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
	backoff := 5 * time.Second
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
	uv, err := findUV()
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, uv, "run", "--project", s.cfg.EngineDir, "acestep-api")
	cmd.Dir = s.cfg.EngineDir
	cmd.Env = append(os.Environ(),
		"ACESTEP_API_HOST=127.0.0.1",
		fmt.Sprintf("ACESTEP_API_PORT=%d", s.cfg.Port),
		"PYTORCH_CUDA_ALLOC_CONF=expandable_segments:True",
	)
	if s.cfg.LMModelPath != "" {
		cmd.Env = append(cmd.Env, "ACESTEP_LM_MODEL_PATH="+s.cfg.LMModelPath)
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
	poll := time.NewTicker(time.Second)
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
