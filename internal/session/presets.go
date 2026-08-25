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
	// Group is the energy group the preset is listed under (one of
	// GroupOrder).
	Group string `json:"group,omitempty"`
	// Spec carries structured constraints (tempo, language, negatives)
	// that seed the session's steering spec so they actually reach the
	// engine.
	Spec *PromptSpec `json:"spec,omitempty"`
}

// GroupOrder is the fixed display order of the preset energy groups:
// pick a feeling first, steer the genre later. Unknown groups sort
// after these.
var GroupOrder = []string{"high-energy", "upbeat", "cruise", "chill", "sleep-noise"}

// GroupIndex ranks a preset group for sorting; unknown groups come last.
func GroupIndex(group string) int {
	for i, g := range GroupOrder {
		if g == group {
			return i
		}
	}
	return len(GroupOrder)
}

// Presets returns all built-in presets in display order: by energy
// group (GroupOrder), then by name inside each group.
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
	SortPresets(out)
	return out
}

// SortPresets orders presets by (group rank, name).
func SortPresets(presets []*Preset) {
	sort.Slice(presets, func(i, j int) bool {
		gi, gj := GroupIndex(presets[i].Group), GroupIndex(presets[j].Group)
		if gi != gj {
			return gi < gj
		}
		return presets[i].Name < presets[j].Name
	})
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
