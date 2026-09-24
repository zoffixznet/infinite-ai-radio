package ui

import (
	"strings"
	"testing"
	"time"

	"iar/internal/player"
	"iar/internal/telemetry"
)

// withSample is a status carrying only a resource sample, for the rows
// that read nothing else.
func withSample(s *telemetry.Sample) player.Status {
	return player.Status{Telemetry: s}
}

func rowText(t *testing.T, rows []telemetryRow, label string) string {
	t.Helper()
	for _, r := range rows {
		if r.Label == label {
			return r.Text
		}
	}
	t.Fatalf("no %q row in %v", label, rows)
	return ""
}

func TestTelemetryRowsEmptyWithoutASample(t *testing.T) {
	if got := telemetryRows(player.Status{}); got != nil {
		t.Errorf("no sampler should render nothing, got %v", got)
	}
	// A sampler that has not measured yet is not news either.
	if got := telemetryRows(withSample(&telemetry.Sample{})); got != nil {
		t.Errorf("unmeasured sample should render nothing, got %v", got)
	}
}

func TestTelemetryRowsAsleepEngine(t *testing.T) {
	s := &telemetry.Sample{
		Taken:      time.Now(),
		RAMTotal:   32 << 30,
		RAMUsed:    16 << 30,
		SwapTotal:  16 << 30,
		GPUPresent: true,
		VRAMTotal:  12 << 30,
		VRAMUsed:   4 << 30,
		VRAMFree:   8 << 30,
		Procs:      []telemetry.GPUProc{{PID: 42, Name: "python (src.main)", VRAM: 4 << 30}},
	}
	s.RAMAvailable = s.RAMTotal - s.RAMUsed
	st := withSample(s)
	st.StoreLevel, st.StoreTarget = 72, 72
	rows := telemetryRows(st)

	if got := rowText(t, rows, "ram"); !strings.Contains(got, "all 16.00 GiB / 32.00 GiB") {
		t.Errorf("ram row reads %q", got)
	}
	if got := rowText(t, rows, "vram"); !strings.Contains(got, "all 4.00 GiB / 12.00 GiB") {
		t.Errorf("vram row reads %q", got)
	}
	// The whole point of phased generation: nothing of ours resident,
	// and the row says why nothing is being made.
	if got := rowText(t, rows, "models"); got != "none - engine asleep: the store is full" {
		t.Errorf("models row should report the engine asleep and why, reads %q", got)
	}
}

// "Asleep" is said only when nothing is being made at all, with the
// reason the buffer row gives; a stopped daemon that is about to wake
// gets no reason it cannot back up.
func TestModelRowSaysWhyTheEngineIsAsleep(t *testing.T) {
	sample := func() *telemetry.Sample {
		return &telemetry.Sample{Taken: time.Now(), RAMTotal: 32 << 30, GPUPresent: true, VRAMTotal: 12 << 30}
	}
	for _, tc := range []struct {
		name string
		st   player.Status
		want string
	}{
		{"store full", player.Status{Telemetry: sample(), StoreLevel: 72, StoreTarget: 72},
			"none - engine asleep: the store is full"},
		{"waiting out the clock", player.Status{Telemetry: sample(), StoreLevel: 9, StoreTarget: 72, NextBatchIn: 12 * time.Minute},
			"none - engine asleep: next batch due in 12m"},
		{"about to wake", player.Status{Telemetry: sample(), StoreLevel: 9, StoreTarget: 72, RampBatch: 20},
			"none - engine asleep"},
	} {
		if got := rowText(t, telemetryRows(tc.st), "models"); got != tc.want {
			t.Errorf("%s: models row reads %q, want %q", tc.name, got, tc.want)
		}
	}
}

