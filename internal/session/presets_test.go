package session

import (
	"testing"
)

// TestPresetFilesAllParse guards against silently dropped preset files:
// every embedded JSON file must survive parsing and carry the required
// fields, and the shipped set must be complete.
func TestPresetFilesAllParse(t *testing.T) {
	entries, err := presetFS.ReadDir("presets")
	if err != nil {
		t.Fatal(err)
	}
	ps := Presets()
	if len(ps) != len(entries) {
		t.Fatalf("%d preset files but %d parsed presets (malformed JSON is dropped silently)", len(entries), len(ps))
	}
	if len(ps) != 21 {
		t.Fatalf("preset count = %d; want 21", len(ps))
	}
	names := map[string]bool{}
	for _, p := range ps {
		if p.Name == "" || p.Description == "" {
			t.Errorf("preset with empty name or description: %+v", p)
		}
		if names[p.Name] {
			t.Errorf("duplicate preset name %q", p.Name)
		}
		names[p.Name] = true
		if GroupIndex(p.Group) >= len(GroupOrder) {
			t.Errorf("preset %s has unknown group %q", p.Name, p.Group)
		}
		if p.Mode == ModeMusic && p.Prompt == "" {
			t.Errorf("music preset %s has no prompt", p.Name)
		}
		if p.Vocal && p.LyricsTheme == "" {
			t.Errorf("vocal preset %s has no lyrics theme", p.Name)
		}
	}
	for _, want := range []string{"nu-metal", "grind", "hard-rock", "pink-noise", "lofi-study"} {
		if !names[want] {
			t.Errorf("preset %s missing", want)
		}
	}
}

// TestPresetGroupsOrdered asserts the energy-group display order and
// that GroupPresets preserves it.
func TestPresetGroupsOrdered(t *testing.T) {
	groups := GroupPresets(Presets())
	var got []string
	for _, g := range groups {
		got = append(got, g.Name)
		if len(g.Presets) == 0 {
			t.Errorf("group %s is empty", g.Name)
		}
	}
	if len(got) != len(GroupOrder) {
		t.Fatalf("groups = %v; want %v", got, GroupOrder)
	}
	for i, name := range GroupOrder {
		if got[i] != name {
			t.Fatalf("groups = %v; want %v", got, GroupOrder)
		}
	}
	// Unknown groups sort last.
	mixed := []*Preset{
		{Name: "z", Group: "high-energy"},
		{Name: "a", Group: "mystery"},
		{Name: "m", Group: "sleep-noise"},
	}
	SortPresets(mixed)
	if mixed[0].Name != "z" || mixed[1].Name != "m" || mixed[2].Name != "a" {
		t.Fatalf("unknown group not sorted last: %+v", mixed)
	}
}

// TestFromPresetCarriesSpec asserts a preset's structured constraints
// reach the seeded session (and stay independent of the preset).
func TestFromPresetCarriesSpec(t *testing.T) {
	p, err := LookupPreset("nu-metal")
	if err != nil {
		t.Fatal(err)
	}
	// Presets deliberately pin no tempo: the model chooses per track,
	// which is a large part of what keeps a station's songs varied.
	if p.Spec == nil || p.Spec.BPM != 0 || p.Spec.VocalLanguage != "en" || len(p.Spec.Negatives) != 2 {
		t.Fatalf("nu-metal spec = %+v", p.Spec)
	}
	s := FromPreset(p)
	if s.Spec == nil || s.Spec.VocalLanguage != "en" {
		t.Fatalf("session spec not seeded: %+v", s.Spec)
	}
	s.Spec.BPM = 60
	if p.Spec.BPM != 0 {
		t.Fatal("mutating the session spec changed the preset (missing clone)")
	}
	// A preset without a spec seeds a session with a nil spec.
	lofi, err := LookupPreset("lofi-study")
	if err != nil {
		t.Fatal(err)
	}
	if lofi.Spec != nil {
		t.Fatalf("lofi-study unexpectedly has a spec: %+v", lofi.Spec)
	}
	if s2 := FromPreset(lofi); s2.Spec != nil {
		t.Fatalf("nil preset spec should seed a nil session spec, got %+v", s2.Spec)
	}
}
