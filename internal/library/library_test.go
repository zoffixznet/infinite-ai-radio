package library

import (
	"io"
	"log/slog"
	"os"
	"testing"

	"bgm/internal/engine"
	"bgm/internal/session"
)

func testLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func track(frames int, v int16) *engine.Track {
	samples := make([]int16, frames*2)
	for i := range samples {
		samples[i] = v
	}
	return &engine.Track{Samples: samples, Prompt: "test prompt", Lyrics: "[Instrumental]"}
}

func TestPutPickRoundTrip(t *testing.T) {
	lib := New(t.TempDir(), 100, testLog())
	if err := lib.Put("lofi-study", track(4800, 1234)); err != nil {
		t.Fatal(err)
	}
	if got := lib.Count("lofi-study"); got != 1 {
		t.Fatalf("count = %d", got)
	}
	tr, ok := lib.Pick("lofi-study")
	if !ok {
		t.Fatal("pick failed")
	}
	if !tr.FromLibrary {
		t.Fatal("picked track not marked FromLibrary")
	}
	if tr.Prompt != "test prompt" || tr.Lyrics != "[Instrumental]" {
		t.Fatalf("metadata lost: %+v", tr)
	}
	if len(tr.Samples) != 4800*2 || tr.Samples[0] != 1234 {
		t.Fatalf("samples wrong: len %d first %d", len(tr.Samples), tr.Samples[0])
	}
}

func TestPickMissingKey(t *testing.T) {
	lib := New(t.TempDir(), 100, testLog())
	if _, ok := lib.Pick("nothing-here"); ok {
		t.Fatal("picked from empty library")
	}
}

func TestEvictionKeepsUnderCap(t *testing.T) {
	// Each track: 48000 frames * 4 bytes = ~187KB. Cap at 1MB.
	lib := New(t.TempDir(), 1, testLog())
	for i := 0; i < 10; i++ {
		if err := lib.Put("k", track(48000, int16(i+1))); err != nil {
			t.Fatal(err)
		}
	}
	if got := lib.Count("k"); got > 6 {
		t.Fatalf("eviction did not run: %d tracks for ~1MB cap", got)
	}
	if got := lib.Count("k"); got == 0 {
		t.Fatal("eviction removed everything")
	}
}

func TestDisabledLibraryIsNil(t *testing.T) {
	lib := New(t.TempDir(), 0, testLog())
	if lib != nil {
		t.Fatal("cap 0 should disable the library")
	}
	// Nil-safe methods.
	if err := lib.Put("k", track(10, 1)); err != nil {
		t.Fatal(err)
	}
	if _, ok := lib.Pick("k"); ok {
		t.Fatal("nil library picked something")
	}
	if lib.Count("k") != 0 {
		t.Fatal("nil library counted something")
	}
}

func TestKeyDerivation(t *testing.T) {
	s := session.New()
	s.Preset = "sleep"
	if Key(s) != "sleep" {
		t.Fatalf("preset key = %q", Key(s))
	}
	s2 := session.New()
	s2.BasePrompt = "Dark Techno, driving!"
	if k := Key(s2); k != "dark-techno-driving" {
		t.Fatalf("prompt key = %q", k)
	}
	s3 := session.New()
	s3.BasePrompt = ""
	if k := Key(s3); k != "default" {
		t.Fatalf("empty key = %q", k)
	}
}

func TestCorruptTrackIsDroppedGracefully(t *testing.T) {
	dir := t.TempDir()
	lib := New(dir, 100, testLog())
	if err := lib.Put("k", track(100, 5)); err != nil {
		t.Fatal(err)
	}
	// Corrupt the wav.
	ids := lib.ids(dir + "/k")
	if len(ids) != 1 {
		t.Fatal("expected one track")
	}
	path := dir + "/k/" + ids[0] + ".wav"
	if err := writeFile(path, []byte("not a wav")); err != nil {
		t.Fatal(err)
	}
	if _, ok := lib.Pick("k"); ok {
		t.Fatal("picked a corrupt track")
	}
	if got := lib.Count("k"); got != 0 {
		t.Fatalf("corrupt track not removed: count %d", got)
	}
}

func writeFile(path string, data []byte) error {
	return os.WriteFile(path, data, 0o644)
}

func TestEvictionIsFairAcrossKeys(t *testing.T) {
	// Cap ~1MB; key A gets many tracks, key B two. A long session on A
	// must not wipe out B's instant-start tracks.
	lib := New(t.TempDir(), 1, testLog())
	for i := 0; i < 2; i++ {
		if err := lib.Put("b-vibe", track(24000, 7)); err != nil { // ~94KB each
			t.Fatal(err)
		}
	}
	for i := 0; i < 12; i++ {
		if err := lib.Put("a-vibe", track(24000, 9)); err != nil {
			t.Fatal(err)
		}
	}
	if got := lib.Count("b-vibe"); got == 0 {
		t.Fatal("small key wiped out by a big key's session")
	}
	if got := lib.Count("a-vibe"); got == 0 {
		t.Fatal("big key lost everything")
	}
}
