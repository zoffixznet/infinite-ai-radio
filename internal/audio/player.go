package audio

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"time"
)

// Player is an audio output backend consuming the internal PCM format.
// Write blocks at roughly realtime pace; that backpressure paces the
// playback pump.
type Player interface {
	// Write plays p (interleaved s16le PCM at the internal format).
	Write(p []byte) (int, error)
	// Close releases the backend.
	Close() error
	// Name identifies the backend for logs and status displays.
	Name() string
}

// EnvPlayerSpeed lets tests speed up the pacing of the null and file
// backends (a float multiplier; 1 is realtime).
const EnvPlayerSpeed = "BGM_PLAYER_SPEED"

// EnvPipeTarget selects an explicit output device/sink for the pipe backend
// (passed to pw-play --target or pacat --device). Tests use it to route
// audio into a null sink.
const EnvPipeTarget = "BGM_PIPE_TARGET"

// NewPlayer builds a playback backend.
//
//	auto  - the best available real backend (currently: pipe)
//	pipe  - one long-lived pw-play (or pacat) process reading PCM on stdin
//	null  - discards audio at realtime pace (tests, soak runs)
//	file  - appends raw PCM to filePath at realtime pace (tests)
func NewPlayer(kind, filePath string) (Player, error) {
	switch kind {
	case "auto":
		p, err := newPipePlayer()
		if err != nil {
			return nil, fmt.Errorf("no usable audio backend: %w (use --player null for silent operation)", err)
		}
		return p, nil
	case "pipe":
		return newPipePlayer()
	case "null":
		return &nullPlayer{pace: newPacer()}, nil
	case "file":
		if filePath == "" {
			return nil, fmt.Errorf("file player needs an output path")
		}
		f, err := os.OpenFile(filePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return nil, err
		}
		return &filePlayer{f: f, pace: newPacer()}, nil
	default:
		return nil, fmt.Errorf("unknown player backend %q", kind)
	}
}

// pacer sleeps writers so bytes flow at realtime speed (divided by the
// BGM_PLAYER_SPEED multiplier).
type pacer struct {
	start   time.Time
	written int64
	speed   float64
}

func newPacer() *pacer {
	speed := 1.0
	if v := os.Getenv(EnvPlayerSpeed); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			speed = f
		}
	}
	return &pacer{speed: speed}
}

func (p *pacer) pace(n int) {
	if p.start.IsZero() {
		p.start = time.Now()
	}
	p.written += int64(n)
	ideal := time.Duration(float64(p.written) / BytesPerSecond / p.speed * float64(time.Second))
	elapsed := time.Since(p.start)
	if sleep := ideal - elapsed; sleep > 0 {
		time.Sleep(sleep)
	}
}

// nullPlayer discards audio at realtime pace.
type nullPlayer struct{ pace *pacer }

func (n *nullPlayer) Write(p []byte) (int, error) {
	n.pace.pace(len(p))
	return len(p), nil
}

func (n *nullPlayer) Close() error { return nil }
func (n *nullPlayer) Name() string { return "null" }

// filePlayer appends raw PCM to a file at realtime pace.
type filePlayer struct {
	f    *os.File
	pace *pacer
}

func (fp *filePlayer) Write(p []byte) (int, error) {
	n, err := fp.f.Write(p)
	fp.pace.pace(n)
	return n, err
}

func (fp *filePlayer) Close() error { return fp.f.Close() }
func (fp *filePlayer) Name() string { return "file" }

// pipePlayer feeds one long-lived PipeWire/PulseAudio command-line player
// through its stdin. The child process consumes at realtime pace, which
// provides the backpressure.
type pipePlayer struct {
	cmd   *exec.Cmd
	stdin *os.File
	name  string
}

func newPipePlayer() (*pipePlayer, error) {
	target := os.Getenv(EnvPipeTarget)
	candidates := []struct {
		bin  string
		args []string
	}{
		{"pw-play", pipeArgsPwPlay(target)},
		{"pacat", pipeArgsPacat(target)},
	}
	for _, c := range candidates {
		path, err := exec.LookPath(c.bin)
		if err != nil {
			continue
		}
		pr, pw, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		cmd := exec.Command(path, c.args...)
		cmd.Stdin = pr
		cmd.Stdout = nil
		cmd.Stderr = nil
		bindToParent(cmd)
		if err := cmd.Start(); err != nil {
			pr.Close()
			pw.Close()
			continue
		}
		pr.Close()
		return &pipePlayer{cmd: cmd, stdin: pw, name: "pipe:" + c.bin}, nil
	}
	return nil, fmt.Errorf("neither pw-play nor pacat found in PATH")
}

func pipeArgsPwPlay(target string) []string {
	args := []string{"--raw", "--rate=48000", "--channels=2", "--format=s16", "-"}
	if target != "" {
		args = append([]string{"--target", target}, args...)
	}
	return args
}

func pipeArgsPacat(target string) []string {
	args := []string{"--raw", "--rate=48000", "--channels=2", "--format=s16le"}
	if target != "" {
		args = append(args, "--device="+target)
	}
	return args
}

func (p *pipePlayer) Write(b []byte) (int, error) {
	return p.stdin.Write(b)
}

func (p *pipePlayer) Close() error {
	p.stdin.Close()
	if p.cmd.Process != nil {
		done := make(chan struct{})
		go func() {
			p.cmd.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			p.cmd.Process.Kill()
			<-done
		}
	}
	return nil
}

func (p *pipePlayer) Name() string { return p.name }
