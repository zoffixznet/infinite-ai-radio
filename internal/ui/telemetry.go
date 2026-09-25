package ui

import (
	"fmt"
	"strings"
	"time"

	"iar/internal/player"
	"iar/internal/telemetry"
)

// telemetryRow is one labelled line of the resource readout, with the
// fraction its gauge should show (negative when the row has no gauge).
type telemetryRow struct {
	Label string
	Frac  float64
	// FracOwn, when positive, is the leading share of the bar drawn in
	// the radio's own color: the part of the machine's usage that is
	// this program and its engine.
	FracOwn float64
	Text    string
}

// telemetryRows turns the status's resource sample into the lines the
// displays show. Both the full-screen and plain interfaces render from
// this, so the numbers never disagree between them. The rest of the
// status is consulted for what the radio is doing: to the listener
// there is one engine and it makes songs, so the rows that say "radio"
// count everything working for it at that moment - the engine and,
// while the words are being written, the writer - and the row that
// names what is on the card says "asleep" only when nothing is being
// made at all.
func telemetryRows(st player.Status) []telemetryRow {
	s := st.Telemetry
	if s == nil || s.Taken.IsZero() {
		return nil
	}
	rows := []telemetryRow{cpuRow(s), ramRow(s), vramRow(s)}
	rows = append(rows, modelRow(st))
	if row, ok := sharedRow(s); ok {
		rows = append(rows, row)
	}
	return rows
}

func cpuRow(s *telemetry.Sample) telemetryRow {
	r := telemetryRow{Label: "cpu", Frac: -1}
	if s.CPUUtil < 0 {
		r.Text = fmt.Sprintf("measuring… · load %.2f", s.Load1)
		return r
	}
	r.Frac = float64(s.CPUUtil) / 100
	if s.CPUSelf >= 0 {
		r.FracOwn = float64(s.CPUSelf) / 100
		r.Text = fmt.Sprintf("radio %d%% · all %d%% · load %.2f", s.CPUSelf, s.CPUUtil, s.Load1)
	} else {
		r.Text = fmt.Sprintf("%d%% busy · load %.2f", s.CPUUtil, s.Load1)
	}
	return r
}

func ramRow(s *telemetry.Sample) telemetryRow {
	r := telemetryRow{Label: "ram", Frac: -1}
	if s.RAMTotal == 0 {
		r.Text = "system memory unavailable"
		return r
	}
	r.Frac = float64(s.RAMUsed) / float64(s.RAMTotal)
	r.FracOwn = float64(s.RAMSelf) / float64(s.RAMTotal)
	r.Text = fmt.Sprintf("radio %s · all %s / %s",
		gib(s.RAMSelf), gib(s.RAMUsed), gib(s.RAMTotal))
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
	// The radio's share is everything held for it: the engine's models
	// and, while the words are being written, the writer's.
	if s.VRAMTotal > 0 {
		r.Frac = float64(s.VRAMUsed) / float64(s.VRAMTotal)
		r.FracOwn = float64(s.RadioVRAM()) / float64(s.VRAMTotal)
	}
	r.Text = fmt.Sprintf("radio %s · all %s / %s · gpu %d%% · %d°C",
		gib(s.RadioVRAM()), gib(s.VRAMUsed), gib(s.VRAMTotal), s.GPUUtil, s.GPUTemp)
	return r
}

// modelRow names what is on the card for the radio: the engine's
// models while it plans and renders, the writer while it writes, and
// otherwise why nothing is - the same facts the buffer row draws on.
// An empty card with the engine asleep is the good news phased
// generation is built to produce; an empty card while the words are
// being written is not the engine idle, and the row does not say so.
func modelRow(st player.Status) telemetryRow {
	s := st.Telemetry
	r := telemetryRow{Label: "models", Frac: -1}
	parts := make([]string, 0, len(s.Models)+1)
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
	if s.WriterBusy && s.WriterVRAM > 0 {
		parts = append(parts, fmt.Sprintf("the writer (%s %s)", s.WriterName, gib(s.WriterVRAM)))
	}
	if len(parts) > 0 {
		r.Text = strings.Join(parts, " · ")
		if len(s.Models) > 0 && s.EngineVRAM > 0 {
			r.Text += fmt.Sprintf("  [engine total %s]", gib(s.EngineVRAM))
		}
		return r
	}
	switch {
	case s.EnginePID > 0 && !st.EngineReady:
		r.Text = "none on the card yet - engine starting"
	case s.EnginePID > 0 && (st.Generating || st.Exporting != ""):
		r.Text = "none on the card yet - engine at work"
	case s.EnginePID > 0:
		r.Text = "none on the card - engine idle"
	case s.WriterBusy || writing(st):
		// The writer answers from the processor, or from another
		// machine: writing for the radio either way, just not here.
		r.Text = "none on the card - the writer is working elsewhere"
	default:
		r.Text = "none - engine asleep"
		if why := asleepWhy(st); why != "" {
			r.Text += ": " + why
		}
	}
	return r
}

// asleepWhy says why nothing is being made, in the words the buffer
// row uses; empty when neither of its two reasons holds (the engine is
// about to wake, or a cooldown after failures is running). A stocked
// store says how much is in it and the mark the engine wakes below,
// because that - not a clock - is what ends the sleep.
func asleepWhy(st player.Status) string {
	switch {
	case stocked(st):
		return fmt.Sprintf("%d songs in store, %s of music; wakes below %d",
			st.StoreLevel, fmtSpan(st.StoreSeconds), st.WakeBelow)
	case st.NextBatchIn > 0:
		return "next batch due in " + fmtSpan(st.NextBatchIn.Seconds())
	}
	return ""
}

// sharedRow names what else is on the card: another program sharing
// the machine, or the radio's own lyric writer while it is not writing
// the radio's songs (it runs as a service of its own, answering
// whoever asks - the radio's own health check and steers included -
// and only counts as the radio's while it writes the radio's songs).
// Without this row the only thing the readout could say while another
// program held the card was that the radio held little - true, and a
// poor answer to "what is using my graphics card". Named by process
// and never folded into the radio's own figure: a process list can say
// who is holding memory, not whose work they are doing.
func sharedRow(s *telemetry.Sample) (telemetryRow, bool) {
	r := telemetryRow{Label: "shared", Frac: -1}
	parts := make([]string, 0, len(s.Procs))
	for _, p := range s.Procs {
		if p.Radio() || p.VRAM == 0 {
			continue
		}
		name := p.Name
		if name == "" {
			name = fmt.Sprintf("pid %d", p.PID)
		}
		parts = append(parts, name+" "+gib(p.VRAM))
	}
	if len(parts) == 0 {
		return r, false
	}
	// Largest first already; a card with a crowd on it says so rather
	// than pushing every other row off a narrow terminal.
	if len(parts) > 3 {
		parts = append(parts[:3], fmt.Sprintf("and %d more", len(parts)-3))
	}
	r.Text = strings.Join(parts, " · ")
	return r, true
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
