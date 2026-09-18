package ui

import (
	"strings"
	"testing"
	"time"

	"iar/internal/telemetry"
)

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
	if got := telemetryRows(nil); got != nil {
		t.Errorf("no sampler should render nothing, got %v", got)
	}
	// A sampler that has not measured yet is not news either.
	if got := telemetryRows(&telemetry.Sample{}); got != nil {
		t.Errorf("unmeasured sample should render nothing, got %v", got)
	}
}

func TestTelemetryRowsHibernatedEngine(t *testing.T) {
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
	rows := telemetryRows(s)

	if got := rowText(t, rows, "ram"); !strings.Contains(got, "all 16.00 GiB / 32.00 GiB") {
		t.Errorf("ram row reads %q", got)
	}
	if got := rowText(t, rows, "vram"); !strings.Contains(got, "all 4.00 GiB / 12.00 GiB") {
		t.Errorf("vram row reads %q", got)
	}
	// The whole point of phased generation: nothing of ours resident.
	if got := rowText(t, rows, "models"); !strings.Contains(got, "hibernated") {
		t.Errorf("models row should report hibernation, reads %q", got)
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
	rows := telemetryRows(s)
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
	rows := telemetryRows(s)
	got := rowText(t, rows, "vram")
	if !strings.Contains(got, "no graphics card found") || !strings.Contains(got, "not found") {
		t.Errorf("vram row reads %q", got)
	}
}

// The readout used to have a blind spot with the shape of the radio's
// own lyric helper: it runs as its own service, so the engine's
// ancestry never claims it, and while it held the card the only thing
// the screen could say was that the radio held nothing. True of the
// engine, and no answer at all to "what is using my graphics card".
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
	rows := telemetryRows(s)

	// The engine really is hibernated, and the rows about the engine
	// must go on saying so: this is a new fact, not a correction.
	if got := rowText(t, rows, "models"); !strings.Contains(got, "hibernated") {
		t.Errorf("models row reads %q", got)
	}
	if got := rowText(t, rows, "vram"); !strings.HasPrefix(got, "radio 0 ") {
		t.Errorf("vram row should still claim nothing for the engine, reads %q", got)
	}
	shared := rowText(t, rows, "shared")
	for _, want := range []string{"ollama", "4.00 GiB", "python (src.main)", "3.81 GiB"} {
		if !strings.Contains(shared, want) {
			t.Errorf("shared row %q is missing %q", shared, want)
		}
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
	rows := telemetryRows(s)
	for _, r := range rows {
		if r.Label == "shared" {
			t.Fatalf("a card the radio has to itself grew a shared row: %q", r.Text)
		}
	}
	if got := rowText(t, rows, "vram"); !strings.HasPrefix(got, "radio 6.00 GiB") {
		t.Errorf("vram row reads %q", got)
	}
}
