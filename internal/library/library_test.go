package library

import (
	"context"
	"io"
	"log/slog"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"iar/internal/audio"
	"iar/internal/engine"
	"iar/internal/session"
)

func testLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// The bank is MP3 now, so every put and pick runs the encoder.
func needFFmpeg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
}

// testQuality is the fastest libmp3lame setting: these tests are about
// what the library does with a file, not how good it sounds.
const testQuality = 9

func newLib(t *testing.T, dir string, maxMB int) *Library {
	t.Helper()
	return New(dir, maxMB, testQuality, testLog())
}

// track makes noise rather than a constant tone: a flat signal
// compresses to almost nothing, which would make every size-based
// assertion below meaningless.
func track(frames int, seed uint64) *engine.Track {
	r := rand.New(rand.NewPCG(seed, 0x9E3779B9))
	samples := make([]int16, frames*audio.Channels)
	for i := range samples {
		samples[i] = int16(r.IntN(20000) - 10000)
	}
	return &engine.Track{Samples: samples, Prompt: "test prompt", Lyrics: "[Instrumental]"}
}

func TestPutPickRoundTrip(t *testing.T) {
	needFFmpeg(t)
	lib := newLib(t, t.TempDir(), 100)
	const frames = audio.SampleRate // one second
	if _, err := lib.Put(context.Background(), "lofi-study", track(frames, 1)); err != nil {
		t.Fatal(err)
	}
	if got := lib.Count("lofi-study"); got != 1 {
		t.Fatalf("count = %d", got)
	}
	tr, _, ok := lib.Pick(context.Background(), "lofi-study")
	if !ok {
		t.Fatal("pick failed")
	}
	if !tr.FromLibrary {
		t.Fatal("picked track not marked FromLibrary")
	}
	// The sidecar is not compressed, so its contents survive exactly.
	if tr.Prompt != "test prompt" || tr.Lyrics != "[Instrumental]" {
		t.Fatalf("metadata lost: %+v", tr)
	}
	// The audio is lossy and the encoder pads its edges, so the honest
	// assertion is "about this long, and not silence" - not the exact
	// samples that went in.
	got := len(tr.Samples) / audio.Channels
	if got < frames*9/10 || got > frames*3/2 {
		t.Fatalf("round trip returned %d frames, want about %d", got, frames)
	}
	var loud int
	for _, s := range tr.Samples {
		if s > 1000 || s < -1000 {
			loud++
		}
	}
	if loud < len(tr.Samples)/10 {
		t.Fatalf("round trip came back silent: %d loud samples of %d", loud, len(tr.Samples))
	}
}

// The play time cannot be divided out of a compressed file, so it rides
// in the sidecar. Everything that lists the bank reads it from there.
func TestEntriesReportPlayTime(t *testing.T) {
	needFFmpeg(t)
	lib := newLib(t, t.TempDir(), 100)
	if _, err := lib.Put(context.Background(), "k", track(2*audio.SampleRate, 2)); err != nil {
		t.Fatal(err)
	}
	entries := lib.Entries("k")
	if len(entries) != 1 {
		t.Fatalf("got %d entries", len(entries))
	}
	if s := entries[0].Seconds; s < 1.9 || s > 2.1 {
		t.Fatalf("entry reports %.2f seconds, want about 2", s)
	}
}

// The WAV era is cleared out on the way past, so a cap that used to hold
// twenty songs is not still spent on them.
func TestUncompressedTracksAreCleared(t *testing.T) {
	dir := t.TempDir()
	keyDir := filepath.Join(dir, "old-vibe")
	if err := os.MkdirAll(keyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(keyDir, "20260101-000000-0001.wav"), make([]byte, 4096))
	writeFile(t, filepath.Join(keyDir, "20260101-000000-0001.json"), []byte(`{"prompt":"old"}`))
	// A sidecar whose audio never arrived goes too.
	writeFile(t, filepath.Join(keyDir, "20260101-000000-0002.json"), []byte(`{"prompt":"orphan"}`))
	// Anything already banked as MP3 stays, sidecar and all.
	writeFile(t, filepath.Join(keyDir, "20260101-000000-0003.mp3"), []byte("ID3fake"))
	writeFile(t, filepath.Join(keyDir, "20260101-000000-0003.json"), []byte(`{"prompt":"keep"}`))

	newLib(t, dir, 100)

	for _, gone := range []string{"0001.wav", "0001.json", "0002.json"} {
		matches, _ := filepath.Glob(filepath.Join(keyDir, "*"+gone))
		if len(matches) != 0 {
			t.Errorf("%s survived the sweep", gone)
		}
	}
	for _, kept := range []string{"0003.mp3", "0003.json"} {
		matches, _ := filepath.Glob(filepath.Join(keyDir, "*"+kept))
		if len(matches) != 1 {
			t.Errorf("%s was swept away", kept)
		}
	}
}

func TestPickMissingKey(t *testing.T) {
	lib := newLib(t, t.TempDir(), 100)
	if _, _, ok := lib.Pick(context.Background(), "nothing-here"); ok {
		t.Fatal("picked from empty library")
	}
}

