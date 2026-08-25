package session

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStoreSaveLoadRoundTrip(t *testing.T) {
	st := NewStore(t.TempDir())
	s := New()
	s.AddTweak("more energetic", "more energetic, driving")
	if err := st.Save(s); err != nil {
		t.Fatal(err)
	}
	got, err := st.Load(s.Name)
	if err != nil {
		t.Fatal(err)
	}
	if got.BasePrompt != s.BasePrompt || len(got.Tweaks) != 1 || got.Tweaks[0].Raw != "more energetic" {
		t.Fatalf("round trip mismatch: %+v", got)
	}
}

func TestStoreRenameViaSaveAndDelete(t *testing.T) {
	st := NewStore(t.TempDir())
	s := New()
	old := s.Name
	if err := st.Save(s); err != nil {
		t.Fatal(err)
	}
	s.Name = SanitizeName("Gym Grind!")
	if s.Name != "gym-grind" {
		t.Fatalf("sanitized name = %q", s.Name)
	}
	if err := st.Save(s); err != nil {
		t.Fatal(err)
	}
	st.Delete(old)
	if _, err := st.Load("gym-grind"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Load(old); err == nil {
		t.Fatal("old name still loads after rename")
	}
}

func TestStoreListToleratesCorruptFiles(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir)
	s := New()
	s.Name = "good"
	if err := st.Save(s); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bad.json"), []byte("{nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	list, err := st.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Name != "good" {
		t.Fatalf("list = %+v; want only the good session", list)
	}
}

func TestStoreLoadMissing(t *testing.T) {
	st := NewStore(t.TempDir())
	if _, err := st.Load("nope"); err == nil {
		t.Fatal("missing session loaded")
	}
}

func TestPresetsShipRequiredSet(t *testing.T) {
	ps := Presets()
	if len(ps) < 5 {
		t.Fatalf("only %d presets; want at least 5", len(ps))
	}
	byName := map[string]*Preset{}
	for _, p := range ps {
		byName[p.Name] = p
		if p.Description == "" {
			t.Errorf("preset %s has no description", p.Name)
		}
	}
	grind, ok := byName["grind"]
	if !ok || !grind.Vocal || grind.LyricsTheme == "" {
		t.Fatalf("grind vocal preset missing or not vocal: %+v", grind)
	}
	pink, ok := byName["pink-noise"]
	if !ok || pink.Mode != ModeNoise || pink.NoiseColor != "pink" {
		t.Fatalf("pink-noise preset wrong: %+v", pink)
	}
	for _, name := range []string{"lofi-study", "deep-focus", "sleep", "jazz-club", "night-drive"} {
		p, ok := byName[name]
		if !ok {
			t.Fatalf("preset %s missing", name)
		}
		if p.Mode != ModeMusic || p.Prompt == "" || p.Vocal {
			t.Fatalf("preset %s should be instrumental music with a prompt: %+v", name, p)
		}
	}
}

func TestFromPresetSeedsSession(t *testing.T) {
	p, err := LookupPreset("grind")
	if err != nil {
		t.Fatal(err)
	}
	s := FromPreset(p)
	if s.Preset != "grind" || !s.Vocal || s.BasePrompt != p.Prompt {
		t.Fatalf("session not seeded: %+v", s)
	}
	if s.Name == "" {
		t.Fatal("seeded session has no name")
	}
}

func TestClearKeepsHistory(t *testing.T) {
	s := New()
	s.AddTweak("calmer", "calmer, softer")
	s.Clear()
	if len(s.Tweaks) != 0 {
		t.Fatal("tweaks not cleared")
	}
	if len(s.History) != 2 { // tweak + clear
		t.Fatalf("history = %d entries; want 2", len(s.History))
	}
}
