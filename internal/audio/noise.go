package audio

import "math/rand/v2"

// NoiseColor identifies a noise spectrum.
type NoiseColor string

// Supported noise colors.
const (
	NoiseWhite NoiseColor = "white"
	NoisePink  NoiseColor = "pink"
	NoiseBrown NoiseColor = "brown"
)

// ParseNoiseColor maps a user word to a NoiseColor, defaulting to pink.
func ParseNoiseColor(s string) NoiseColor {
	switch NoiseColor(s) {
	case NoiseWhite, NoisePink, NoiseBrown:
		return NoiseColor(s)
	default:
		return NoisePink
	}
}

// NoiseGenerator synthesizes an endless stereo noise stream in the internal
// PCM format. The two channels run independent generator states so the
// result has natural stereo width. It is not safe for concurrent use.
type NoiseGenerator struct {
	color NoiseColor
	amp   float64
	rng   *rand.Rand
	ch    [Channels]noiseState
}

// noiseState is the per-channel filter state.
type noiseState struct {
	// Paul Kellet's refined pink noise filter taps.
	b0, b1, b2, b3, b4, b5, b6 float64
	// Leaky integrator state for brown noise.
	brown float64
}

// NewNoiseGenerator returns a generator for the given color. amp scales the
// output amplitude (0..1); 0.3 is a comfortable background level.
func NewNoiseGenerator(color NoiseColor, amp float64) *NoiseGenerator {
	if amp <= 0 || amp > 1 {
		amp = 0.3
	}
	return &NoiseGenerator{
		color: color,
		amp:   amp,
		rng:   rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64())),
	}
}

// Color reports the generator's noise color.
func (g *NoiseGenerator) Color() NoiseColor { return g.color }

// Generate fills and returns a slice of frames*Channels interleaved samples.
func (g *NoiseGenerator) Generate(frames int) []int16 {
	out := make([]int16, frames*Channels)
	for f := 0; f < frames; f++ {
		for c := 0; c < Channels; c++ {
			out[f*Channels+c] = clampSample(g.next(&g.ch[c]) * g.amp)
		}
	}
	return out
}

// next produces one sample in roughly [-1, 1] for one channel.
func (g *NoiseGenerator) next(st *noiseState) float64 {
	white := g.rng.Float64()*2 - 1
	switch g.color {
	case NoiseWhite:
		return white
	case NoiseBrown:
		// Leaky integrator over white noise: ~-6 dB/octave.
		st.brown = (st.brown + 0.02*white) / 1.02
		return st.brown * 3.5
	default: // pink
		// Paul Kellet's refined filter: ~-3 dB/octave within audio band.
		st.b0 = 0.99886*st.b0 + white*0.0555179
		st.b1 = 0.99332*st.b1 + white*0.0750759
		st.b2 = 0.96900*st.b2 + white*0.1538520
		st.b3 = 0.86650*st.b3 + white*0.3104856
		st.b4 = 0.55000*st.b4 + white*0.5329522
		st.b5 = -0.7616*st.b5 - white*0.0168980
		pink := st.b0 + st.b1 + st.b2 + st.b3 + st.b4 + st.b5 + st.b6 + white*0.5362
		st.b6 = white * 0.115926
		return pink * 0.11
	}
}
