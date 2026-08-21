package audio

import "math"

// EqualPowerGains returns the fade-out and fade-in gains at progress t in
// [0, 1] of an equal-power crossfade. The two gains always satisfy
// out^2 + in^2 = 1, keeping perceived loudness constant across the joint.
func EqualPowerGains(t float64) (out, in float64) {
	if t < 0 {
		t = 0
	}
	if t > 1 {
		t = 1
	}
	angle := t * math.Pi / 2
	return math.Cos(angle), math.Sin(angle)
}

// MixEqualPower overlap-adds the tail of one source and the head of the next
// with equal-power gains. tail and head must be the same length and contain
// interleaved stereo samples; the mix is written into dst (which may alias
// either input). Lengths are frame-aligned by truncation.
func MixEqualPower(dst, tail, head []int16) {
	frames := len(tail) / Channels
	if h := len(head) / Channels; h < frames {
		frames = h
	}
	if d := len(dst) / Channels; d < frames {
		frames = d
	}
	if frames == 0 {
		return
	}
	for f := 0; f < frames; f++ {
		t := float64(f) / float64(frames)
		gOut, gIn := EqualPowerGains(t)
		for c := 0; c < Channels; c++ {
			i := f*Channels + c
			v := float64(tail[i])*gOut + float64(head[i])*gIn
			dst[i] = clampInt(int32(math.Round(v)))
		}
	}
}

// CrossfadeJoin concatenates tracks, overlapping consecutive tracks by
// fadeFrames frames with an equal-power crossfade. Tracks shorter than twice
// the fade are joined with a proportionally shorter fade.
func CrossfadeJoin(tracks [][]int16, fadeFrames int) []int16 {
	var out []int16
	for _, t := range tracks {
		if len(t) == 0 {
			continue
		}
		if len(out) == 0 {
			out = append(out, t...)
			continue
		}
		fade := fadeFrames
		if outFrames := len(out) / Channels; fade > outFrames/2 {
			fade = outFrames / 2
		}
		if tFrames := len(t) / Channels; fade > tFrames/2 {
			fade = tFrames / 2
		}
		overlap := fade * Channels
		tailStart := len(out) - overlap
		MixEqualPower(out[tailStart:], out[tailStart:], t[:overlap])
		out = append(out, t[overlap:]...)
	}
	return out
}

// ApplyEdgeFades applies short linear fades to the first and last fadeFrames
// frames of samples, killing boundary clicks.
func ApplyEdgeFades(samples []int16, fadeFrames int) {
	frames := len(samples) / Channels
	if fadeFrames*2 > frames {
		fadeFrames = frames / 2
	}
	for f := 0; f < fadeFrames; f++ {
		g := float64(f) / float64(fadeFrames)
		for c := 0; c < Channels; c++ {
			samples[f*Channels+c] = int16(float64(samples[f*Channels+c]) * g)
			j := (frames-1-f)*Channels + c
			samples[j] = int16(float64(samples[j]) * g)
		}
	}
}

// ApplyGain scales samples in place by gain (0 silences, 1 leaves as-is).
func ApplyGain(samples []int16, gain float64) {
	if gain == 1 {
		return
	}
	for i, v := range samples {
		samples[i] = clampInt(int32(math.Round(float64(v) * gain)))
	}
}
