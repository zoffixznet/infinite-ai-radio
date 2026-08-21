package audio

import (
	"math"
	"math/cmplx"
	"testing"
)

// fft is a simple recursive radix-2 FFT, plenty for a coarse spectral
// sanity check.
func fft(x []complex128) []complex128 {
	n := len(x)
	if n == 1 {
		return x
	}
	even := make([]complex128, n/2)
	odd := make([]complex128, n/2)
	for i := 0; i < n/2; i++ {
		even[i] = x[2*i]
		odd[i] = x[2*i+1]
	}
	fe := fft(even)
	fo := fft(odd)
	out := make([]complex128, n)
	for k := 0; k < n/2; k++ {
		t := cmplx.Exp(complex(0, -2*math.Pi*float64(k)/float64(n))) * fo[k]
		out[k] = fe[k] + t
		out[k+n/2] = fe[k] - t
	}
	return out
}

// spectralSlope estimates the power slope in dB per octave across octave
// bands between 100 Hz and 6.4 kHz.
func spectralSlope(t *testing.T, color NoiseColor) float64 {
	t.Helper()
	const n = 1 << 16
	gen := NewNoiseGenerator(color, 0.5)
	samples := gen.Generate(n)
	x := make([]complex128, n)
	for i := 0; i < n; i++ {
		x[i] = complex(float64(samples[i*Channels])/32768, 0) // left channel
	}
	spec := fft(x)

	centers := []float64{100, 200, 400, 800, 1600, 3200, 6400}
	var xs, ys []float64
	for _, fc := range centers {
		lo := int(fc / math.Sqrt2 / SampleRate * n)
		hi := int(fc * math.Sqrt2 / SampleRate * n)
		var power float64
		for k := lo; k < hi; k++ {
			power += cmplx.Abs(spec[k]) * cmplx.Abs(spec[k])
		}
		power /= float64(hi - lo) // average PSD in band
		xs = append(xs, math.Log2(fc))
		ys = append(ys, 10*math.Log10(power))
	}
	// Least-squares slope of dB vs octaves.
	var sx, sy, sxx, sxy float64
	for i := range xs {
		sx += xs[i]
		sy += ys[i]
		sxx += xs[i] * xs[i]
		sxy += xs[i] * ys[i]
	}
	m := float64(len(xs))
	return (m*sxy - sx*sy) / (m*sxx - sx*sx)
}

func TestNoiseSpectralSlopes(t *testing.T) {
	cases := []struct {
		color     NoiseColor
		want      float64
		tolerance float64
	}{
		{NoiseWhite, 0, 1.0},
		{NoisePink, -3, 1.0},
		{NoiseBrown, -6, 1.5},
	}
	for _, tc := range cases {
		slope := spectralSlope(t, tc.color)
		if math.Abs(slope-tc.want) > tc.tolerance {
			t.Errorf("%s noise slope = %.2f dB/oct; want %.1f +/- %.1f",
				tc.color, slope, tc.want, tc.tolerance)
		}
	}
}

func TestNoiseAmplitudeAndStereo(t *testing.T) {
	gen := NewNoiseGenerator(NoisePink, 0.3)
	s := gen.Generate(48000)
	if len(s) != 48000*Channels {
		t.Fatalf("generated %d samples; want %d", len(s), 48000*Channels)
	}
	var peak int16
	same := true
	for i := 0; i < len(s); i += Channels {
		for c := 0; c < Channels; c++ {
			v := s[i+c]
			if v < 0 {
				v = -v
			}
			if v > peak {
				peak = v
			}
		}
		if s[i] != s[i+1] {
			same = false
		}
	}
	if peak == 0 {
		t.Fatal("noise is silent")
	}
	if peak > 29000 {
		t.Fatalf("noise peak %d too hot for amp 0.3", peak)
	}
	if same {
		t.Fatal("stereo channels are identical; want decorrelated noise")
	}
}

func TestParseNoiseColor(t *testing.T) {
	if ParseNoiseColor("brown") != NoiseBrown {
		t.Fatal("brown not parsed")
	}
	if ParseNoiseColor("chartreuse") != NoisePink {
		t.Fatal("unknown color should default to pink")
	}
}
