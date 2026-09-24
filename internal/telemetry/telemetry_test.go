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
	if got[0].Name != "audio decoder" || got[0].Bytes != 644*1024*1024 {
		t.Errorf("vae recorded as %q %d bytes", got[0].Name, got[0].Bytes)
	}
	if got[1].Name != "prompt reader" || got[1].Bytes != 1137*1024*1024 {
		t.Errorf("text encoder recorded as %q %d bytes", got[1].Name, got[1].Bytes)
	}

	appendLog(`[_load_model_context] Offloading vae to CPU (RSS: 3484 MB)`)
	got = tr.resident()
	if len(got) != 1 || got[0].Name != "prompt reader" {
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
		case "music generator":
			dit = &got[i]
		case "song planner":
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
	s := New(path, nil, nil)
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
	// And with nobody saying the writer is at work, nothing of it is
	// the radio's either.
	if got.WriterBusy || got.WriterVRAM != 0 || got.WriterRAM != 0 || got.WriterName != "" {
		t.Errorf("unexpected writer attribution: %+v", got)
	}
}

// A fake process table in the shape of this machine's: the radio, its
// engine daemon supervising a Python server, the writer's daemon
// running a model in a runner it spawned, and somebody else's Python
// holding the card too.
func fakeProcTable() procTable {
	return newProcTable(
		map[int]int{
			100: 1, 200: 100, // iar, and the engine daemon it started
			210:  200,           // the Python engine the daemon supervises
			2084: 1, 3001: 2084, // ollama serve, and its llama-server
			42: 1, // an unrelated program
		},
		map[int]string{
			100: "iar", 200: "iar", 210: "python",
			2084: "ollama", 3001: "llama-server",
			42: "python",
		})
}

// The writer runs as a service of its own, never as a child of
// anything here, so it is found by name: the daemon and whatever it
// spawned, and nothing else.
func TestWriterTreeIsTheDaemonAndWhatItSpawned(t *testing.T) {
	table := fakeProcTable()
	got := table.writerTree()
	want := map[int]bool{2084: true, 3001: true}
	if len(got) != len(want) {
		t.Fatalf("writer tree = %v, want the daemon and its runner", got)
	}
	for _, p := range got {
		if !want[p] {
			t.Errorf("writer tree claims pid %d", p)
		}
	}
	if name := table.writerName(got); name != "llama-server" {
		t.Errorf("writer named %q, want its runner", name)
	}
	// A daemon with no runner is named after itself; no daemon, no name.
	if name := table.writerName([]int{2084}); name != "ollama" {
		t.Errorf("a lone daemon is named %q", name)
	}
	if name := table.writerName(nil); name != "" {
		t.Errorf("an empty tree is named %q", name)
	}
	// The engine's own tree is still the daemon and its children.
	if eng := table.descendants(200); len(eng) != 2 || eng[0] != 200 || eng[1] != 210 {
		t.Errorf("engine tree = %v", eng)
	}
}

// Whose the card's holders are: the engine's through its ancestry, the
// writer's by name while - and only while - the writer is working for
// the radio. The rest is shared, and the radio's card figure is the
// engine plus the writer.
func TestAttributeCountsTheWriterOnlyWhileItWorksForTheRadio(t *testing.T) {
	table := fakeProcTable()
	held := func() []GPUProc {
		return []GPUProc{
			{PID: 210, Name: "python (acestep-api)", VRAM: 2 << 30},
			{PID: 3001, Name: "llama-server", VRAM: 5 << 30},
			{PID: 42, Name: "python", VRAM: 3 << 30},
		}
	}

	busy := Sample{WriterBusy: true}
	attribute(&busy, held(), table.parent, 200, table.writerTree())
	if busy.EngineVRAM != 2<<30 {
		t.Errorf("engine holds %d, want 2 GiB", busy.EngineVRAM)
	}
	if busy.WriterVRAM != 5<<30 || busy.WriterName != "llama-server" {
		t.Errorf("writer attributed as %q %d bytes, want llama-server 5 GiB", busy.WriterName, busy.WriterVRAM)
	}
	if busy.RadioVRAM() != 7<<30 {
		t.Errorf("radio holds %d, want engine plus writer (7 GiB)", busy.RadioVRAM())
	}
	// Largest first, and each holder marked for whoever it is.
	if len(busy.Procs) != 3 || busy.Procs[0].PID != 3001 {
		t.Fatalf("procs = %+v", busy.Procs)
	}
	for _, p := range busy.Procs {
		switch p.PID {
		case 210:
			if !p.Engine || p.Writer {
				t.Errorf("engine process marked %+v", p)
			}
		case 3001:
			if !p.Writer || p.Engine || !p.Radio() {
				t.Errorf("writer process marked %+v", p)
			}
		case 42:
			if p.Radio() {
				t.Errorf("somebody else's process claimed for the radio: %+v", p)
			}
		}
	}

	// The same holders while the writer is not working for the radio:
	// it is somebody else's, and the shared row gets it.
	idle := Sample{}
	attribute(&idle, held(), table.parent, 200, table.writerTree())
	if idle.WriterVRAM != 0 || idle.WriterName != "" {
		t.Errorf("an idle writer was attributed to the radio: %q %d", idle.WriterName, idle.WriterVRAM)
	}
	if idle.RadioVRAM() != 2<<30 {
		t.Errorf("radio holds %d, want the engine alone (2 GiB)", idle.RadioVRAM())
	}
	for _, p := range idle.Procs {
		if p.PID == 3001 && p.Radio() {
			t.Errorf("idle writer marked as the radio's: %+v", p)
		}
	}
}

