package acestep

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"iar/internal/config"
)

// cleanEngineTree materializes the pinned engine source, unpatched, from
// the local checkout's git history. Without a checkout there is nothing
// to test against, which is the normal case on a machine that has not
// run setup.
func cleanEngineTree(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	paths, err := config.ResolvePaths()
	if err != nil {
		t.Skip("no data dir")
	}
	src := paths.EngineDir()
	if _, err := os.Stat(filepath.Join(src, ".git")); err != nil {
		t.Skip("no engine checkout to test against")
	}
	dst := t.TempDir()
	// archive of HEAD is the pinned tag's source with no patches on it,
	// whatever state the working tree happens to be in.
	tar := exec.Command("git", "-C", src, "archive", "HEAD")
	out, err := tar.Output()
	if err != nil {
		t.Skipf("git archive failed: %v", err)
	}
	untar := exec.Command("tar", "-x", "-C", dst)
	untar.Stdin = strings.NewReader(string(out))
	if err := untar.Run(); err != nil {
		t.Skipf("tar failed: %v", err)
	}
	return dst
}

// Every patch must apply to the pinned source, and applying twice must
// change nothing. The second half matters because a hunk whose
// replacement restates its own anchor will happily append a second copy
// of itself when the replacement text is later edited - which is how
// the DiT helpers ended up defined twice in a live install.
func TestPatchesApplyCleanlyAndAreIdempotent(t *testing.T) {
	dir := cleanEngineTree(t)

	applied, err := ApplyEnginePatches(dir)
	if err != nil {
		t.Fatalf("applying to the pinned source failed: %v", err)
	}
	want := make(map[string]bool, len(enginePatches))
	for _, p := range enginePatches {
		want[p.name] = true
	}
	for _, name := range applied {
		delete(want, name)
	}
	if len(want) != 0 {
		t.Errorf("patches that did not apply: %v", want)
	}

	again, err := ApplyEnginePatches(dir)
	if err != nil {
		t.Fatalf("second pass failed: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("second pass changed %v; patching is not idempotent", again)
	}

	loader := filepath.Join(dir, "acestep/core/generation/handler/init_service_loader.py")
	raw, err := os.ReadFile(loader)
	if err != nil {
		t.Fatal(err)
	}
	for _, decl := range []string{"def _iar_materialize_dit", "def _iar_evict_dit"} {
		if n := strings.Count(string(raw), decl); n != 1 {
			t.Errorf("%q appears %d times, want 1", decl, n)
		}
	}
}

// The whole point of the audio_codes hunk: with the DiT deferred, the
// planner's codes must survive to the renderer. The old guard returned
// before the model context could stream the weights in, the callers
// quietly conditioned on silence instead, and every song came out with
// a 5 Hz comb on it.
func TestDeferredDiTKeepsThePlannedAudioCodes(t *testing.T) {
	dir := cleanEngineTree(t)
	if _, err := ApplyEnginePatches(dir); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "acestep/core/generation/handler/audio_codes.py"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	start := strings.Index(src, "def _decode_audio_codes_to_latents")
	end := strings.Index(src, "def convert_src_audio_to_codes")
	if start < 0 || end < start {
		t.Fatal("could not isolate the decode function")
	}
	fn := src[start:end]

	if !strings.Contains(fn, `if self.model is None and not getattr(self, "offload_dit_to_disk", False)`) {
		t.Error("the entry guard does not tolerate a deferred DiT")
	}
	ctxAt := strings.Index(fn, `with self._load_model_context("model")`)
	checkAt := strings.Index(fn, `if self.model is None or not hasattr(self.model, "tokenizer")`)
	if ctxAt < 0 || checkAt < 0 {
		t.Fatalf("expected both the model context and the tokenizer check:\n%s", fn)
	}
	if checkAt < ctxAt {
		t.Error("the tokenizer check still runs before the weights are streamed in")
	}
	if !strings.Contains(fn, "this render will not follow its plan") {
		t.Error("a dropped code set is still silent; it must be logged loudly")
	}
}

// A hunk carrying a guard must refuse to apply over an older version of
// itself rather than appending a duplicate.
func TestGuardedHunkRefusesToDuplicateItself(t *testing.T) {
	dir := cleanEngineTree(t)
	if _, err := ApplyEnginePatches(dir); err != nil {
		t.Fatal(err)
	}
	loader := filepath.Join(dir, "acestep/core/generation/handler/init_service_loader.py")
	raw, err := os.ReadFile(loader)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate the patch text having been edited since it was applied:
	// the guard marker is present, the current replacement is not.
	edited := strings.Replace(string(raw), "self.model = self.model.to(self.dtype)", "pass  # older version", 1)
	if edited == string(raw) {
		t.Skip("force-cast line not present to edit")
	}
	if err := os.WriteFile(loader, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = ApplyEnginePatches(dir)
	if err == nil {
		t.Fatal("applying over an older version of a guarded hunk should fail loudly")
	}
	if !strings.Contains(err.Error(), "older version") {
		t.Errorf("error does not explain the situation: %v", err)
	}
	// And it must not have duplicated anything on the way out.
	raw, _ = os.ReadFile(loader)
	if n := strings.Count(string(raw), "def _iar_materialize_dit"); n != 1 {
		t.Errorf("_iar_materialize_dit appears %d times after the refusal, want 1", n)
	}
}
