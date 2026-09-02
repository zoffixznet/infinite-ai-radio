package acestep

import (
	"encoding/json"
	"strings"
	"testing"

	"iar/internal/engine"
)

// The planner's elaborated prose caption - different for every plan -
// is what the music generator should be conditioned on; the echo of
// the terse steering tag list is only the fallback. Losing this was a
// large share of why a day of phased radio blurred together.
func TestPlanCaptionPrefersThePlannersProse(t *testing.T) {
	res := &GenerateResult{Prompt: "nu-metal, aggressive, heavy groove"}
	res.Metas.Caption = "An aggressive, high-energy nu-metal track driven by down-tuned guitars."
	if got := elaboratedCaption(res); !strings.HasPrefix(got, "An aggressive") {
		t.Fatalf("caption = %q", got)
	}
	res.Metas.Caption = "  "
	if got := elaboratedCaption(res); got != "nu-metal, aggressive, heavy groove" {
		t.Fatalf("fallback caption = %q", got)
	}
}

// A render request must pin the server's CoT switches off - they
// default to true when absent, and a render whose plan came back with
// a hole in its metadata would otherwise have its caption re-invented
// by the planner LM at render time.
func TestRenderRequestPinsCoTOff(t *testing.T) {
	req := GenerateRequest{
		UseCotCaption:  ptrFalse(),
		UseCotLanguage: ptrFalse(),
		UseCotMetas:    ptrFalse(),
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{`"use_cot_caption":false`, `"use_cot_language":false`, `"use_cot_metas":false`} {
		if !strings.Contains(string(raw), k) {
			t.Fatalf("render request missing %s: %s", k, raw)
		}
	}
	// Every other path leaves the switches to the server's defaults.
	raw, _ = json.Marshal(GenerateRequest{})
	if strings.Contains(string(raw), "use_cot") {
		t.Fatalf("plain request must not mention the CoT switches: %s", raw)
	}
}

// Plans always think, and the steered negative conditioning survives a
// config that turned thinking off for fused jobs.
func TestPlanKeepsNegativesWhenConfigThinkingIsOff(t *testing.T) {
	e := &Engine{opts: Options{Thinking: false, InferenceSteps: 12}}
	spec := engine.Spec{
		Prompt:         "nu-metal, aggressive",
		Lyrics:         "[Verse]\nsteel in the water",
		NegativePrompt: "acoustic guitar, muddy low-end",
		LMCfgScale:     3,
		Seconds:        150,
		Seed:           -1,
	}
	req := e.planRequest(spec)
	if !req.Thinking {
		t.Fatal("a plan must think")
	}
	if req.LMNegativePrompt != "acoustic guitar, muddy low-end" || req.LMCfgScale != 3 {
		t.Fatalf("negatives dropped: %+v", req)
	}
}
