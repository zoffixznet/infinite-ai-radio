package telemetry

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// daemonLine wraps engine output the way the daemon log records it, so
// the tracker is exercised against the real line shape rather than a
// convenient one.
func daemonLine(payload string) string {
	return `{"time":"2026-09-01T09:00:51.422-04:00","level":"DEBUG","msg":"sidecar output",` +
		`"event":"sidecar_output","stream":"stderr","line":"2026-09-01 09:00:51.422 | INFO | ` +
		payload + `"}` + "\n"
}

func writeLog(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func names(models []Model) []string {
	out := make([]string, 0, len(models))
	for _, m := range models {
		out = append(out, m.Name)
	}
	return out
}

func TestModelTrackerFollowsLoadsAndOffloads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "engine-daemon.log")
	writeLog(t, path, "")
	tr := newModelTracker(path)
	if got := tr.resident(); len(got) != 0 {
		t.Fatalf("fresh tracker should hold nothing, got %v", names(got))
	}

	appendLog := func(lines ...string) {
		t.Helper()
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		for _, l := range lines {
			if _, err := f.WriteString(daemonLine(l)); err != nil {
				t.Fatal(err)
			}
		}
	}

	appendLog(
		`[_load_model_context] Loaded vae to cuda in 0.1717s (RSS: 4128 -> 3484 MB, delta: -644 MB)`,
		`[_load_model_context] Loaded text_encoder to cuda in 0.2236s (RSS: 4044 -> 2907 MB, delta: -1137 MB)`,
	)
	got := tr.resident()
	if len(got) != 2 {
		t.Fatalf("want vae and text encoder resident, got %v", names(got))
	}
	if got[0].Name != "audio VAE" || got[0].Bytes != 644*1024*1024 {
		t.Errorf("vae recorded as %q %d bytes", got[0].Name, got[0].Bytes)
	}
	if got[1].Name != "text encoder" || got[1].Bytes != 1137*1024*1024 {
		t.Errorf("text encoder recorded as %q %d bytes", got[1].Name, got[1].Bytes)
	}

	appendLog(`[_load_model_context] Offloading vae to CPU (RSS: 3484 MB)`)
	got = tr.resident()
	if len(got) != 1 || got[0].Name != "text encoder" {
		t.Fatalf("offloaded vae should be gone, got %v", names(got))
	}
}

func TestModelTrackerDiskDiTAndPlannerLM(t *testing.T) {
	path := filepath.Join(t.TempDir(), "engine-daemon.log")
	writeLog(t, path, daemonLine(`[_load_model_context] Loaded model from disk to cuda in 1.0081s (RSS: 4044 -> 4048 MB)`)+
		daemonLine(`acestep.llm_inference:_load_model_context:4147 - Loaded LLM to cuda in 0.2459s`))
	tr := newModelTracker(path)
	got := tr.resident()
	if len(got) != 2 {
		t.Fatalf("want DiT and planner LM, got %v", names(got))
	}
	var dit, lm *Model
	for i := range got {
		switch got[i].Name {
		case "diffusion DiT":
			dit = &got[i]
		case "planner LM":
			lm = &got[i]
		}
	}
	if dit == nil || !dit.FromDisk {
		t.Errorf("DiT should be recorded as streamed from disk, got %+v", dit)
	}
	if lm == nil {
		t.Errorf("planner LM not recorded, got %v", names(got))
	}

	// The two ways each of them leaves the card again.
	writeLog(t, path, daemonLine(`[_load_model_context] Loaded model from disk to cuda in 1.0s (RSS: 1 -> 2 MB)`)+
		daemonLine(`[dit-from-disk] evicting DiT (weights reload from disk on next use)`)+
		daemonLine(`Offloading LLM to cpu`))
	tr2 := newModelTracker(path)
	if got := tr2.resident(); len(got) != 0 {
		t.Fatalf("evicted DiT and offloaded LM should leave nothing, got %v", names(got))
	}
}

