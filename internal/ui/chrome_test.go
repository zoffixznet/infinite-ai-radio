package ui

import (
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"

	"iar/internal/player"
)

// chromeShape returns the height of the chrome and the width of its
// widest line, which together are what the eye notices moving. Width is
// measured as rendered columns, so the styles' escape sequences do not
// count towards it.
func chromeShape(st player.Status) (lines, width int) {
	m := tuiModel{status: st, styles: newStyles(), width: 78}
	out := m.renderChrome()
	for _, l := range strings.Split(out, "\n") {
		if n := lipgloss.Width(l); n > width {
			width = n
		}
	}
	return strings.Count(out, "\n"), width
}

// The generation row used to be drawn only while a job was in flight,
// and the panel repeated "· generating" in its border. Under phased
// generation both flip several times a minute as the cycle moves
// between plan and render jobs, so the whole screen shifted up, down
// and sideways continuously. The chrome must hold its shape.
func TestChromeDoesNotMoveWhileGenerating(t *testing.T) {
	base := player.Status{
		State: "playing", Source: "nu-metal", Session: "drive", Phase: "playing",
		Phased: true, EngineName: "acestep", EngineReady: true,
		BufferedTracks: 11, BufferedSeconds: 2000,
		BufferTargetSeconds: 7200, BufferLowSeconds: 2700, Volume: 80,
		Elapsed: 30 * time.Second, Duration: 200 * time.Second,
	}
	gen := base
	gen.Generating = true

	idleLines, idleWidth := chromeShape(base)
	genLines, genWidth := chromeShape(gen)
	if idleLines != genLines {
		t.Errorf("chrome is %d lines idle and %d generating", idleLines, genLines)
	}
	if idleWidth != genWidth {
		t.Errorf("chrome is %d columns idle and %d generating", idleWidth, genWidth)
	}

	// A long track title must not widen the panel either: titles change
	// every few minutes and the border would jump with them.
	long := base
	long.Source = "a very long track description that would otherwise stretch the panel border"
	longLines, longWidth := chromeShape(long)
	if longWidth != idleWidth {
		t.Errorf("a long title widened the chrome to %d columns, want %d", longWidth, idleWidth)
	}
	if longLines != idleLines {
		t.Errorf("a long title changed the chrome height to %d lines, want %d", longLines, idleLines)
	}
}

// Whatever the state, the generation row is present and says something.
func TestChromeAlwaysDrawsTheGenerationRow(t *testing.T) {
	for _, st := range []player.Status{
		{State: "playing", Phase: "playing"},
		{State: "playing", Phase: "playing", Generating: true},
		{State: "playing", Phase: "playing", Phased: true, EngineName: "acestep"},
		{State: "playing", Phase: "playing", Exporting: "10 min"},
	} {
		m := tuiModel{status: st, styles: newStyles(), width: 78}
		out := m.renderChrome()
		if !strings.Contains(out, "gen ") {
			t.Errorf("no generation row for %+v:\n%s", st, out)
		}
	}
}
