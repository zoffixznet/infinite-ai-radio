package trackbuffer

import (
	"context"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"iar/internal/audio"
	"iar/internal/engine"
)

func TestPlanLifecycle(t *testing.T) {
	s := New(t.TempDir(), 0, nil)
	p := &engine.Plan{Caption: "rock", Lyrics: "[Verse 1]\nwords", AudioCodes: "codes", Seconds: 200}
	if err := s.PutPlan(3, 1, p); err != nil {
		t.Fatal(err)
	}
	if err := s.PutPlan(3, 2, &engine.Plan{Caption: "second", Seconds: 100}); err != nil {
		t.Fatal(err)
	}
	count, secs := s.PlanStats(3)
	if count != 2 || secs != 300 {
		t.Fatalf("stats = %d, %v", count, secs)
	}
	got, seq, ok := s.NextPlan(3)
	if !ok || seq != 1 || got.Caption != "rock" || got.AudioCodes != "codes" {
		t.Fatalf("NextPlan = %+v seq=%d ok=%v", got, seq, ok)
	}
	s.DropPlan(3, 1)
	if got, seq, _ := s.NextPlan(3); seq != 2 || got.Caption != "second" {
		t.Fatalf("after drop: %+v seq=%d", got, seq)
	}
	if s.MaxSeq(3) != 2 {
		t.Fatalf("MaxSeq = %d", s.MaxSeq(3))
	}
	if n := s.DropOtherEpochs(4); n != 1 {
		t.Fatalf("dropped %d; want the stale epoch-3 plan", n)
	}
	if _, _, ok := s.NextPlan(3); ok {
		t.Fatal("epoch 3 plan survived the epoch change")
	}
}

func TestTrackRoundTrip(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	s := New(t.TempDir(), 0, nil)
	samples := make([]int16, 2*audio.SampleRate*audio.Channels)
	for i := 0; i < len(samples); i += 2 {
		v := int16(8000 * math.Sin(2*math.Pi*440*float64(i/2)/audio.SampleRate))
		samples[i], samples[i+1] = v, v
	}
	track := &engine.Track{
		Samples: samples,
		Prompt:  "test tone",
		Lyrics:  "[Instrumental]",
		Seed:    "42",
		Spec:    engine.Spec{VocalLanguage: "ru"},
	}
	if err := s.PutTrack(context.Background(), 1, 7, "song:1-7", track); err != nil {
		t.Fatal(err)
	}
	count, secs := s.TrackStats(1)
	if count != 1 || secs < 1.9 || secs > 2.1 {
		t.Fatalf("stats = %d, %v", count, secs)
	}
	got, titleKey, ok := s.NextTrack(context.Background(), 1)
	if !ok || titleKey != "song:1-7" {
		t.Fatal("NextTrack found nothing")
	}
	if got.Prompt != "test tone" || got.Seed != "42" || got.Spec.VocalLanguage != "ru" {
		t.Fatalf("meta lost: %+v", got)
	}
	// MP3 is lossy; length within a frame or two of the original.
	if d := math.Abs(float64(len(got.Samples)-len(samples))) / float64(audio.SampleRate*audio.Channels); d > 0.2 {
		t.Fatalf("length drifted %vs", d)
	}
	if _, _, ok := s.NextTrack(context.Background(), 1); ok {
		t.Fatal("track not consumed")
	}
}

