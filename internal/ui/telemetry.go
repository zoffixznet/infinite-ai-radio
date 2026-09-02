package ui

import (
	"fmt"
	"strings"
	"time"

	"iar/internal/telemetry"
)

// telemetryRow is one labelled line of the resource readout, with the
// fraction its gauge should show (negative when the row has no gauge).
type telemetryRow struct {
	Label string
	Frac  float64
	Text  string
}

// telemetryRows turns a resource sample into the lines the displays
// show. Both the full-screen and plain interfaces render from this, so
// the numbers never disagree between them.
func telemetryRows(s *telemetry.Sample) []telemetryRow {
	if s == nil || s.Taken.IsZero() {
		return nil
	}
	rows := []telemetryRow{cpuRow(s), ramRow(s), vramRow(s)}
	if r, ok := cardRow(s); ok {
		rows = append(rows, r)
	}
	rows = append(rows, modelRow(s))
	return rows
}

func cpuRow(s *telemetry.Sample) telemetryRow {
	r := telemetryRow{Label: "cpu", Frac: -1}
	if s.CPUUtil < 0 {
		r.Text = fmt.Sprintf("measuring… · load %.2f", s.Load1)
		return r
	}
	r.Frac = float64(s.CPUUtil) / 100
	r.Text = fmt.Sprintf("%d%% busy · load %.2f", s.CPUUtil, s.Load1)
	return r
}

func ramRow(s *telemetry.Sample) telemetryRow {
	r := telemetryRow{Label: "ram", Frac: -1}
	if s.RAMTotal == 0 {
		r.Text = "system memory unavailable"
		return r
	}
	r.Frac = float64(s.RAMUsed) / float64(s.RAMTotal)
	r.Text = fmt.Sprintf("%s / %s used, %s free",
		gib(s.RAMUsed), gib(s.RAMTotal), gib(s.RAMAvailable))
	if s.SwapTotal > 0 {
		r.Text += fmt.Sprintf(" · swap %s / %s", gib(s.SwapUsed), gib(s.SwapTotal))
	}
	return r
}

func vramRow(s *telemetry.Sample) telemetryRow {
	r := telemetryRow{Label: "vram", Frac: -1}
	if !s.GPUPresent {
		r.Text = "no graphics card found"
		if s.GPUError != "" {
			r.Text += " (" + s.GPUError + ")"
		}
		return r
	}
	if s.VRAMTotal > 0 {
		r.Frac = float64(s.VRAMUsed) / float64(s.VRAMTotal)
	}
	r.Text = fmt.Sprintf("%s / %s used, %s free · gpu %d%% · %d°C",
		gib(s.VRAMUsed), gib(s.VRAMTotal), gib(s.VRAMFree), s.GPUUtil, s.GPUTemp)
	return r
}

// cardRow names who is holding graphics memory. The radio shares the
// card with whatever else the machine runs, and knowing which share is
// ours is the whole question.
func cardRow(s *telemetry.Sample) (telemetryRow, bool) {
	if !s.GPUPresent {
		return telemetryRow{}, false
	}
	r := telemetryRow{Label: "card", Frac: -1}
	if len(s.Procs) == 0 {
		r.Text = "nothing on the card"
		return r, true
	}
	parts := make([]string, 0, len(s.Procs))
	for _, p := range s.Procs {
		name := p.Name
		if p.Engine {
			name = "this radio's engine"
		}
		parts = append(parts, fmt.Sprintf("%s %s", name, gib(p.VRAM)))
	}
	r.Text = strings.Join(parts, " · ")
	return r, true
}

// modelRow says which of the engine's models are on the card. An empty
// list is the good news phased generation is built to produce.
func modelRow(s *telemetry.Sample) telemetryRow {
	r := telemetryRow{Label: "models", Frac: -1}
	if len(s.Models) == 0 {
		if s.EnginePID == 0 {
			r.Text = "none - engine hibernated (no process, no memory held)"
		} else {
			r.Text = "none on the card - engine idle, weights in system memory"
		}
		return r
	}
	parts := make([]string, 0, len(s.Models))
	for _, m := range s.Models {
		part := m.Name
		if m.Bytes > 0 {
			part += " " + gib(m.Bytes)
		}
		if m.FromDisk {
			part += " (streamed from disk)"
		}
		if held := time.Since(m.Since); held >= 2*time.Second {
			part += fmt.Sprintf(" %ds", int(held.Seconds()))
		}
		parts = append(parts, part)
	}
	r.Text = strings.Join(parts, " · ")
	if s.EngineVRAM > 0 {
		r.Text += fmt.Sprintf("  [engine total %s]", gib(s.EngineVRAM))
	}
	return r
}

// gib formats a byte count the way a person reads memory: gibibytes
// for the machine's totals, mebibytes for anything under one, which is
// how model sizes are usually quoted.
func gib(b uint64) string {
	const g = 1024 * 1024 * 1024
	if b >= g {
		return fmt.Sprintf("%.2f GiB", float64(b)/float64(g))
	}
	if b == 0 {
		return "0"
	}
	return fmt.Sprintf("%d MiB", b/(1024*1024))
}