// capAfter banks one track, then sets the cap to hold about n of them:
// how large a compressed second of noise comes out is the encoder's
// business, not something to hard-code here.
func capAfter(t *testing.T, lib *Library, key string, n int) {
	t.Helper()
	dir := filepath.Join(lib.dir, session.SanitizeName(key))
	ids := lib.ids(dir)
	if len(ids) == 0 {
		t.Fatal("nothing banked to measure")
	}
	fi, err := os.Stat(filepath.Join(dir, ids[0]+".mp3"))
	if err != nil {
		t.Fatal(err)
	}
	lib.maxBytes = fi.Size() * int64(n)
}

func TestEvictionKeepsUnderCap(t *testing.T) {
	needFFmpeg(t)
	lib := newLib(t, t.TempDir(), 100)
	ctx := context.Background()
	if _, err := lib.Put(ctx, "k", track(audio.SampleRate, 10)); err != nil {
		t.Fatal(err)
	}
	capAfter(t, lib, "k", 4)
	for i := 0; i < 10; i++ {
		if _, err := lib.Put(ctx, "k", track(audio.SampleRate, uint64(i+11))); err != nil {
			t.Fatal(err)
		}
	}
	if got := lib.Count("k"); got > 6 {
		t.Fatalf("eviction did not run: %d tracks for a four-track cap", got)
	}
	if got := lib.Count("k"); got == 0 {
		t.Fatal("eviction removed everything")
	}
}

func TestEvictionIsFairAcrossKeys(t *testing.T) {
	needFFmpeg(t)
	lib := newLib(t, t.TempDir(), 100)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if _, err := lib.Put(ctx, "b-vibe", track(audio.SampleRate/2, uint64(i+30))); err != nil {
			t.Fatal(err)
		}
	}
	capAfter(t, lib, "b-vibe", 6)
	for i := 0; i < 12; i++ {
		if _, err := lib.Put(ctx, "a-vibe", track(audio.SampleRate/2, uint64(i+40))); err != nil {
			t.Fatal(err)
		}
	}
	// A long session on one vibe must not wipe out another's instant
	// starts.
	if got := lib.Count("b-vibe"); got == 0 {
		t.Fatal("small key wiped out by a big key's session")
	}
	if got := lib.Count("a-vibe"); got == 0 {
		t.Fatal("big key lost everything")
	}
}

func TestDisabledLibraryIsNil(t *testing.T) {
	lib := newLib(t, t.TempDir(), 0)
	if lib != nil {
		t.Fatal("cap 0 should disable the library")
	}
	// Nil-safe methods.
	if _, err := lib.Put(context.Background(), "k", track(10, 1)); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := lib.Pick(context.Background(), "k"); ok {
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
	needFFmpeg(t)
	dir := t.TempDir()
	lib := newLib(t, dir, 100)
	if _, err := lib.Put(context.Background(), "k", track(audio.SampleRate/10, 50)); err != nil {
		t.Fatal(err)
	}
	ids := lib.ids(filepath.Join(dir, "k"))
	if len(ids) != 1 {
		t.Fatal("expected one track")
	}
	writeFile(t, filepath.Join(dir, "k", ids[0]+".mp3"), []byte("not an mp3"))
	if _, _, ok := lib.Pick(context.Background(), "k"); ok {
		t.Fatal("picked a corrupt track")
	}
	if got := lib.Count("k"); got != 0 {
		t.Fatalf("corrupt track not removed: count %d", got)
	}
}

// A shutdown cancels every decode; that must read as "not now", never as
// "this bank is corrupt, delete it".
func TestCancelledPickKeepsTheTrack(t *testing.T) {
	needFFmpeg(t)
	lib := newLib(t, t.TempDir(), 100)
	if _, err := lib.Put(context.Background(), "k", track(audio.SampleRate/10, 60)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, ok := lib.Pick(ctx, "k"); ok {
		t.Fatal("a cancelled pick returned a track")
	}
	if got := lib.Count("k"); got != 1 {
		t.Fatalf("a cancelled pick deleted the bank: count %d", got)
	}
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// A banked song keeps the id it had everywhere else, so it is still the
// same song: listed under it, handed back under it, and found by it from
// any vibe - which is how a listener saves a song the radio played long
// ago, even after a restart has forgotten where it was banked.
func TestABankedSongKeepsItsOwnID(t *testing.T) {
	needFFmpeg(t)
	lib := newLib(t, t.TempDir(), 100)
	ctx := context.Background()
	tr := track(audio.SampleRate/2, 70)
	tr.ID = "t-1790000000000-0007"
	if _, err := lib.Put(ctx, "some-vibe", tr); err != nil {
		t.Fatal(err)
	}
	if es := lib.Entries("some-vibe"); len(es) != 1 || es[0].TrackID != tr.ID {
		t.Fatalf("entries: %+v", es)
	}
	picked, _, ok := lib.Pick(ctx, "some-vibe")
	if !ok || picked.ID != tr.ID {
		t.Fatalf("picked id = %q ok=%v", picked.ID, ok)
	}
	found, ok := lib.Find(ctx, tr.ID)
	if !ok || found.ID != tr.ID || len(found.Samples) == 0 {
		t.Fatalf("found by id: ok=%v track=%+v", ok, found)
	}
	if _, ok := lib.Find(ctx, "t-never-banked"); ok {
		t.Fatal("found a song that was never banked")
	}
}