// Songs rendered by a path that has since been fixed must not keep
// playing: the render-version marker drops the audio on the next start
// while keeping the plans, which the renderer reads rather than writes.
func TestRenderVersionDropsSongsButKeepsPlans(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	dir := t.TempDir()
	s := New(dir, 9, nil)
	if got := s.RenderVersionOf(); got != 0 {
		t.Fatalf("a fresh buffer reports version %d, want 0", got)
	}
	if err := s.PutPlan(1, 1, &engine.Plan{Caption: "keep me", Seconds: 3}); err != nil {
		t.Fatal(err)
	}
	track := &engine.Track{Samples: make([]int16, audio.SampleRate*audio.Channels)}
	if err := s.PutTrack(context.Background(), 1, 2, "k", track); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.TrackStats(1); n != 1 {
		t.Fatalf("setup: %d songs stored, want 1", n)
	}

	if dropped := s.DropTracks(); dropped != 1 {
		t.Errorf("DropTracks removed %d songs, want 1", dropped)
	}
	if n, _ := s.TrackStats(1); n != 0 {
		t.Errorf("%d songs survived the drop", n)
	}
	if n, _ := s.PlanStats(1); n != 1 {
		t.Errorf("%d plans survived the drop, want 1 - plans are still good", n)
	}
	// The audio file must go with its sidecar, not linger as garbage.
	entries, err := os.ReadDir(filepath.Join(dir, "tracks"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("tracks directory still holds %d file(s)", len(entries))
	}

	s.SetRenderVersion(RenderVersion)
	if got := s.RenderVersionOf(); got != RenderVersion {
		t.Errorf("RenderVersionOf = %d, want %d", got, RenderVersion)
	}
}

// A crash mid-encode leaves a ".part"; a crash between writing a song's
// audio and its sidecar leaves an MP3 no listing will ever look at.
// Neither parses as a buffer name, so the epoch and context sweeps skip
// them and they stay on disk for good.
func TestSweepRemovesCrashDebris(t *testing.T) {
	dir := t.TempDir()
	s := New(dir, 9, nil)
	tracks := filepath.Join(dir, "tracks")
	plans := filepath.Join(dir, "plans")
	for _, d := range []string{tracks, plans} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path string, n int) {
		t.Helper()
		if err := os.WriteFile(path, make([]byte, n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeJSON := func(path, body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A complete song: both halves, must survive.
	write(filepath.Join(tracks, "e00000001-00000001.mp3"), 100)
	writeJSON(filepath.Join(tracks, "e00000001-00000001.json"), `{"seconds":200}`)
	// A plan, must survive.
	writeJSON(filepath.Join(plans, "e00000001-00000009.json"), `{"plan":{"Seconds":200}}`)
	// Debris.
	write(filepath.Join(tracks, ".e00000001-00000002.mp3.part"), 7_000_000)
	write(filepath.Join(tracks, "e00000001-00000003.json.tmp"), 20)
	write(filepath.Join(tracks, "e00000001-00000004.mp3"), 6_000_000)            // no sidecar
	writeJSON(filepath.Join(tracks, "e00000001-00000005.json"), `{"seconds":9}`) // no audio

	dropped, freed := s.Sweep()
	if dropped != 4 {
		t.Errorf("swept %d files, want 4", dropped)
	}
	if freed < 13_000_000 {
		t.Errorf("freed %d bytes, want at least 13 MB", freed)
	}
	for _, keep := range []string{
		filepath.Join(tracks, "e00000001-00000001.mp3"),
		filepath.Join(tracks, "e00000001-00000001.json"),
		filepath.Join(plans, "e00000001-00000009.json"),
	} {
		if _, err := os.Stat(keep); err != nil {
			t.Errorf("sweep removed a good file: %s", keep)
		}
	}
	if n, _ := s.TrackStats(1); n != 1 {
		t.Errorf("%d songs after the sweep, want 1", n)
	}
	// Sweeping a clean buffer must do nothing.
	if dropped, _ := s.Sweep(); dropped != 0 {
		t.Errorf("second sweep removed %d files, want 0", dropped)
	}
}

// The broken-era backlog: dozens of plans and songs singing one
// identical sheet. The sweep keeps at most two copies of any sheet and
// leaves instrumentals alone.
func TestDedupeSheetsSweepsTheMonoculture(t *testing.T) {
	s := New(t.TempDir(), 0, nil)
	same := "[Verse]\nsteel in the water"
	for seq := 1; seq <= 5; seq++ {
		track := &engine.Track{Lyrics: same, Samples: make([]int16, 9600)}
		if err := s.PutTrack(context.Background(), 0, seq, "k", track); err != nil {
			t.Fatal(err)
		}
	}
	inst := &engine.Track{Lyrics: engine.InstrumentalLyrics, Samples: make([]int16, 9600)}
	if err := s.PutTrack(context.Background(), 0, 6, "", inst); err != nil {
		t.Fatal(err)
	}
	other := &engine.Track{Lyrics: "[Verse]\ndifferent words", Samples: make([]int16, 9600)}
	if err := s.PutTrack(context.Background(), 0, 7, "k2", other); err != nil {
		t.Fatal(err)
	}
	for seq := 8; seq <= 10; seq++ {
		if err := s.PutPlan(0, seq, &engine.Plan{Lyrics: same, AudioCodes: "c"}); err != nil {
			t.Fatal(err)
		}
	}

	removed := s.DedupeSheets(2)
	// Plans swept first (2 kept of 3 -> 1 removed)... the shared cap
	// spans plans and tracks: 8 copies of one sheet total, 2 survive.
	if removed != 6 {
		t.Fatalf("removed %d, want 6", removed)
	}
	var sameLeft, instLeft, otherLeft int
	for _, e := range s.List(0) {
		switch e.Lyrics {
		case same:
			sameLeft++
		case engine.InstrumentalLyrics:
			instLeft++
		default:
			otherLeft++
		}
	}
	if instLeft != 1 || otherLeft != 1 {
		t.Fatalf("sweep touched the wrong entries: inst=%d other=%d", instLeft, otherLeft)
	}
	if sameLeft > 2 {
		t.Fatalf("monoculture survived: %d tracks of one sheet", sameLeft)
	}
}
