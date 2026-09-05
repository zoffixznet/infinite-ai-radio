package player

import (
	"context"
	"strings"
	"testing"
	"time"

	"iar/internal/engine/enginetest"
	"iar/internal/prompting"
	"iar/internal/session"
	"iar/internal/state"
)

// The point of the hold is that the buffer is still there when the
// listener comes back: a held radio must take nothing out of it and
// put nothing into it. Mute was never enough - it silences the room
// while the mixer keeps eating songs and the card keeps making them.
func TestStandbyStopsConsumingAndGenerating(t *testing.T) {
	o, pl := newTestOrchestrator(t, enginetest.NewMock(), session.New())

	waitFor(t, 15*time.Second, "music playing before the hold", func() bool {
		return pl.written() > 0 && o.Status().TrackTitle != ""
	})

	if ack := o.ToggleStandby(); !strings.Contains(ack, "standby") {
		t.Fatalf("standby ack = %q", ack)
	}
	if !o.Status().Standby {
		t.Fatal("status does not report the hold")
	}
	if o.wantGeneration() {
		t.Fatal("a held radio still wants to generate")
	}

	// The song that was playing is still the song that is playing: the
	// mixer has not moved on, so nothing was spent.
	before := o.Status().TrackTitle
	time.Sleep(1500 * time.Millisecond)
	if after := o.Status().TrackTitle; after != before {
		t.Fatalf("the hold consumed the buffer: %q became %q", before, after)
	}

	// Output keeps flowing - silence, not a stalled device.
	fed := pl.written()
	time.Sleep(500 * time.Millisecond)
	if pl.written() <= fed {
		t.Fatal("the audio device stopped being fed while held")
	}

	if ack := o.ToggleStandby(); !strings.Contains(ack, "awake") {
		t.Fatalf("wake ack = %q", ack)
	}
	if o.Status().Standby {
		t.Fatal("status still reports the hold after waking")
	}
}

// A machine is left on for days; a restart must not quietly put it
// back to work.
func TestStandbySurvivesARestart(t *testing.T) {
	dir, err := state.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if dir.Standby() {
		t.Fatal("a fresh state directory reported a hold")
	}
	if err := dir.SetStandby(true); err != nil {
		t.Fatal(err)
	}
	if !dir.Standby() {
		t.Fatal("the hold was not recorded")
	}

	cfg := testConfig()
	o := New(cfg, enginetest.NewMock(), prompting.NewBuilder(nil, testLogger()),
		session.NewStore(t.TempDir()), session.New(), &capturePlayer{}, testLogger())
	o.StateDir = &dir
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	o.Start(ctx)
	t.Cleanup(func() { o.Close() })

	if !o.Status().Standby {
		t.Fatal("a radio started from a recorded hold began making music anyway")
	}
	o.ToggleStandby()
	if dir.Standby() {
		t.Fatal("waking did not clear the recorded hold")
	}
}

// written reports how many bytes have reached the audio device.
func (p *capturePlayer) written() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.buf)
}
