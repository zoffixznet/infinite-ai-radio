package audio

import "math"

// DefaultTargetRMS is the loudness-normalization target in dBFS. It
// approximates the level streaming platforms normalize to, so tracks sit
// at a consistent, familiar loudness.
const DefaultTargetRMS = -16.0

// maxBoostDB caps how much a quiet track is amplified so noise floors are
// not dragged up.
const maxBoostDB = 6.0

// NormalizeLoudness scales samples toward the target RMS level (in dBFS),
// never boosting more than maxBoostDB and never letting peaks clip. It
// returns the gain that was applied.
func NormalizeLoudness(samples []int16, targetDB float64) float64 {
	if len(samples) == 0 {
		return 1
	}
	var sum float64
	peak := 1.0
	for _, s := range samples {
		v := float64(s)
		sum += v * v
		if a := math.Abs(v); a > peak {
			peak = a
		}
	}
	rms := math.Sqrt(sum / float64(len(samples)))
	if rms < 1 {
		return 1 // silence; leave alone
	}
	target := math.Pow(10, targetDB/20) * 32768
	gain := target / rms
	if maxBoost := math.Pow(10, maxBoostDB/20); gain > maxBoost {
		gain = maxBoost
	}
	// Peak safety: leave a little headroom below full scale.
	if limit := 32000 / peak; gain > limit {
		gain = limit
	}
	if math.Abs(gain-1) < 0.01 {
		return 1
	}
	ApplyGain(samples, gain)
	return gain
}
