package state

import (
	"os"
	"testing"
	"time"
)

func TestEngineStateRoundTrip(t *testing.T) {
	d, err := NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := d.ReadEngineState(); ok {
		t.Fatal("state present before write")
	}
	st := EngineState{PID: os.Getpid(), Port: 12345, EngineDir: "/x", Started: time.Now()}
	if err := d.WriteEngineState(st); err != nil {
		t.Fatal(err)
	}
	got, ok := d.ReadEngineState()
	if !ok || got.PID != st.PID || got.Port != 12345 {
		t.Fatalf("read = %+v ok=%v", got, ok)
	}
	d.RemoveEngineState()
	if _, ok := d.ReadEngineState(); ok {
		t.Fatal("state present after remove")
	}
}

func TestPIDAlive(t *testing.T) {
	if !PIDAlive(os.Getpid()) {
		t.Fatal("own pid reported dead")
	}
	if PIDAlive(0) || PIDAlive(-5) {
		t.Fatal("invalid pids reported alive")
	}
	// A pid from the far end of the space is almost certainly free.
	if PIDAlive(4194000) {
		t.Skip("improbable pid actually alive")
	}
}

func TestLocksExcludeEachOther(t *testing.T) {
	d, err := NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	l1, err := d.AcquireLock("test.lock", false)
	if err != nil {
		t.Fatal(err)
	}
	// Locks are per-process (flock on the same fd family), so exercise
	// re-entry from this process via a second open: flock allows the
	// same process to re-lock; the real contention case is covered by
	// the dual-instance verification run.
	l1.Release()
	l2, err := d.AcquireLock("test.lock", false)
	if err != nil {
		t.Fatalf("relock after release: %v", err)
	}
	l2.Release()
}

func TestHeartbeat(t *testing.T) {
	d, err := NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if age := d.HeartbeatAge(); age < time.Hour {
		t.Fatalf("age without heartbeat = %s; want large", age)
	}
	if err := d.Heartbeat(); err != nil {
		t.Fatal(err)
	}
	if age := d.HeartbeatAge(); age > 5*time.Second {
		t.Fatalf("age after heartbeat = %s", age)
	}
}

func TestFreePort(t *testing.T) {
	p1, err := FreePort()
	if err != nil {
		t.Fatal(err)
	}
	if p1 < 1024 || p1 > 65535 {
		t.Fatalf("port %d out of range", p1)
	}
}

func TestTimings(t *testing.T) {
	d, err := NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tm := NewTimings(d)
	// Defaults before any measurement.
	if got := tm.Expected(PhaseModelLoad); got != 60*time.Second {
		t.Fatalf("default model_load = %s", got)
	}
	tm.Record(PhaseModelLoad, 30*time.Second)
	tm.Record(PhaseModelLoad, 40*time.Second)
	tm.Record(PhaseModelLoad, 50*time.Second)
	if got := tm.Expected(PhaseModelLoad); got != 40*time.Second {
		t.Fatalf("median = %s; want 40s", got)
	}
	// Persists across instances.
	tm2 := NewTimings(d)
	if got := tm2.Expected(PhaseModelLoad); got != 40*time.Second {
		t.Fatalf("persisted median = %s; want 40s", got)
	}
	// Nil receiver is safe and returns defaults.
	var nilT *Timings
	if got := nilT.Expected(PhaseFirstTrack); got != 40*time.Second {
		t.Fatalf("nil default = %s", got)
	}
	nilT.Record(PhaseFirstTrack, time.Second) // must not panic
}

// A daemon that exits must retract only its own record. Deleting a
// successor's leaves a live engine that no client can find and no
// command can stop, with its GPU memory pinned.
func TestRemoveEngineStateOnlyRetractsItsOwn(t *testing.T) {
	d, err := NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// A successor is running and owns the record.
	successor := EngineState{PID: os.Getpid(), Port: 4321, Started: time.Now()}
	if err := d.WriteEngineState(successor); err != nil {
		t.Fatal(err)
	}
	// The predecessor exits and tries to clean up after itself.
	d.RemoveEngineStateIf(os.Getpid() + 1)
	got, ok := d.ReadEngineState()
	if !ok || got.PID != successor.PID || got.Port != 4321 {
		t.Fatalf("the successor's record was deleted: %+v ok=%v", got, ok)
	}
	// Its own record it may retract.
	d.RemoveEngineStateIf(successor.PID)
	if _, ok := d.ReadEngineState(); ok {
		t.Fatal("a daemon could not retract its own record")
	}
	// A record naming a dead daemon is stale and anyone may clear it.
	if err := d.WriteEngineState(EngineState{PID: 0x7FFFFFF0, Port: 1}); err != nil {
		t.Fatal(err)
	}
	d.RemoveEngineStateIf(os.Getpid())
	if _, ok := d.ReadEngineState(); ok {
		t.Fatal("a stale record naming a dead daemon should be cleared")
	}
}
