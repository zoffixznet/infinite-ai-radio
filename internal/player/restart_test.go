package player

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"iar/internal/engine"
	"iar/internal/engine/enginetest"
	"iar/internal/library"
	"iar/internal/prompting"
	"iar/internal/session"
	"iar/internal/trackbuffer"
)

// Once the batch ladder has climbed to eighty-song batches, the only
// way to hear a fresh run of songs under the same settings used to be a
// detour through another preset. Restarting generation throws away
// everything made ahead - the prefetch, the rendered songs, the plans -
// and puts the ladder back on its first rung, with the session and its
// steering untouched.
func TestRestartGenerationEmptiesTheBufferAndStartsTheLadderOver(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	cfg := testConfig()
	cfg.Buffer.Phased = true
	sess := session.New()
	o := New(cfg, &phasedMock{}, prompting.NewBuilder(nil, testLogger()),
		session.NewStore(t.TempDir()), sess, &capturePlayer{}, testLogger())
	o.Buffer = trackbuffer.New(t.TempDir(), 9, testLogger())
	o.Buffer.SetContext(library.Key(sess))

	// An evening of settled listening: songs and plans on disk, two
	// songs staged in the prefetch, one of them looping on request, and
	// the ladder at the top.
	for seq := 1; seq <= 3; seq++ {
		track := &engine.Track{Lyrics: fmt.Sprintf("[Verse]\nsong %d", seq), Samples: make([]int16, 9600)}
		if err := o.Buffer.PutTrack(context.Background(), 0, seq, track); err != nil {
			t.Fatal(err)
		}
	}
	for seq := 4; seq <= 5; seq++ {
		plan := &engine.Plan{Caption: "planned", Lyrics: "[Verse]\nwords", Seconds: 90}
		if err := o.Buffer.PutPlan(0, seq, plan); err != nil {
			t.Fatal(err)
		}
	}
	o.mu.Lock()
	o.queue = []*engine.Track{mkTrack("staged one", 2), mkTrack("staged two", 2)}
	o.lastGood, o.lastGoodEpoch = o.queue[1], o.epoch
	o.loopOn, o.loopEpoch = true, o.epoch
	o.playedInEpoch, o.properPlayedInEpoch = 56, 55
	o.phasedEpoch, o.phasedSynced = o.epoch, true
	o.mu.Unlock()
	if _, _, batch := o.cycleTargets(0); batch != 80 {
		t.Fatalf("batch before the restart = %d; the test needs the ladder at the top", batch)
	}
	desc := sess.Describe()

	ack := o.RestartGeneration()
	for _, want := range []string{"5 song(s)", "2 plan(s)", "generating again from the top"} {
		if !strings.Contains(ack, want) {
			t.Errorf("ack %q does not say %q", ack, want)
		}
	}

	o.mu.Lock()
	epoch, queued, last := o.epoch, len(o.queue), o.lastGood
	stale, pending, looping := o.lastGoodStaleLocked(), o.steerPending, o.loopOn
	sameSession := o.sess == sess && o.sess.Describe() == desc
	o.mu.Unlock()
	if epoch != 1 {
		t.Errorf("epoch = %d, want 1", epoch)
	}
	if queued != 0 {
		t.Errorf("the prefetch survived: %d queued", queued)
	}
	// The last good track is deliberately kept: it is what the speakers
	// fall back to while the first fresh song generates, and dropping
	// it would answer the button with dead air.
	if last == nil || !stale {
		t.Errorf("lastGood %v, stale %v; want the previous sound kept and marked as previous", last, stale)
	}
	if !pending || looping {
		t.Errorf("steerPending=%v loopOn=%v; want true, false", pending, looping)
	}
	if !sameSession {
		t.Error("the session changed; a restart keeps it exactly as it was")
	}
	if !sess.LastPlayed.IsZero() {
		t.Error("the session was saved; a restart changes nothing worth saving")
	}
	if n, _ := o.Buffer.TrackStats(0); n != 0 {
		t.Errorf("%d rendered songs survived on disk", n)
	}
	if n, _ := o.Buffer.PlanStats(0); n != 0 {
		t.Errorf("%d plans survived on disk", n)
	}

	// What the listener actually asked for: the next cycle plans one
	// quick song, not eighty.
	if synced := o.syncPhasedState(); synced != epoch {
		t.Fatalf("the loops reconcile against epoch %d, want %d", synced, epoch)
	}
	if _, _, batch := o.cycleTargets(epoch); batch != 1 {
		t.Errorf("batch after the restart = %d, want 1", batch)
	}
}

// A restart on a held radio empties the buffer all the same, but says
// that generation waits for the wake instead of promising music that is
// not coming. In noise mode nothing is generated ahead, so there is
// nothing to start over and nothing is touched.
func TestRestartGenerationIsHonestWhenNothingWillCome(t *testing.T) {
	newRadio := func(t *testing.T, sess *session.Session) *Orchestrator {
		t.Helper()
		return New(testConfig(), enginetest.NewMock(), prompting.NewBuilder(nil, testLogger()),
			session.NewStore(t.TempDir()), sess, &capturePlayer{}, testLogger())
	}
	t.Run("held", func(t *testing.T) {
		o := newRadio(t, session.New())
		o.mu.Lock()
		o.standby = true
		o.queue = []*engine.Track{mkTrack("staged", 2)}
		o.mu.Unlock()
		ack := o.RestartGeneration()
		if !strings.Contains(ack, "1 song(s)") || !strings.Contains(ack, "standby") || strings.Contains(ack, "generating again") {
			t.Fatalf("held ack = %q", ack)
		}
		if o.Status().Queued != 0 {
			t.Fatal("the hold kept the staged song")
		}
	})
	t.Run("noise", func(t *testing.T) {
		sess := session.New()
		sess.Mode = session.ModeNoise
		o := newRadio(t, sess)
		before := o.Status().Epoch
		if ack := o.RestartGeneration(); !strings.Contains(ack, "noise mode") {
			t.Fatalf("noise ack = %q", ack)
		}
		if after := o.Status().Epoch; after != before {
			t.Fatalf("noise mode moved the epoch %d -> %d for nothing", before, after)
		}
	})
}
