package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Phase names used for progress estimates.
const (
	PhaseEngineStart = "engine_start" // spawn until the API port answers
	PhaseModelLoad   = "model_load"   // port answering until models loaded
	PhaseFirstTrack  = "first_track"  // engine ready until first track done
)

// defaultExpected seeds estimates before any measurement exists.
var defaultExpected = map[string]time.Duration{
	PhaseEngineStart: 15 * time.Second,
	PhaseModelLoad:   60 * time.Second,
	PhaseFirstTrack:  40 * time.Second,
}

// keepSamples is how many recent measurements are kept per phase.
const keepSamples = 5

// Timings keeps a rolling record of how long startup phases take, so
// progress displays can show "usually ~Ns" estimates.
type Timings struct {
	mu   sync.Mutex
	path string
	data map[string][]float64 // seconds, newest last
}

// NewTimings loads (or initializes) the timings store in dir.
func NewTimings(dir Dir) *Timings {
	t := &Timings{
		path: filepath.Join(dir.Path(), "timings.json"),
		data: map[string][]float64{},
	}
	if raw, err := os.ReadFile(t.path); err == nil {
		json.Unmarshal(raw, &t.data) // best effort; corrupt file = fresh start
	}
	return t
}

// Expected returns the estimated duration of a phase (median of recent
// measurements, or a built-in default).
func (t *Timings) Expected(phase string) time.Duration {
	if t == nil {
		if d, ok := defaultExpected[phase]; ok {
			return d
		}
		return 30 * time.Second
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	samples := append([]float64(nil), t.data[phase]...)
	if len(samples) == 0 {
		if d, ok := defaultExpected[phase]; ok {
			return d
		}
		return 30 * time.Second
	}
	sort.Float64s(samples)
	return time.Duration(samples[len(samples)/2] * float64(time.Second))
}

// Record stores a completed phase duration and persists the file.
func (t *Timings) Record(phase string, d time.Duration) {
	if t == nil || d <= 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	s := append(t.data[phase], d.Seconds())
	if len(s) > keepSamples {
		s = s[len(s)-keepSamples:]
	}
	t.data[phase] = s
	raw, err := json.MarshalIndent(t.data, "", "  ")
	if err != nil {
		return
	}
	tmp := t.path + ".tmp"
	if os.WriteFile(tmp, raw, 0o644) == nil {
		os.Rename(tmp, t.path)
	}
}
