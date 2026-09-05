package main

import (
	"errors"
	"os"
	"testing"

	"iar/internal/session"
	"iar/internal/state"
)

// A restart used to invent a new session every time, so the sound
// someone had settled on was replaced by the default the moment the
// machine came back. It resumes what was playing instead.
func TestRestartResumesTheSessionThatWasPlaying(t *testing.T) {
	dir := t.TempDir()
	stateD, err := state.NewDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	store := session.NewStore(t.TempDir())

	// Nothing recorded yet: a first run has nothing to resume, and that
	// is not an error.
	if s, err := resumeSession(store, stateD); s != nil || err != nil {
		t.Fatalf("first run resumed %+v (%v)", s, err)
	}

	sess := session.New()
	sess.Name = "road-trip"
	sess.BasePrompt = "desert rock, wide open, dusty"
	if err := store.Save(sess); err != nil {
		t.Fatal(err)
	}
	if err := stateD.WriteCurrentSession(state.CurrentSession{Name: sess.Name, PID: os.Getpid()}); err != nil {
		t.Fatal(err)
	}

	got, err := resumeSession(store, stateD)
	if err != nil || got == nil {
		t.Fatalf("resume = %+v, %v", got, err)
	}
	if got.Name != "road-trip" || got.BasePrompt != sess.BasePrompt {
		t.Fatalf("resumed the wrong session: %+v", got)
	}

	// A session deleted since it played is reported as gone rather than
	// stopping the radio from booting.
	if err := store.Delete("road-trip"); err != nil {
		t.Fatal(err)
	}
	got, err = resumeSession(store, stateD)
	if got != nil || !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("resume of a deleted session = %+v, %v", got, err)
	}
}
