package ui

import (
	"strings"
	"testing"
	"time"

	"iar/internal/player"
	"iar/internal/telemetry"
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
		StoreSeconds:    31 * 60,
		WakeBelow:       93,
		StoreTarget:     172,
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
	// Nine untaken songs of a store that comes to hold 172.
	if frac < 0.05 || frac > 0.06 {
		t.Errorf("gauge fraction %.3f, want about 0.052", frac)
	}
	for _, want := range []string{"11 songs to play (37m)", "next batch in 45m"} {
		if !strings.Contains(text, want) {
			t.Errorf("gauge text %q is missing %q", text, want)
		}
	}
	// A stocked store says what wakes the engine - the level falling
	// under the mark - and never a countdown, because none runs; a
	// stale clock left over from the climb does not show either.
	st.StoreLevel, st.StoreSeconds = 151, 8*3600+50*60
	if _, _, text, _ = bufferGauge(st); !strings.Contains(text, "stocked, engine wakes below 93 in store") ||
		strings.Contains(text, "next batch") {
		t.Errorf("a stocked store reads %q", text)
	}
	// At the mark exactly the store is stocked too.
	st.StoreLevel, st.NextBatchIn = 93, 0
	if _, _, text, _ = bufferGauge(st); !strings.Contains(text, "wakes below 93") {
		t.Errorf("a store at the mark reads %q", text)
	}
	// A due batch says how big it is.
	st.StoreLevel, st.NextBatchIn, st.RampBatch = 92, 0, 80
	if _, _, text, _ = bufferGauge(st); !strings.Contains(text, "next batch of 80 due") {
		t.Errorf("a due batch reads %q", text)
	}
	// The bar never runs past full, however deep the store gets.
	st.StoreLevel = 200
	if frac, _, _, _ = bufferGauge(st); frac != 1 {
		t.Errorf("gauge fraction past the ceiling = %.3f, want 1", frac)
	}
}

func TestBufferPhasedWithNothingYet(t *testing.T) {
	st := player.Status{WakeBelow: 93, StoreTarget: 172}
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
		{"asleep", player.Status{EngineName: "acestep"}, "idle · engine asleep"},
		// A daemon the radio is bringing up for a batch is waking, not
		// asleep, however long its models take to load.
		{"waking for a batch", player.Status{EngineName: "acestep", EngineAwake: true},
			"waking the engine for the next batch"},
		{"waking for an export", player.Status{EngineName: "acestep", EngineAwake: true, Exporting: "10 min"},
			"idle · the engine is busy with an export"},
		{"exporting", player.Status{EngineName: "acestep", EngineReady: true, Exporting: "10 min"},
			"idle · the engine is busy with an export"},
		// An export outranks the sleep note: it explains the engine
		// better than "asleep" does, and it is the temporary state.
		{"exporting while asleep", player.Status{Exporting: "10 min"},
			"idle · the engine is busy with an export"},
		{"no engine", player.Status{}, "idle"},
		// Words being written is not idle, whichever way it is asked.
		{"writing", player.Status{EngineName: "acestep", Vocal: true, WordsmithWant: 10, WordsmithWrote: 4},
			"writing song words (4 of 10)"},
	} {
		if got := genIdle(tc.st); got != tc.want {
			t.Errorf("%s: genIdle = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// To the listener there is one engine and it makes songs. While the
// words are written the radio is making songs, and the row pulses and
// says so - with no word about which program has the card - and it
// says "asleep" only when nothing is being made at all.
func TestGenTextReadsWritingAsMakingSongs(t *testing.T) {
	writerBusy := &telemetry.Sample{Taken: time.Now(), WriterBusy: true}
	writerWithEngineUp := &telemetry.Sample{Taken: time.Now(), WriterBusy: true, EnginePID: 99}
	daemonUp := &telemetry.Sample{Taken: time.Now(), EnginePID: 99}
	for _, tc := range []struct {
		name string
		st   player.Status
		want string
		busy bool
	}{
		{"rendering", player.Status{EngineName: "acestep", EngineReady: true, Generating: true}, "generating next track", true},
		{"a wordsmith round", player.Status{EngineName: "acestep", Vocal: true, WordsmithWant: 10, WordsmithWrote: 4},
			"writing song words (4 of 10)", true},
		{"an instrumental round", player.Status{EngineName: "acestep", WordsmithWant: 10, WordsmithWrote: 4},
			"writing song descriptions (4 of 10)", true},
		// The writer's model lingering on the card after a round, with
		// the engine off it and nothing more to make: the round is
		// over, and the row does not go on saying the words are being
		// written on the strength of a process that is still resident.
		{"writer lingering, engine down", player.Status{EngineName: "acestep", Vocal: true, Telemetry: writerBusy},
			"idle · engine asleep", false},
		// With the daemon up, a lingering writer is not what the row
		// is about.
		{"writer lingering, engine up", player.Status{EngineName: "acestep", EngineReady: true, Vocal: true, Telemetry: writerWithEngineUp},
			"idle", false},
		// The daemon spawned for a batch and loading its models: the
		// batch is under way, and the row pulses with the status row
		// instead of calling the engine asleep beside "engine starting".
		{"engine waking for a batch", player.Status{EngineName: "acestep", EngineAwake: true, Phase: "loading models", Telemetry: daemonUp},
			"waking the engine for the next batch", true},
		{"engine waking, no telemetry", player.Status{EngineName: "acestep", EngineAwake: true, Phase: "starting engine"},
			"waking the engine for the next batch", true},
		{"asleep", player.Status{EngineName: "acestep", Telemetry: &telemetry.Sample{Taken: time.Now()}},
			"idle · engine asleep", false},
	} {
		got, busy := genText(tc.st)
		if got != tc.want || busy != tc.busy {
			t.Errorf("%s: genText = %q, %v; want %q, %v", tc.name, got, busy, tc.want, tc.busy)
		}
		if strings.Contains(got, "card") || strings.Contains(got, "hibernat") {
			t.Errorf("%s: the row talks components: %q", tc.name, got)
		}
		if busy && strings.Contains(got, "asleep") {
			t.Errorf("%s: the row is busy and says asleep: %q", tc.name, got)
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
		WakeBelow:       93,
		StoreTarget:     172,
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
