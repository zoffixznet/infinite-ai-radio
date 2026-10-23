package acestep

import (
	"context"
	"path/filepath"
	"testing"

	"iar/internal/state"
)

// A remote started dormant - the player's way, so a stocked store
// costs no engine process - is left alone by the health probe: no
// daemon is spawned, no state is written, and the phase reads asleep
// until a cycle or an export activates it.
func TestADormantRemoteIsNotSpawnedByTheProbe(t *testing.T) {
	dir, err := state.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := NewRemote(dir, filepath.Join(t.TempDir(), "no-such-iar"), filepath.Join(t.TempDir(), "d.log"), quietLogger())
	r.SetActive(false)

	for i := 0; i < 3; i++ {
		if got := r.probe(context.Background()); got != "hibernated" {
			t.Fatalf("probe %d of a dormant remote = %q, want hibernated", i, got)
		}
	}
	if _, ok := dir.ReadEngineState(); ok {
		t.Fatal("the probe spawned a daemon nobody asked for")
	}
	r.mu.Lock()
	client := r.client
	r.mu.Unlock()
	if client != nil {
		t.Fatal("the probe attached a client to a daemon nobody asked for")
	}
}
