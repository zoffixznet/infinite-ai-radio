package player

import "testing"

// runBurst drives the scheduler the way runCycle does, recording the
// shape of one cycle: 'P' for a plan, 'R' for a render. Each plan adds
// one stored plan; each render turns the oldest into trackSeconds of
// audio. It stops at stepDone or the step budget.
func runBurst(s cycleState, trackSeconds float64, budget int) string {
	out := make([]byte, 0, budget)
	for i := 0; i < budget; i++ {
		switch nextCycleStep(s) {
		case stepPlan:
			s.planned++
			s.plannedSecs += trackSeconds
			s.plannedThisCycle++
			out = append(out, 'P')
		case stepRender:
			s.planned--
			s.plannedSecs -= trackSeconds
			s.buffered += trackSeconds
			out = append(out, 'R')
		default:
			return string(out)
		}
	}
	return string(out)
}

// The regression this whole split exists for: the refill trigger
// (render_low_minutes, 45 minutes by default) was being used as the
// preemption threshold, so a cycle could never get two plans in a row
// until three quarters of an hour of audio had piled up. Every song
// paid a model swap on both sides.
func TestCyclePlansInBatchesOncePlaybackIsSafe(t *testing.T) {
	const track = 200.0 // a typical song, in seconds
	base := cycleState{
		renderTarget: 2 * 60 * 60, // 2h of rendered audio
		starve:       8 * 60,      // 8m is the danger line
		batchCap:     10,
	}

	// Playback already has comfortable runway: the cycle should write
	// its whole batch of plans before rendering any of them.
	safe := base
	safe.buffered = 30 * 60
	got := runBurst(safe, track, 40)
	if want := "PPPPPPPPPP" + "RRRRRRRRRR"; got != want {
		t.Errorf("with a healthy buffer the cycle ran %q, want %q", got, want)
	}

	// The old behaviour, for contrast: had starve been the 45-minute
	// refill trigger, the same state would alternate one for one.
	old := safe
	old.starve = 45 * 60
	if got := runBurst(old, track, 8); got != "PRPRPRPR" {
		t.Errorf("sanity check on the old shape got %q, want PRPRPRPR", got)
	}
}

func TestCycleRendersFirstWhenPlaybackIsAboutToRunDry(t *testing.T) {
	const track = 200.0
	s := cycleState{
		buffered:     0, // nothing secured at all
		renderTarget: 2 * 60 * 60,
		starve:       8 * 60,
		batchCap:     10,
	}
	got := runBurst(s, track, 12)
	// The first step must be a plan (there is nothing to render yet),
	// and every plan while below the danger line is rendered at once.
	if got[:6] != "PRPRPR" {
		t.Errorf("a starved buffer ran %q, want it to alternate until safe", got)
	}
	// Once past the danger line it must settle into batching.
	if got[6:] == "PRPRPR" {
		t.Errorf("the cycle never stopped alternating: %q", got)
	}
}

// Stage 0 of the ramp: a fresh steering context puts exactly one song
// through both phases so music starts as fast as the fused path.
func TestCycleFirstSongOfAContextGoesThroughAlone(t *testing.T) {
	s := cycleState{renderTarget: 2 * 60 * 60, starve: 8 * 60, batchCap: 1}
	if got := runBurst(s, 200, 10); got != "PR" {
		t.Errorf("stage-0 cycle ran %q, want PR", got)
	}
}

// At full depth there is no batch cap; planning runs to the plan target
// and rendering to the render target.
func TestCycleAtFullDepthPlansToItsTarget(t *testing.T) {
	const track = 200.0
	s := cycleState{
		buffered:     30 * 60,
		planTarget:   60 * 60, // 1h of plans
		renderTarget: 40 * 60,
		starve:       8 * 60,
		batchCap:     0,
	}
	got := runBurst(s, track, 60)
	if got == "" {
		t.Fatal("full-depth cycle did nothing")
	}
	plans, renders := 0, 0
	for _, c := range got {
		if c == 'P' {
			plans++
		} else {
			renders++
		}
	}
	// It plans until plannedSecs+buffered reaches an hour: 30 minutes
	// are already buffered, so about 30 minutes of plans, then renders
	// until the buffer reaches the 40-minute render target.
	if plans < 8 || plans > 11 {
		t.Errorf("planned %d songs for the remaining half hour (%q)", plans, got)
	}
	if renders < 2 || renders > 4 {
		t.Errorf("rendered %d songs to reach the 40m target (%q)", renders, got)
	}
	// And it must finish rather than spin.
	if len(got) >= 60 {
		t.Errorf("cycle never reached stepDone: %q", got)
	}
}

func TestCycleStopsWhenEveryTargetIsMet(t *testing.T) {
	s := cycleState{
		planned:      3,
		plannedSecs:  600,
		buffered:     3 * 60 * 60, // well past the render target
		planTarget:   60 * 60,
		renderTarget: 2 * 60 * 60,
		starve:       8 * 60,
		batchCap:     0,
	}
	if got := nextCycleStep(s); got != stepDone {
		t.Errorf("nextCycleStep = %v, want stepDone", got)
	}
}
