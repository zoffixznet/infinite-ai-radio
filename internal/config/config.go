// Package config resolves application directories and loads the user
// configuration file, providing sensible defaults for everything.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// Config is the user-tunable configuration. Every field has a working
// default; the config file only needs to contain overrides.
type Config struct {
	// Engine selects the generation engine: "acestep" or "noise".
	Engine string `json:"engine"`
	// Player selects the audio output backend: "auto", "pipe", "null" or
	// "file". "auto" picks the best available real backend.
	Player string `json:"player"`
	// TrackSeconds is the duration of each generated track.
	TrackSeconds int `json:"track_seconds"`
	// CrossfadeSeconds is the overlap between consecutive tracks.
	CrossfadeSeconds float64 `json:"crossfade_seconds"`
	// BufferTracks is how many finished tracks the generate-ahead worker
	// keeps queued beyond the one currently playing.
	BufferTracks int `json:"buffer_tracks"`
	// Volume is the output volume in percent (0-100).
	Volume int `json:"volume"`

	ACEStep ACEStep `json:"acestep"`
	Ollama  Ollama  `json:"ollama"`
}

// ACEStep configures the default music engine and its sidecar process.
type ACEStep struct {
	// Port is the localhost port the sidecar API server listens on.
	Port int `json:"port"`
	// LMModelPath names the 5Hz language-model checkpoint the engine uses
	// for planning ("acestep-5Hz-lm-0.6B" or "acestep-5Hz-lm-1.7B").
	// Empty lets the engine pick one matching the GPU.
	LMModelPath string `json:"lm_model_path"`
	// InferenceSteps is the diffusion step count (turbo model: 1-20).
	InferenceSteps int `json:"inference_steps"`
	// Thinking enables the engine's planner LM for higher quality output.
	Thinking bool `json:"thinking"`
	// RepoURL and Tag pin the engine source checkout installed by setup.
	RepoURL string `json:"repo_url"`
	Tag     string `json:"tag"`
}

// Ollama configures optional prompt rewriting through a local Ollama daemon.
// The application works fully without it.
type Ollama struct {
	// Enabled turns Ollama-assisted prompt rewriting on or off.
	Enabled bool `json:"enabled"`
	// URL is the daemon base URL.
	URL string `json:"url"`
	// Model names the model to use; empty picks the first installed model.
	Model string `json:"model"`
}

// Default returns the built-in configuration.
func Default() Config {
	return Config{
		Engine:           "acestep",
		Player:           "auto",
		TrackSeconds:     150,
		CrossfadeSeconds: 3,
		BufferTracks:     2,
		Volume:           80,
		ACEStep: ACEStep{
			Port:           8451,
			LMModelPath:    "",
			InferenceSteps: 8,
			Thinking:       true,
			RepoURL:        "https://github.com/ace-step/ACE-Step-1.5",
			Tag:            "v0.1.8",
		},
		Ollama: Ollama{
			Enabled: true,
			URL:     "http://127.0.0.1:11434",
			Model:   "",
		},
	}
}

// Load reads the configuration file under p, applying defaults for any
// missing fields. A missing file is not an error; a malformed one is.
func Load(p Paths) (Config, error) {
	cfg := Default()
	data, err := os.ReadFile(p.ConfigFile())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return cfg, nil
		}
		return cfg, fmt.Errorf("reading config: %w", err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parsing %s: %w", p.ConfigFile(), err)
	}
	cfg.sanitize()
	return cfg, nil
}

// sanitize clamps out-of-range values back to safe ones.
func (c *Config) sanitize() {
	if c.TrackSeconds < 30 {
		c.TrackSeconds = 30
	}
	if c.TrackSeconds > 300 {
		c.TrackSeconds = 300
	}
	if c.CrossfadeSeconds < 0.5 {
		c.CrossfadeSeconds = 0.5
	}
	if c.CrossfadeSeconds > 10 {
		c.CrossfadeSeconds = 10
	}
	if c.BufferTracks < 1 {
		c.BufferTracks = 1
	}
	if c.BufferTracks > 4 {
		c.BufferTracks = 4
	}
	if c.Volume < 0 {
		c.Volume = 0
	}
	if c.Volume > 100 {
		c.Volume = 100
	}
	if c.ACEStep.InferenceSteps < 1 || c.ACEStep.InferenceSteps > 20 {
		c.ACEStep.InferenceSteps = 8
	}
}
