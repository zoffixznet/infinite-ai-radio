package logging

import (
	"fmt"
	"os"
	"strconv"
	"sync"
)

// KeepOld is how many rotated copies of a log are kept next to it:
// <name>.1 is the most recent, <name>.2 the one before; anything older
// is dropped.
const KeepOld = 2

// RotatingFile is an append-only log file that never grows much past a
// size cap. A byte counter tracks the file's size, and the write that
// would carry it over the cap first moves it aside to <name>.1 (pushing
// the older copies down) and starts a fresh file, so a long run rotates
// without a restart. Writes are serialised by a mutex; the file is safe
// to share between goroutines.
type RotatingFile struct {
	path string
	max  int64

	// OnRotate, when set, is called with the fresh file after every
	// rotation (and is not called for the file opened at start). The
	// engine daemon uses it to move its stdout and stderr onto the new
	// file. It runs under the rotation lock.
	OnRotate func(*os.File)

	mu   sync.Mutex
	f    *os.File
	size int64
}

// OpenRotating opens (appending, creating if needed) the log at path,
// capped at maxBytes. A file already at or over the cap is rotated
// before anything is written to it.
func OpenRotating(path string, maxBytes int64) (*RotatingFile, error) {
	if maxBytes < 1 {
		return nil, fmt.Errorf("log cap must be positive, got %d", maxBytes)
	}
	r := &RotatingFile{path: path, max: maxBytes}
	if fi, err := os.Stat(path); err == nil && fi.Size() >= maxBytes {
		// Best effort: a log that cannot be moved aside is still
		// better written to than lost.
		shiftOld(path)
	}
	if err := r.open(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *RotatingFile) open() error {
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("opening log file: %w", err)
	}
	var size int64
	if fi, err := f.Stat(); err == nil {
		size = fi.Size()
	}
	r.f, r.size = f, size
	return nil
}

// File returns the file currently written to.
func (r *RotatingFile) File() *os.File {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.f
}

// Write appends p, rotating first when p would carry the file over its
// cap. A single write larger than the cap still lands whole, in a file
// of its own.
func (r *RotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return 0, os.ErrClosed
	}
	if r.size > 0 && r.size+int64(len(p)) > r.max {
		r.rotate()
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

// rotate moves the current file aside and opens a fresh one. Another
// process appending to the same log (a second iar command) may have
// rotated it already; then the path is simply reopened. The caller
// holds the lock.
func (r *RotatingFile) rotate() {
	cur, errCur := r.f.Stat()
	onDisk, errDisk := os.Stat(r.path)
	if errCur == nil && errDisk == nil && !os.SameFile(cur, onDisk) {
		if onDisk.Size() < r.max {
			r.reopen()
			return
		}
	}
	shiftOld(r.path)
	r.reopen()
}

// reopen swaps in a freshly opened file at the path. If it cannot be
// opened the old file stays in use, and the counter restarts so the
// next attempt waits for another cap's worth of writes.
func (r *RotatingFile) reopen() {
	old := r.f
	if err := r.open(); err != nil {
		r.f, r.size = old, 0
		return
	}
	old.Close()
	if r.OnRotate != nil {
		r.OnRotate(r.f)
	}
}

// Close closes the current file.
func (r *RotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return nil
	}
	err := r.f.Close()
	r.f = nil
	return err
}

// shiftOld renames path to path.1, path.1 to path.2 and so on up to
// KeepOld; the oldest copy is overwritten.
func shiftOld(path string) {
	for i := KeepOld - 1; i >= 1; i-- {
		os.Rename(oldName(path, i), oldName(path, i+1))
	}
	os.Rename(path, oldName(path, 1))
}

func oldName(path string, i int) string {
	return path + "." + strconv.Itoa(i)
}
