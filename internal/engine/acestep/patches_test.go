package acestep

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixtureEngine writes a fake engine tree whose files contain each
// patch's anchor text, returning its root.
func fixtureEngine(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, p := range enginePatches {
		path := filepath.Join(dir, p.file)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		var b strings.Builder
		b.WriteString("# synthetic fixture for " + p.name + "\n")
		for _, h := range p.hunks {
			b.WriteString("# unrelated line\n")
			b.WriteString(h.find)
		}
		if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestApplyEnginePatchesAppliesOnceAndIsIdempotent(t *testing.T) {
	dir := fixtureEngine(t)

	applied, err := ApplyEnginePatches(dir)
	if err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if len(applied) != len(enginePatches) {
		t.Fatalf("applied %v; want all %d patches", applied, len(enginePatches))
	}
	for _, p := range enginePatches {
		raw, err := os.ReadFile(filepath.Join(dir, p.file))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), p.marker) {
			t.Errorf("patch %s: marker missing after apply", p.name)
		}
	}

	applied, err = ApplyEnginePatches(dir)
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if len(applied) != 0 {
		t.Fatalf("second apply changed %v; want nothing", applied)
	}
}

func TestApplyEnginePatchesRefusesDriftedSource(t *testing.T) {
	dir := fixtureEngine(t)
	// Corrupt the first patch's anchor so it no longer matches.
	p := enginePatches[0]
	path := filepath.Join(dir, p.file)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	drifted := strings.Replace(string(raw), p.hunks[0].find, "# upstream rewrote this\n", 1)
	if err := os.WriteFile(path, []byte(drifted), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyEnginePatches(dir); err == nil {
		t.Fatal("apply on drifted source succeeded; want an error")
	}
}

func TestApplyEnginePatchesRefusesAmbiguousAnchor(t *testing.T) {
	dir := fixtureEngine(t)
	p := enginePatches[0]
	path := filepath.Join(dir, p.file)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Duplicate the anchor so it matches twice.
	if err := os.WriteFile(path, append(raw, []byte(p.hunks[0].find)...), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyEnginePatches(dir); err == nil {
		t.Fatal("apply with ambiguous anchor succeeded; want an error")
	}
}

// TestEnginePatchesFitInstalledEngine checks the real checkout on this
// machine: every patch must either already be applied or have exactly
// one anchor match. Skipped where no engine is installed.
func TestEnginePatchesFitInstalledEngine(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	dir := filepath.Join(home, ".local", "share", "iar", "engine")
	if _, err := os.Stat(dir); err != nil {
		t.Skip("no installed engine")
	}
	for _, p := range enginePatches {
		raw, err := os.ReadFile(filepath.Join(dir, p.file))
		if err != nil {
			t.Fatalf("patch %s: %v", p.name, err)
		}
		src := string(raw)
		if strings.Contains(src, p.marker) {
			continue
		}
		for i, h := range p.hunks {
			if n := strings.Count(src, h.find); n != 1 {
				t.Errorf("patch %s hunk %d: anchor matches %d times in installed engine", p.name, i+1, n)
			}
		}
	}
}
