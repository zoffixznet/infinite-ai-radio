package acestep

import (
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"iar/internal/state"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// spawnStandin starts a long-lived process to stand in for a daemon or
// a second client, and reaps it as soon as it dies. Without the reap a
// signalled child lingers as a zombie, which still answers a liveness
// probe - an artefact of the test being the process's parent, which
// the real player never is.
func spawnStandin(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		cmd.Process.Kill()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	})
	return cmd.Process.Pid
}

// The engine daemon is shared. A player finishing its own cycle used to
// stop it whatever else was going on, because the guard compared one
// shared heartbeat file against this client's own beat and so could
// never see anyone else. Running 'iar export' while the radio played
// died mid-song with "connection refused" when the radio hibernated.
func TestHibernateSparesADaemonAnotherClientIsUsing(t *testing.T) {
	dir, err := state.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Stand-ins for the daemon and for a second client, both alive.
	daemonPID := spawnStandin(t)
	otherPID := spawnStandin(t)

	if err := dir.WriteEngineState(state.EngineState{PID: daemonPID, Port: 1234, Started: time.Now()}); err != nil {
		t.Fatal(err)
	}
	r := NewRemote(dir, "iar", filepath.Join(t.TempDir(), "d.log"), quietLogger())
	r.st = state.EngineState{PID: daemonPID, Port: 1234}
	// This client has been beating all along, as a running player does.
	dir.Heartbeat()

	// Nobody else about: hibernating is this client's call to make.
	// (Checked before the other client beats, so the two cases differ
	// only by the other client's heartbeat.)
	if !r.Hibernate() {
		t.Fatal("a daemon with no other client was left running")
	}
	if state.PIDAlive(daemonPID) {
		t.Fatal("the daemon survived a hibernate that reported stopping it")
	}

	// Now with another live client mid-generation on the same daemon.
	survivorPID := spawnStandin(t)
	dir.WriteEngineState(state.EngineState{PID: survivorPID, Port: 1235, Started: time.Now()})
	r2 := NewRemote(dir, "iar", filepath.Join(t.TempDir(), "d.log"), quietLogger())
	r2.st = state.EngineState{PID: survivorPID, Port: 1235}
	dir.Heartbeat()
	beat := filepath.Join(dir.Path(), "heartbeats", strconv.Itoa(otherPID))
	if err := os.WriteFile(beat, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	if r2.Hibernate() {
		t.Fatal("hibernate stopped a daemon another client was using")
	}
	if !state.PIDAlive(survivorPID) {
		t.Fatal("the other client's daemon was killed anyway")
	}
}