// While the words are being written the radio is making songs: the
// row names the writer and what it holds, never the engine asleep.
func TestModelRowNamesTheWriterWhileItWrites(t *testing.T) {
	s := &telemetry.Sample{
		Taken: time.Now(), RAMTotal: 32 << 30, GPUPresent: true, VRAMTotal: 12 << 30,
		VRAMUsed: 6 << 30, WriterBusy: true, WriterName: "llama-server", WriterVRAM: 5900 << 20,
		Procs: []telemetry.GPUProc{{PID: 3001, Name: "llama-server", VRAM: 5900 << 20, Writer: true}},
	}
	st := player.Status{Telemetry: s, Vocal: true, WordsmithWant: 10, WordsmithWrote: 4, StoreLevel: 9, StoreTarget: 72}
	rows := telemetryRows(st)
	if got := rowText(t, rows, "models"); got != "the writer (llama-server 5.76 GiB)" {
		t.Errorf("models row reads %q", got)
	}
	// The writer's card memory is the radio's, and not "shared".
	if got := rowText(t, rows, "vram"); !strings.HasPrefix(got, "radio 5.76 GiB") {
		t.Errorf("vram row reads %q", got)
	}
	for _, r := range rows {
		if r.Label == "shared" {
			t.Errorf("the radio's own writer landed in the shared row: %q", r.Text)
		}
	}

	// A writer with nothing on the card - answering from the
	// processor, or from another machine - is still at work.
	elsewhere := &telemetry.Sample{Taken: time.Now(), RAMTotal: 32 << 30, GPUPresent: true, VRAMTotal: 12 << 30, WriterBusy: true}
	if got := rowText(t, telemetryRows(player.Status{Telemetry: elsewhere}), "models"); got != "none on the card - the writer is working elsewhere" {
		t.Errorf("a writer elsewhere reads %q", got)
	}
	// So is a wordsmith round the sampler has not caught up with yet.
	quiet := &telemetry.Sample{Taken: time.Now(), RAMTotal: 32 << 30, GPUPresent: true, VRAMTotal: 12 << 30}
	round := player.Status{Telemetry: quiet, WordsmithWant: 10, Vocal: true, StoreLevel: 72, StoreTarget: 72}
	if got := rowText(t, telemetryRows(round), "models"); strings.Contains(got, "asleep") {
		t.Errorf("a wordsmith round reads as asleep: %q", got)
	}
}