func TestModelTrackerResetForgetsResidency(t *testing.T) {
	path := filepath.Join(t.TempDir(), "engine-daemon.log")
	writeLog(t, path, daemonLine(`[_load_model_context] Loaded vae to cuda in 0.1s (RSS: 2 -> 1 MB, delta: -644 MB)`))
	tr := newModelTracker(path)
	if len(tr.resident()) != 1 {
		t.Fatal("setup: vae should be resident")
	}
	// A hibernated daemon takes everything with it.
	tr.reset()
	if got := tr.resident(); len(got) != 0 {
		t.Fatalf("reset should clear residency, got %v", names(got))
	}
}

// The daemon log runs to hundreds of megabytes over a long run. A
// tracker must never read it whole, or a status display that repaints
// four times a second turns into a disk and allocation storm.
func TestModelTrackerReadsOnlyTheTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "engine-daemon.log")
	filler := daemonLine(`[_load_model_context] Loaded vae to cuda in 0.1s (RSS: 2 -> 1 MB, delta: -644 MB)`)
	big := ""
	for len(big) < 2*catchUp {
		big += filler
	}
	writeLog(t, path, big)
	tr := newModelTracker(path)
	tr.resident()
	if tr.offset < int64(len(big))-catchUp-int64(len(filler)) {
		t.Errorf("tracker started %d bytes from the end, want no more than %d",
			int64(len(big))-tr.offset, catchUp+int64(len(filler)))
	}
}

func TestReadMeminfoReportsThisMachine(t *testing.T) {
	var s Sample
	readMeminfo(&s)
	if s.RAMTotal == 0 {
		t.Skip("no /proc/meminfo on this platform")
	}
	if s.RAMAvailable > s.RAMTotal {
		t.Errorf("available %d exceeds total %d", s.RAMAvailable, s.RAMTotal)
	}
	if s.RAMUsed+s.RAMAvailable != s.RAMTotal {
		t.Errorf("used %d plus available %d should be total %d", s.RAMUsed, s.RAMAvailable, s.RAMTotal)
	}
}

func TestSamplerPublishesASnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "engine-daemon.log")
	writeLog(t, path, "")
	s := New(path, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)
	got := s.Sample()
	if got.Taken.IsZero() {
		t.Fatal("Start should publish a first sample synchronously")
	}
	if time.Since(got.Taken) > time.Minute {
		t.Errorf("sample is stale: %v", got.Taken)
	}
	// With no engine pid there is nothing to attribute to this radio.
	if got.EnginePID != 0 || got.EngineVRAM != 0 {
		t.Errorf("unexpected engine attribution: pid %d, %d bytes", got.EnginePID, got.EngineVRAM)
	}
}

func TestDescendsFromWalksAncestry(t *testing.T) {
	parents := map[int]int{5: 4, 4: 3, 3: 1}
	if !descendsFrom(parents, 5, 3) {
		t.Error("5 descends from 3")
	}
	if descendsFrom(parents, 5, 9) {
		t.Error("5 does not descend from 9")
	}
	// A cycle from a torn read of /proc must not hang.
	if descendsFrom(map[int]int{7: 8, 8: 7}, 7, 99) {
		t.Error("cycle should not report a match")
	}
}

func TestDeltaBytes(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want uint64
	}{
		{"to cuda in 0.17s (RSS: 4128 -> 3484 MB, delta: -644 MB)", 644 * 1024 * 1024},
		{"to CPU in 0.24s (RSS: 3484 -> 4128 MB, delta: +643 MB)", 0},
		{"to cuda in 1.00s (RSS: 4044 -> 4048 MB)", 0},
		// The line arrives wrapped in the daemon's JSON record.
		{`to cuda in 0.17s (RSS: 4128 -> 3484 MB, delta: -644 MB)"}`, 644 * 1024 * 1024},
	} {
		if got := deltaBytes(tc.in); got != tc.want {
			t.Errorf("deltaBytes(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