// A runner the tree walk did not catch (reparented, or a daemon that
// was renamed) is still the writer's when its name says so - and only
// while the writer is working. A writer on another machine leaves no
// process here and contributes nothing.
func TestAttributeRecognisesTheRunnerByName(t *testing.T) {
	held := []GPUProc{{PID: 777, Name: "llama-server", VRAM: 4 << 30}}
	busy := Sample{WriterBusy: true}
	attribute(&busy, held, map[int]int{777: 1}, 0, nil)
	if busy.WriterVRAM != 4<<30 || busy.WriterName != "llama-server" || !busy.Procs[0].Writer {
		t.Errorf("a runner found by name was not the writer's: %+v", busy)
	}
	elsewhere := Sample{WriterBusy: true}
	attribute(&elsewhere, nil, map[int]int{}, 0, nil)
	if elsewhere.WriterVRAM != 0 || elsewhere.WriterName != "" || len(elsewhere.Procs) != 0 {
		t.Errorf("a writer with no process here contributed: %+v", elsewhere)
	}
	// An older daemon ran its model in a second "ollama" process.
	old := Sample{WriterBusy: true}
	attribute(&old, []GPUProc{{PID: 778, Name: "ollama", VRAM: 1 << 30}}, map[int]int{}, 0, nil)
	if old.WriterVRAM != 1<<30 {
		t.Errorf("an older runner was not recognised: %+v", old)
	}
}

// The radio's processor share adds the writer's work only over the
// interval it was measured across: the moment the writer joins the
// figure, its lifetime of accumulated time must not land in one sample
// and read as a machine pegged at 100%.
func TestSelfTreeFoldsTheWriterInByTheInterval(t *testing.T) {
	self := os.Getpid()
	s := &Sampler{lastTotalDelta: 1 << 40}
	// Prime the radio's own counter; the writer's is still unknown.
	first := Sample{CPUUtil: 10}
	s.selfTree([]int{self}, nil, &first)
	if s.prevSelfJiffies == 0 {
		t.Skip("no /proc on this platform")
	}
	// The writer appears and is at work: its memory counts from this
	// sample, its processor time only from the next.
	second := Sample{CPUUtil: 10, WriterBusy: true}
	rss, cpu := s.selfTree([]int{self}, []int{self}, &second)
	if second.WriterRAM == 0 || rss < second.WriterRAM*3/2 {
		t.Errorf("writer memory not folded in: self %d, writer %d", rss, second.WriterRAM)
	}
	if cpu < 0 {
		t.Errorf("a measured radio tree should have a share, got %d", cpu)
	}
	if s.prevWriterJiffies == 0 {
		t.Error("the writer's counter should be primed for the next sample")
	}
	// Not at work: measured still (so the next join is by interval),
	// but nothing of it counted.
	third := Sample{CPUUtil: 10}
	rss3, _ := s.selfTree([]int{self}, []int{self}, &third)
	if third.WriterRAM != 0 || rss3 > rss {
		t.Errorf("an idle writer was folded in: rss %d (was %d), writer %d", rss3, rss, third.WriterRAM)
	}
}

func TestCPUShareClamps(t *testing.T) {
	for _, tc := range []struct {
		delta uint64
		total int64
		want  int
	}{
		{0, 100, 0},
		{25, 100, 25},
		{100, 100, 100},
		{250, 100, 100}, // a torn read never exceeds the machine
		{5, 0, -1},      // no interval yet
	} {
		if got := cpuShare(tc.delta, tc.total); got != tc.want {
			t.Errorf("cpuShare(%d, %d) = %d, want %d", tc.delta, tc.total, got, tc.want)
		}
	}
}

// The process table reads the short name and parent out of one stat
// line, including a name with spaces and parentheses in it.
func TestReadStatOfThisProcess(t *testing.T) {
	comm, ppid, ok := readStat(os.Getpid())
	if !ok {
		t.Skip("no /proc on this platform")
	}
	if comm == "" || ppid != os.Getppid() {
		t.Errorf("readStat = %q, %d; want this process's name and parent %d", comm, ppid, os.Getppid())
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
