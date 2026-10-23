package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"iar/internal/state"
	"iar/internal/trackbuffer"
)

// A build that changes nothing about the songs keeps the store: the
// stamp becomes this build's, the songs stay, and the answer says
// what the stamp was. A running radio owns the store, so the command
// refuses while one holds the player lock.
func TestBufferKeepStampsTheStoreForThisBuild(t *testing.T) {
	dir := t.TempDir()
	stateD, err := state.NewDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	buf := trackbuffer.New(filepath.Join(dir, "buffer"), 2, log)
	buf.SetBuild("v9.8.7")

	was, songs, err := keepStore(stateD, buf, "v9.9.0")
	if err != nil || was != "v9.8.7" || songs != 0 {
		t.Fatalf("keep = %q, %d, %v", was, songs, err)
	}
	if got := buf.Build(); got != "v9.9.0" {
		t.Fatalf("the store is stamped %q, want v9.9.0", got)
	}

	// Stamping again is nothing: the store already says this build.
	if was, _, err := keepStore(stateD, buf, "v9.9.0"); err != nil || was != "v9.9.0" {
		t.Fatalf("repeat keep = %q, %v", was, err)
	}

	lock, err := stateD.AcquireLock("player.lock", false)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	if _, _, err := keepStore(stateD, buf, "v9.9.1"); err == nil || !strings.Contains(err.Error(), "running") {
		t.Fatalf("keep while the radio runs = %v, want a refusal", err)
	}
	if got := buf.Build(); got != "v9.9.0" {
		t.Fatalf("a refused keep changed the stamp to %q", got)
	}
}
