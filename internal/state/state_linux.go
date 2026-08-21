package state

import (
	"errors"
	"os"
	"syscall"
)

// pidAlive probes a pid with signal 0.
func pidAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// flock takes an exclusive advisory lock on f.
func flock(f *os.File, block bool) error {
	how := syscall.LOCK_EX
	if !block {
		how |= syscall.LOCK_NB
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return ErrLocked
		}
		return err
	}
	return nil
}

// funlock releases the advisory lock on f.
func funlock(f *os.File) {
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}

// Terminate sends SIGTERM to a process.
func Terminate(pid int) error {
	return syscall.Kill(pid, syscall.SIGTERM)
}
