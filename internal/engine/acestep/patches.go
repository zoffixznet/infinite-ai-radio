package acestep

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The engine checkout is pinned to a release tag, but two of its defects
// hit this application hard enough that waiting for an upstream release
// is not an option. Each fix below is a minimal source edit applied to
// the checkout by setup (and re-applied after a tag update, which resets
// the tree first). A patch anchors on the exact upstream text and refuses
// to apply when that text has drifted, so a future tag bump either
// carries the fix cleanly or fails setup loudly instead of silently
// shipping without it.

// patchHunk replaces one uniquely-occurring stretch of source text.
type patchHunk struct {
	find    string
	replace string
}

// enginePatch is one named fix. marker is a string present only in the
// patched form; its presence means the patch is already in place.
type enginePatch struct {
	name   string
	file   string
	marker string
	hunks  []patchHunk
}

var enginePatches = []enginePatch{
	{
		// The planner LM's two custom decoding loops read only the
		// final position's logits, but their shared prefill forward
		// pass materializes logits for every prompt position: a
		// [batch, prompt_len, 217k-vocab] bf16 tensor of 0.5-1.8 GiB,
		// allocated and discarded unread on every lyric-planning call.
		// On a card shared with other tenants that transient is what
		// runs out of memory. Keeping only the last position is
		// numerically identical.
		name:   "lm-prefill-logits",
		file:   "acestep/llm_inference.py",
		marker: "iar-patch: lm-prefill-logits",
		hunks: []patchHunk{{
			find: `        if past_key_values is None:
            outputs = model(
                input_ids=generated_ids,
                **model_kwargs,
                use_cache=use_cache,
            )
`,
			replace: `        if past_key_values is None:
            # iar-patch: lm-prefill-logits. Both generation loops read
            # only logits[:, -1, :], but this prefill materialized
            # logits for every prompt position - a huge tensor
            # allocated and thrown away unread. Keep the final
            # position only; the output is numerically identical.
            outputs = model(
                input_ids=generated_ids,
                **model_kwargs,
                use_cache=use_cache,
                logits_to_keep=1,
            )
`,
		}},
	},
	{
		// In sample mode the planner LM chooses the track length and
		// the request's audio_duration is discarded, so nothing bounds
		// how long (and how memory- and time-expensive) a track can
		// get. When the supervisor sets ACESTEP_SAMPLE_DURATION_CAP,
		// treat it as a ceiling on the LM's choice; shorter choices
		// pass through untouched, so track lengths stay varied.
		name:   "sample-duration-cap",
		file:   "acestep/api/llm_generation_inputs.py",
		marker: "iar-patch: sample-duration-cap",
		hunks: []patchHunk{
			{
				find:    "from dataclasses import dataclass\n",
				replace: "import os\n\nfrom dataclasses import dataclass\n",
			},
			{
				find: "        audio_duration = sample_result.duration\n",
				replace: `        audio_duration = sample_result.duration
        # iar-patch: sample-duration-cap (see the supervisor's patch
        # list for rationale).
        _iar_cap_raw = os.getenv("ACESTEP_SAMPLE_DURATION_CAP", "")
        if _iar_cap_raw:
            try:
                _iar_cap = float(_iar_cap_raw)
            except ValueError:
                _iar_cap = 0.0
            if _iar_cap > 0 and audio_duration is not None:
                try:
                    if float(audio_duration) > _iar_cap:
                        audio_duration = _iar_cap
                except (TypeError, ValueError):
                    pass
`,
			},
		},
	},
}

// ApplyEnginePatches brings the engine checkout under dir up to date
// with every patch, returning the names of those newly applied. A file
// already carrying a patch's marker is left alone, so the call is
// idempotent. An anchor that matches zero times or more than once means
// the upstream source changed under the patch; that is an error, never
// a silent skip.
func ApplyEnginePatches(dir string) ([]string, error) {
	var applied []string
	for _, p := range enginePatches {
		path := filepath.Join(dir, p.file)
		raw, err := os.ReadFile(path)
		if err != nil {
			return applied, fmt.Errorf("patch %s: %w", p.name, err)
		}
		src := string(raw)
		if strings.Contains(src, p.marker) {
			continue
		}
		for i, h := range p.hunks {
			switch n := strings.Count(src, h.find); n {
			case 1:
				src = strings.Replace(src, h.find, h.replace, 1)
			default:
				return applied, fmt.Errorf(
					"patch %s hunk %d: anchor matches %d times in %s (engine source changed; update the patch)",
					p.name, i+1, n, p.file)
			}
		}
		if !strings.Contains(src, p.marker) {
			return applied, fmt.Errorf("patch %s: marker absent after applying (broken patch definition)", p.name)
		}
		info, err := os.Stat(path)
		if err != nil {
			return applied, fmt.Errorf("patch %s: %w", p.name, err)
		}
		if err := os.WriteFile(path, []byte(src), info.Mode().Perm()); err != nil {
			return applied, fmt.Errorf("patch %s: %w", p.name, err)
		}
		applied = append(applied, p.name)
	}
	return applied, nil
}
