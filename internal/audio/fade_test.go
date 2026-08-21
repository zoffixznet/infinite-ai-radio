package audio

import (
	"math"
	"testing"
)

func TestEqualPowerGains(t *testing.T) {
	for _, tc := range []float64{0, 0.25, 0.5, 0.75, 1} {
		out, in := EqualPowerGains(tc)
		if sum := out*out + in*in; math.Abs(sum-1) > 1e-9 {
			t.Fatalf("t=%v: out^2+in^2 = %v; want 1", tc, sum)
		}
	}
	out, in := EqualPowerGains(0)
	if out != 1 || in != 0 {
		t.Fatalf("t=0 gains = %v, %v; want 1, 0", out, in)
	}
	out, in = EqualPowerGains(1)
	if math.Abs(out) > 1e-9 || math.Abs(in-1) > 1e-9 {
		t.Fatalf("t=1 gains = %v, %v; want 0, 1", out, in)
	}
}

// constant returns frames of a constant stereo signal.
func constant(frames int, v int16) []int16 {
	out := make([]int16, frames*Channels)
	for i := range out {
		out[i] = v
	}
	return out
}

func TestCrossfadeJoinLengthAndContinuity(t *testing.T) {
	const trackFrames = 4800
	const fadeFrames = 480
	a := constant(trackFrames, 10000)
	b := constant(trackFrames, 10000)
	joined := CrossfadeJoin([][]int16{a, b}, fadeFrames)
	wantFrames := 2*trackFrames - fadeFrames
	if got := len(joined) / Channels; got != wantFrames {
		t.Fatalf("joined frames = %d; want %d", got, wantFrames)
	}
	// Equal-power blend of two identical constant signals must never dip
	// below the original level (it bulges slightly, up to sqrt(2)).
	for f := trackFrames - fadeFrames; f < trackFrames; f++ {
		v := joined[f*Channels]
		if v < 9999 {
			t.Fatalf("frame %d dips to %d during crossfade; equal-power should not dip", f, v)
		}
	}
	// No sample discontinuity at the fade boundaries.
	pre := joined[(trackFrames-fadeFrames-1)*Channels]
	first := joined[(trackFrames-fadeFrames)*Channels]
	if delta := int(first) - int(pre); delta > 700 || delta < -700 {
		t.Fatalf("discontinuity entering fade: %d -> %d", pre, first)
	}
}

func TestCrossfadeJoinShortTracks(t *testing.T) {
	a := constant(100, 5000)
	b := constant(100, 5000)
	joined := CrossfadeJoin([][]int16{a, b}, 4800)
	if got := len(joined) / Channels; got < 150 || got > 200 {
		t.Fatalf("short-track join frames = %d; want fade clamped to half a track", got)
	}
}

func TestApplyEdgeFades(t *testing.T) {
	s := constant(100, 20000)
	ApplyEdgeFades(s, 10)
	if s[0] != 0 || s[len(s)-1] != 0 {
		t.Fatalf("edges not silenced: first=%d last=%d", s[0], s[len(s)-1])
	}
	if s[50*Channels] != 20000 {
		t.Fatalf("middle changed: %d", s[50*Channels])
	}
}

func TestApplyGain(t *testing.T) {
	s := []int16{10000, -10000}
	ApplyGain(s, 0.5)
	if s[0] != 5000 || s[1] != -5000 {
		t.Fatalf("gain result = %v", s)
	}
}