// A live daemon with nothing on the card is starting, at work, or
// idle - and only the last of those is called idle.
func TestModelRowWithTheDaemonUpAndTheCardEmpty(t *testing.T) {
	sample := func() *telemetry.Sample {
		return &telemetry.Sample{Taken: time.Now(), RAMTotal: 32 << 30, GPUPresent: true, VRAMTotal: 12 << 30, EnginePID: 99}
	}
	for _, tc := range []struct {
		name string
		st   player.Status
		want string
	}{
		{"starting", player.Status{Telemetry: sample(), EngineName: "acestep"}, "none on the card yet - engine starting"},
		{"between jobs of a batch", player.Status{Telemetry: sample(), EngineName: "acestep", EngineReady: true, Generating: true},
			"none on the card yet - engine at work"},
		{"exporting", player.Status{Telemetry: sample(), EngineName: "acestep", EngineReady: true, Exporting: "10 min"},
			"none on the card yet - engine at work"},
		{"idle", player.Status{Telemetry: sample(), EngineName: "acestep", EngineReady: true}, "none on the card - engine idle"},
	} {
		if got := rowText(t, telemetryRows(tc.st), "models"); got != tc.want {
			t.Errorf("%s: models row reads %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestTelemetryRowsNamesResidentModels(t *testing.T) {
	s := &telemetry.Sample{
		Taken:      time.Now(),
		RAMTotal:   32 << 30,
		GPUPresent: true,
		VRAMTotal:  12 << 30,
		EnginePID:  99,
		EngineVRAM: 6 << 30,
		Procs: []telemetry.GPUProc{
			{PID: 99, Name: "python", VRAM: 6 << 30, Engine: true},
			{PID: 42, Name: "python (src.main)", VRAM: 4 << 30},
		},
		Models: []telemetry.Model{
			{Name: "audio decoder", Bytes: 644 << 20, Since: time.Now()},
			{Name: "music generator", Since: time.Now(), FromDisk: true},
		},
	}
	rows := telemetryRows(withSample(s))
	models := rowText(t, rows, "models")
	for _, want := range []string{"audio decoder", "644 MiB", "music generator", "streamed from disk", "engine total 6.00 GiB"} {
		if !strings.Contains(models, want) {
			t.Errorf("models row %q is missing %q", models, want)
		}
	}
	// The radio's share leads the vram row.
	if got := rowText(t, rows, "vram"); !strings.HasPrefix(got, "radio 6.00 GiB") {
		t.Errorf("vram row reads %q", got)
	}
}

func TestTelemetryRowsReportsAMissingCard(t *testing.T) {
	s := &telemetry.Sample{Taken: time.Now(), RAMTotal: 8 << 30, GPUError: "nvidia-smi: not found"}
	rows := telemetryRows(withSample(s))
	got := rowText(t, rows, "vram")
	if !strings.Contains(got, "no graphics card found") || !strings.Contains(got, "not found") {
		t.Errorf("vram row reads %q", got)
	}
}

// The lyric writer runs as its own service, so the engine's ancestry
// never claims it. Answering somebody else, it is another program on
// the card and the shared row names it; the radio is asleep and holds
// nothing. Answering the radio, the very same process is the radio's.
func TestTelemetryRowsNamesWhatElseHoldsTheCard(t *testing.T) {
	s := &telemetry.Sample{
		Taken:      time.Now(),
		RAMTotal:   32 << 30,
		GPUPresent: true,
		VRAMTotal:  12 << 30,
		VRAMUsed:   8 << 30,
		Procs: []telemetry.GPUProc{
			{PID: 2084, Name: "ollama", VRAM: 4 << 30},
			{PID: 3729075, Name: "python (src.main)", VRAM: 3900 << 20},
		},
	}
	st := withSample(s)
	st.StoreLevel, st.StoreTarget = 72, 72
	rows := telemetryRows(st)

	// Nothing is being made: the engine really is asleep, and the rows
	// about it say so.
	if got := rowText(t, rows, "models"); !strings.Contains(got, "asleep") {
		t.Errorf("models row reads %q", got)
	}
	if got := rowText(t, rows, "vram"); !strings.HasPrefix(got, "radio 0 ") {
		t.Errorf("vram row should claim nothing for the radio, reads %q", got)
	}
	shared := rowText(t, rows, "shared")
	for _, want := range []string{"ollama", "4.00 GiB", "python (src.main)", "3.81 GiB"} {
		if !strings.Contains(shared, want) {
			t.Errorf("shared row %q is missing %q", shared, want)
		}
	}

	// The same holders, with the writer now answering the radio: its
	// memory moves from "shared" to "radio", and the rows say the words
	// are being written rather than that the engine is asleep.
	s.WriterBusy, s.WriterName, s.WriterVRAM = true, "ollama", 4<<30
	s.Procs[0].Writer = true
	rows = telemetryRows(st)
	if got := rowText(t, rows, "vram"); !strings.HasPrefix(got, "radio 4.00 GiB") {
		t.Errorf("vram row should count the writer for the radio, reads %q", got)
	}
	if got := rowText(t, rows, "models"); got != "the writer (ollama 4.00 GiB)" {
		t.Errorf("models row reads %q", got)
	}
	shared = rowText(t, rows, "shared")
	if strings.Contains(shared, "ollama") || !strings.Contains(shared, "python (src.main)") {
		t.Errorf("shared row should hold only the other program, reads %q", shared)
	}
}

// Another program's memory is never folded into the radio's own figure,
// and a card the radio has to itself grows no row at all.
func TestTelemetryRowsKeepsTheEngineOutOfTheSharedRow(t *testing.T) {
	s := &telemetry.Sample{
		Taken:      time.Now(),
		RAMTotal:   32 << 30,
		GPUPresent: true,
		VRAMTotal:  12 << 30,
		VRAMUsed:   6 << 30,
		EnginePID:  99,
		EngineVRAM: 6 << 30,
		Procs: []telemetry.GPUProc{
			{PID: 99, Name: "python (acestep-api)", VRAM: 6 << 30, Engine: true},
		},
	}
	rows := telemetryRows(withSample(s))
	for _, r := range rows {
		if r.Label == "shared" {
			t.Fatalf("a card the radio has to itself grew a shared row: %q", r.Text)
		}
	}
	if got := rowText(t, rows, "vram"); !strings.HasPrefix(got, "radio 6.00 GiB") {
		t.Errorf("vram row reads %q", got)
	}
}
