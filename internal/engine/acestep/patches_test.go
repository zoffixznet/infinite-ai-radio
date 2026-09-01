package acestep

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixtureEngine writes a fake engine tree whose files contain each
// hunk's anchor text, returning its root. Several hunks (from any
// patch) may target the same file, so anchors accumulate per file.
func fixtureEngine(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]*strings.Builder{}
	for _, p := range enginePatches {
		for _, h := range p.hunks {
			b := files[h.file]
			if b == nil {
				b = &strings.Builder{}
				files[h.file] = b
			}
			b.WriteString("# fixture section for " + p.name + "\n")
			b.WriteString(h.find)
		}
	}
	for file, b := range files {
		path := filepath.Join(dir, file)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
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
		for i, h := range p.hunks {
			raw, err := os.ReadFile(filepath.Join(dir, h.file))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(raw), h.replace) {
				t.Errorf("patch %s hunk %d: replacement missing after apply", p.name, i+1)
			}
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
	h := enginePatches[0].hunks[0]
	path := filepath.Join(dir, h.file)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	drifted := strings.Replace(string(raw), h.find, "# upstream rewrote this\n", 1)
	if err := os.WriteFile(path, []byte(drifted), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyEnginePatches(dir); err == nil {
		t.Fatal("apply on drifted source succeeded; want an error")
	}
}

func TestApplyEnginePatchesRefusesAmbiguousAnchor(t *testing.T) {
	dir := fixtureEngine(t)
	h := enginePatches[0].hunks[0]
	path := filepath.Join(dir, h.file)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(raw, []byte(h.find)...), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyEnginePatches(dir); err == nil {
		t.Fatal("apply with ambiguous anchor succeeded; want an error")
	}
}

// TestEnginePatchesFitInstalledEngine checks the real checkout on this
// machine: every hunk must either already be applied byte-exactly or
// have exactly one anchor match. On a machine whose checkout carries
// the patches, this doubles as a transcription-fidelity check between
// this file and what is actually running. Skipped where no engine is
// installed.
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
		for i, h := range p.hunks {
			raw, err := os.ReadFile(filepath.Join(dir, h.file))
			if err != nil {
				t.Fatalf("patch %s: %v", p.name, err)
			}
			src := string(raw)
			if strings.Contains(src, h.replace) {
				continue
			}
			if n := strings.Count(src, h.find); n != 1 {
				t.Errorf("patch %s hunk %d: neither applied nor a unique anchor (%d matches) in installed engine", p.name, i+1, n)
			}
		}
	}
}
