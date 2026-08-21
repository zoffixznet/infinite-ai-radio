package session

import (
	"embed"
	"encoding/json"
	"fmt"
	"sort"
)

//go:embed presets/*.json
var presetFS embed.FS

// Preset is a curated, read-only starting point. Loading one seeds a fresh
// session; the preset itself never changes.
type Preset struct {
	// Name is the identifier used with --preset and /preset.
	Name string `json:"name"`
	// Description is a one-line summary for listings.
	Description string `json:"description"`
	// Mode, Prompt, NoiseColor, NoiseBed, Vocal and LyricsTheme seed the
	// corresponding Session fields.
	Mode        Mode   `json:"mode"`
	Prompt      string `json:"prompt"`
	NoiseColor  string `json:"noise_color,omitempty"`
	NoiseBed    string `json:"noise_bed"`
	Vocal       bool   `json:"vocal"`
	LyricsTheme string `json:"lyrics_theme,omitempty"`
}

// Presets returns all built-in presets sorted by name.
func Presets() []*Preset {
	entries, err := presetFS.ReadDir("presets")
	if err != nil {
		return nil // embedded FS; cannot happen at runtime
	}
	var out []*Preset
	for _, e := range entries {
		data, err := presetFS.ReadFile("presets/" + e.Name())
		if err != nil {
			continue
		}
		var p Preset
		if err := json.Unmarshal(data, &p); err != nil {
			continue
		}
		out = append(out, &p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// LookupPreset finds a preset by name.
func LookupPreset(name string) (*Preset, error) {
	name = SanitizeName(name)
	for _, p := range Presets() {
		if p.Name == name {
			return p, nil
		}
	}
	return nil, fmt.Errorf("unknown preset %q (try: iar sessions)", name)
}
