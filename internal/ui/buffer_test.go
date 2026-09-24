package ui

import (
	"strings"
	"testing"
	"time"

	"iar/internal/player"
)

// The bug this replaces: both displays reported len(queue), which in
// phased mode is the two-track in-memory prefetch, measured against the
// fused path's six-track target. It read "0/6 buffered" and "2 track(s)
// ready" while half an hour of music sat on disk.
func TestBufferShowsTheDiskBufferNotThePrefetch(t *testing.T) {
	st := player.Status{
		Queued:          2, // the in-memory prefetch, and nothing more
		BufferedTracks:  11,
		BufferedSeconds: 37 * 60,
		PlannedTracks:   1,
		PlannedSeconds:  200,
		StoreLevel:      9,
		StoreTarget:     72,
		NextBatchIn:     45 * time.Minute,
	}

	ready := bufferReady(st)
	if !strings.Contains(ready, "11 song(s) ready") || !strings.Contains(ready, "37m") {
		t.Errorf("ready line reads %q", ready)
	}
	if strings.Contains(ready, "2 track") {
		t.Errorf("ready line still reports the prefetch: %q", ready)
	}

	frac, _, text, ok := bufferGauge(st)
	if !ok {
		t.Fatal("phased status produced no gauge")
	}
	// Nine untaken songs of a store that fills to 72.
	if frac < 0.12 || frac > 0.13 {
		t.Errorf("gauge fraction %.3f, want about 0.125", frac)
	}
	for _, want := range []string{"11 songs to play (37m)", "next batch in 45m"} {
		if !strings.Contains(text, want) {
			t.Errorf("gauge text %q is missing %q", text, want)
		}
	}
	// A full store says so, and a due batch says how big it is.
	st.StoreLevel = 72
	if _, _, text, _ = bufferGauge(st); !strings.Contains(text, "store full") {
		t.Errorf("a full store reads %q", text)
	}
	st.StoreLevel, st.NextBatchIn, st.RampBatch = 9, 0, 20
	if _, _, text, _ = bufferGauge(st); !strings.Contains(text, "next batch of 20 due") {
		t.Errorf("a due batch reads %q", text)
	}
}

func TestBufferPhasedWithNothingYet(t *testing.T) {
	st := player.Status{StoreTarget: 72}
	if got := bufferReady(st); got != "nothing buffered yet" {
		t.Errorf("ready line reads %q", got)
	}
	// Plans written but nothing rendered is a real, distinct state:
	// the planner is ahead and the renderer has not caught up.
	st.PlannedTracks = 4
	if got := bufferReady(st); !strings.Contains(got, "4 planned") {
		t.Errorf("ready line reads %q", got)
	}
}

func TestFmtSpan(t *testing.T) {
	for _, tc := range []struct {
		in   float64
		want string
	}{
		{0, "0m"},
		{45, "45s"},
		{200, "3m"},
		{45 * 60, "45m"},
		{2 * 60 * 60, "2h00m"},
		{6*60*60 + 7*60, "6h07m"},
	} {
		if got := fmtSpan(tc.in); got != tc.want {
			t.Errorf("fmtSpan(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The generation row flips several times a minute as the cycle moves
// between plan and render jobs. It is drawn in both states so the rows
// under it hold still; these are the words it uses when idle.
func TestGenIdleText(t *testing.T) {
	for _, tc := range []struct {
		name string
		st   player.Status
		want string
	}{
		{"plain idle", player.Status{EngineName: "acestep", EngineReady: true}, "idle"},
		{"hibernated", player.Status{EngineName: "acestep"}, "idle · engine asleep"},
		{"exporting", player.Status{EngineName: "acestep", EngineReady: true, Exporting: "10 min"},
			"idle · the engine is busy with an export"},
		// An export outranks the sleep note: it explains the engine
		// better than "asleep" does, and it is the temporary state.
		{"exporting while asleep", player.Status{Exporting: "10 min"},
			"idle · the engine is busy with an export"},
		{"no engine", player.Status{}, "idle"},
	} {
		if got := genIdle(tc.st); got != tc.want {
			t.Errorf("%s: genIdle = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A cycle that stops early - the writer ran out of words, so the batch
// rendered 11 of an intended 20 and handed the card back - is finished
// business, not a stall. Beside an idle engine the batch's own count
// reads as one, so the line says what is banked and what starts the
// next batch instead.
func TestBufferLineBetweenBatches(t *testing.T) {
	st := player.Status{
		RampBatch:       20,
		BatchRendered:   11,
		BufferedTracks:  17,
		BufferedSeconds: 51 * 60,
		StoreLevel:      15,
		StoreTarget:     72,
		NextBatchIn:     45 * time.Minute,
	}

	// Idle: the batch count is gone from the words.
	_, _, text, ok := bufferGauge(st)
	if !ok {
		t.Fatal("no gauge for an idle phased buffer")
	}
	if strings.Contains(text, "of 20") {
		t.Errorf("an idle engine still advertises an unfinished batch: %q", text)
	}
	for _, want := range []string{"17 songs to play", "51m", "next batch in 45m"} {
		if !strings.Contains(text, want) {
			t.Errorf("idle line %q is missing %q", text, want)
		}
	}

	// Generating: the batch is live again and its progress is the news.
	st.Generating = true
	_, _, text, ok = bufferGauge(st)
	if !ok {
		t.Fatal("no gauge while generating")
	}
	if !strings.Contains(text, "11 of 20 songs rendered") || !strings.Contains(text, "17 to play") {
		t.Errorf("working line reads %q", text)
	}
}
