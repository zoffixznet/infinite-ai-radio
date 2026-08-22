// Package state manages the small runtime state files shared between the
// player's processes: the engine daemon's state file, advisory locks, the client
// heartbeat that keeps the daemon alive, and rolling phase-duration
// measurements used for progress estimates.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

// Dir wraps the state directory under the data dir.
type Dir struct {
	path string
}

// NewDir returns the state directory handle, creating the directory.
func NewDir(dataDir string) (Dir, error) {
	p := filepath.Join(dataDir, "state")
	if err := os.MkdirAll(p, 0o755); err != nil {
		return Dir{}, err
	}
	return Dir{path: p}, nil
}

// Path returns the directory path.
func (d Dir) Path() string { return d.path }

func (d Dir) engineFile() string    { return filepath.Join(d.path, "engine.json") }
func (d Dir) heartbeatFile() string { return filepath.Join(d.path, "heartbeat") }

// EngineState describes a running (or starting) engine daemon.
type EngineState struct {
	// PID is the daemon process id.
	PID int `json:"pid"`
	// Port is the localhost port the engine API listens on.
	Port int `json:"port"`
	// EngineDir is the engine install the daemon serves.
	EngineDir string `json:"engine_dir"`
	// Started is when the daemon started.
	Started time.Time `json:"started"`
}

// WriteEngineState atomically records the daemon state.
func (d Dir) WriteEngineState(st EngineState) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := d.enginePath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, d.enginePath())
}

func (d Dir) enginePath() string { return d.engineFile() }

// ReadEngineState returns the recorded daemon state, or ok=false when none
// exists or it is unreadable.
func (d Dir) ReadEngineState() (EngineState, bool) {
	data, err := os.ReadFile(d.enginePath())
	if err != nil {
		return EngineState{}, false
	}
	var st EngineState
	if err := json.Unmarshal(data, &st); err != nil {
		return EngineState{}, false
	}
	return st, st.PID > 0 && st.Port > 0
}

// RemoveEngineState deletes the daemon state file.
func (d Dir) RemoveEngineState() { os.Remove(d.enginePath()) }

func (d Dir) sessionFile() string { return filepath.Join(d.path, "session.json") }

// CurrentSession records which session a player is playing.
type CurrentSession struct {
	// Name is the session name.
	Name string `json:"name"`
	// PID is the player process.
	PID int `json:"pid"`
	// Since is when the session started playing.
	Since time.Time `json:"since"`
}

// WriteCurrentSession atomically records the playing session.
func (d Dir) WriteCurrentSession(cs CurrentSession) error {
	data, err := json.MarshalIndent(cs, "", "  ")
	if err != nil {
		return err
	}
	tmp := d.sessionFile() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, d.sessionFile())
}

// ReadCurrentSession returns the last recorded playing session; Playing
// reports whether that player process is still alive.
func (d Dir) ReadCurrentSession() (cs CurrentSession, playing bool) {
	data, err := os.ReadFile(d.sessionFile())
	if err != nil {
		return CurrentSession{}, false
	}
	if err := json.Unmarshal(data, &cs); err != nil || cs.Name == "" {
		return CurrentSession{}, false
	}
	return cs, PIDAlive(cs.PID)
}

// ForgetSession clears the recorded playing session when it names name.
func (d Dir) ForgetSession(name string) {
	cs, _ := d.ReadCurrentSession()
	if cs.Name == name {
		os.Remove(d.sessionFile())
	}
}

// PIDAlive reports whether a process with the given pid exists.
func PIDAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return pidAlive(pid)
}

// Heartbeat updates the client heartbeat; the daemon shuts down when no
// client has heartbeat for its idle timeout.
func (d Dir) Heartbeat() error {
	f, err := os.OpenFile(d.heartbeatFile(), os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	f.Close()
	now := time.Now()
	return os.Chtimes(d.heartbeatFile(), now, now)
}

// HeartbeatAge reports how long ago a client last heartbeat. Returns a
// large value when no heartbeat exists.
func (d Dir) HeartbeatAge() time.Duration {
	fi, err := os.Stat(d.heartbeatFile())
	if err != nil {
		return 24 * time.Hour
	}
	return time.Since(fi.ModTime())
}

// Lock is a held advisory file lock.
type Lock struct {
	f *os.File
}

// ErrLocked reports that another process holds the lock.
var ErrLocked = errors.New("already locked by another process")

// AcquireLock takes an exclusive advisory lock on name inside the state
// dir. When block is false and the lock is held elsewhere, ErrLocked is
// returned immediately.
func (d Dir) AcquireLock(name string, block bool) (*Lock, error) {
	f, err := os.OpenFile(filepath.Join(d.path, name), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := flock(f, block); err != nil {
		f.Close()
		return nil, err
	}
	// Record the holder for friendly error messages.
	f.Truncate(0)
	fmt.Fprintf(f, "%d %s\n", os.Getpid(), time.Now().Format(time.RFC3339))
	return &Lock{f: f}, nil
}

// Release drops the lock.
func (l *Lock) Release() {
	if l == nil || l.f == nil {
		return
	}
	funlock(l.f)
	l.f.Close()
	l.f = nil
}

// FreePort asks the kernel for a free localhost TCP port.
func FreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}
