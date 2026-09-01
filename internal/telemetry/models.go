package telemetry

import (
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The engine narrates every model it moves between system memory and
// the card, so the daemon log is the only place outside the Python
// process that knows what is resident right now. The tracker follows
// that log from wherever it was last read - the file runs to hundreds
// of megabytes over a long run, and reading it whole is what a status
// display must never do.
const (
	// catchUp is how far back a fresh tracker starts reading. Enough to
	// cover the model moves of the job in flight, small enough to be
	// free.
	catchUp = 256 * 1024
	// maxRead bounds one catch-up read, so a burst of engine output
	// between samples cannot turn into an unbounded allocation.
	maxRead = 4 * 1024 * 1024
)

// modelTracker follows the engine daemon log and keeps the set of
// models currently on the card.
type modelTracker struct {
	path string

	mu     sync.Mutex
	offset int64
	seeded bool
	live   map[string]Model
}

func newModelTracker(path string) *modelTracker {
	return &modelTracker{path: path, live: map[string]Model{}}
}

// resident returns the models on the card, in load order.
func (t *modelTracker) resident() []Model {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.consume()
	out := make([]Model, 0, len(t.live))
	for _, m := range t.live {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Since.Before(out[j].Since) })
	return out
}

// reset forgets everything on the card, for when the daemon that held
// it is gone. The log is still consumed so a fresh daemon's output is
// not replayed as history.
func (t *modelTracker) reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.consume()
	clear(t.live)
}

// consume reads whatever the daemon has written since the last call and
// applies it. The caller holds the lock.
func (t *modelTracker) consume() {
	f, err := os.Open(t.path)
	if err != nil {
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return
	}
	size := info.Size()
	if !t.seeded {
		// A fresh tracker cares about now, not about every model move
		// since the log was created.
		t.seeded = true
		t.offset = size - catchUp
		if t.offset < 0 {
			t.offset = 0
		}
	}
	if size < t.offset {
		// Truncated or replaced: start over from the beginning.
		t.offset = 0
	}
	n := size - t.offset
	if n <= 0 {
		return
	}
	if n > maxRead {
		// Skipped ahead: the models named in the part we jumped over
		// are stale anyway, and the tail carries the current truth.
		t.offset = size - maxRead
		n = maxRead
	}
	buf := make([]byte, n)
	read, err := f.ReadAt(buf, t.offset)
	if read <= 0 {
		return
	}
	buf = buf[:read]
	// Stop at the last complete line; the rest arrives next time.
	end := strings.LastIndexByte(string(buf), '\n')
	if end < 0 {
		return
	}
	t.offset += int64(end) + 1
	for _, line := range strings.Split(string(buf[:end]), "\n") {
		t.apply(line)
	}
}

// apply updates residency from one daemon log line. The lines are JSON
// records wrapping the engine's own output, but every marker of
// interest is plain ASCII inside the message, so matching on the raw
// text avoids decoding hundreds of records that say nothing.
func (t *modelTracker) apply(line string) {
	switch {
	case strings.Contains(line, "[dit-from-disk] evicting DiT"):
		delete(t.live, "dit")
		return
	case strings.Contains(line, "Loaded LLM to cuda"):
		t.load("llm", "planner LM", 0, false)
		return
	case strings.Contains(line, "Offloading LLM to"), strings.Contains(line, "Offloaded LLM to"):
		delete(t.live, "llm")
		return
	case strings.Contains(line, "Loaded model from disk to cuda"):
		t.load("dit", "diffusion DiT", 0, true)
		return
	}

	const marker = "[_load_model_context] "
	i := strings.Index(line, marker)
	if i < 0 {
		return
	}
	rest := line[i+len(marker):]
	verb, rest, ok := strings.Cut(rest, " ")
	if !ok {
		return
	}
	name, rest, ok := strings.Cut(rest, " ")
	if !ok {
		return
	}
	key, label := modelLabel(name)
	if key == "" {
		return
	}
	switch verb {
	case "Loaded":
		if !strings.HasPrefix(rest, "to cuda") {
			return
		}
		// The engine reports the system memory it gave up, which is
		// exactly what the weights now occupy on the card.
		t.load(key, label, deltaBytes(rest), false)
	case "Offloading", "Offloaded":
		delete(t.live, key)
	}
}

// load records a model as resident, keeping the time of the first load
// in an unbroken run so the display can say how long it has been there.
func (t *modelTracker) load(key, label string, bytes uint64, fromDisk bool) {
	m, had := t.live[key]
	if !had {
		m = Model{Name: label, Since: time.Now()}
	}
	m.Name = label
	m.FromDisk = fromDisk
	if bytes > 0 {
		m.Bytes = bytes
	}
	t.live[key] = m
}

// modelLabel maps the engine's internal model names to plain words,
// and rejects anything it does not recognise.
func modelLabel(name string) (key, label string) {
	switch name {
	case "vae":
		return "vae", "audio VAE"
	case "text_encoder":
		return "text_encoder", "text encoder"
	case "model":
		return "dit", "diffusion DiT"
	}
	return "", ""
}

// deltaBytes pulls the "delta: -644 MB" figure out of a load line and
// returns it as a positive byte count. Loading to the card frees system
// memory, so the delta is negative; anything else is not a load we can
// size.
func deltaBytes(s string) uint64 {
	i := strings.Index(s, "delta: ")
	if i < 0 {
		return 0
	}
	f := strings.Fields(s[i+len("delta: "):])
	// The engine's line sits inside a JSON record, so the unit arrives
	// with whatever closes the record still attached to it.
	if len(f) < 2 || !strings.HasPrefix(f[1], "MB") {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSuffix(f[0], ","), 10, 64)
	if err != nil || n >= 0 {
		return 0
	}
	return uint64(-n) * 1024 * 1024
}
