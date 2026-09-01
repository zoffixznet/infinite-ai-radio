package trackbuffer

import (
	"context"
	"math"
	"os/exec"
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
	if err := s.PutTrack(context.Background(), 1, 7, track); err != nil {
		t.Fatal(err)
	}
	count, secs := s.TrackStats(1)
	if count != 1 || secs < 1.9 || secs > 2.1 {
		t.Fatalf("stats = %d, %v", count, secs)
	}
	got, ok := s.NextTrack(context.Background(), 1)
	if !ok {
		t.Fatal("NextTrack found nothing")
	}
	if got.Prompt != "test tone" || got.Seed != "42" || got.Spec.VocalLanguage != "ru" {
		t.Fatalf("meta lost: %+v", got)
	}
	// MP3 is lossy; length within a frame or two of the original.
	if d := math.Abs(float64(len(got.Samples)-len(samples))) / float64(audio.SampleRate*audio.Channels); d > 0.2 {
		t.Fatalf("length drifted %vs", d)
	}
	if _, ok := s.NextTrack(context.Background(), 1); ok {
		t.Fatal("track not consumed")
	}
}
