package player

import "testing"

// runBurst drives the scheduler the way runCycle does, recording the
// shape of one cycle: 'P' for a plan, 'R' for a render. Each plan adds
// one stored plan; each render consumes the oldest. It stops at
// stepDone or the step budget.
func runBurst(s cycleState, budget int) string {
	out := make([]byte, 0, budget)
	for i := 0; i < budget; i++ {
		switch nextCycleStep(s) {
		case stepPlan:
			s.planned++
			s.plannedThisCycle++
			out = append(out, 'P')
		case stepRender:
			s.planned--
			out = append(out, 'R')
		default:
			return string(out)
		}
	}
	return string(out)
}

// A cycle plans its whole batch, then renders all of it: the models
// swap once per cycle, never once per song. Nothing preempts the plan
// burst, because no player draws straight from the generator.
func TestCyclePlansTheBatchThenRendersIt(t *testing.T) {
	got := runBurst(cycleState{batchCap: 10}, 40)
	if want := "PPPPPPPPPP" + "RRRRRRRRRR"; got != want {
		t.Errorf("a ten-song batch ran %q, want %q", got, want)
	}
}

// The opener: one song through both phases, as fast as possible.
func TestCycleOpenerGoesThroughAlone(t *testing.T) {
	if got := runBurst(cycleState{batchCap: 1}, 10); got != "PR" {
		t.Errorf("the opener's cycle ran %q, want PR", got)
	}
}

// Plans an interrupted cycle left behind get their audio even while
// the tap is waiting - and nothing new is planned then.
func TestCycleRendersLeftoverPlansWithoutPlanningMore(t *testing.T) {
	if got := runBurst(cycleState{planned: 3, batchCap: 0}, 10); got != "RRR" {
		t.Errorf("a render-only cycle ran %q, want RRR", got)
	}
}

func TestCycleStopsWhenNothingIsDue(t *testing.T) {
	if got := nextCycleStep(cycleState{}); got != stepDone {
		t.Errorf("nextCycleStep = %v, want stepDone", got)
	}
}
