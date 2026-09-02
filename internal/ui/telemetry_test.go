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
	if got := rowText(t, rows, "vram"); !strings.Contains(got, "4.00 GiB / 12.00 GiB used") {
		t.Errorf("vram row reads %q", got)
	}
	// Somebody else's process is named, not claimed as ours.
	card := rowText(t, rows, "card")
	if !strings.Contains(card, "python (src.main)") || strings.Contains(card, "this radio") {
		t.Errorf("card row reads %q", card)
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
	// Our share of the card is labelled as ours, and listed first
	// because it is the larger one.
	card := rowText(t, rows, "card")
	if !strings.HasPrefix(card, "this radio's engine 6.00 GiB") {
		t.Errorf("card row reads %q", card)
	}
}

func TestTelemetryRowsReportsAMissingCard(t *testing.T) {
	s := &telemetry.Sample{Taken: time.Now(), RAMTotal: 8 << 30, GPUError: "nvidia-smi: not found"}
	rows := telemetryRows(s)
	got := rowText(t, rows, "vram")
	if !strings.Contains(got, "no graphics card found") || !strings.Contains(got, "not found") {
		t.Errorf("vram row reads %q", got)
	}
	// Without a card there is nobody to list holding it.
	for _, r := range rows {
		if r.Label == "card" {
			t.Errorf("unexpected card row: %q", r.Text)
		}
	}
}
