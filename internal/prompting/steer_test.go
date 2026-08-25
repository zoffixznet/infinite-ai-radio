package prompting

import (
	"strings"
	"testing"

	"iar/internal/session"
)

func TestSteerNoiseRouting(t *testing.T) {
	s := session.New()
	ack := Steer(s, "generate pink noise")
	if s.Mode != session.ModeNoise || s.NoiseColor != "pink" {
		t.Fatalf("session not routed to noise: %+v", s)
	}
	if !ack.ContextChanged || !strings.Contains(ack.Text, "pink noise") {
		t.Fatalf("ack = %+v", ack)
	}

	ack = Steer(s, "brown noise please")
	if s.NoiseColor != "brown" {
		t.Fatalf("color not switched: %s", s.NoiseColor)
	}
	_ = ack

	// Any musical tweak returns to music mode.
	ack = Steer(s, "calmer")
	if s.Mode != session.ModeMusic {
		t.Fatal("did not return to music mode")
	}
	if !strings.Contains(ack.Text, "back to music") {
		t.Fatalf("ack should mention returning to music: %q", ack.Text)
	}
}

func TestSteerVocalRouting(t *testing.T) {
	s := session.New()
	ack := Steer(s, "add vocals about winning the day")
	if !s.Vocal {
		t.Fatal("vocal flag not set")
	}
	if s.LyricsTheme != "winning the day" {
		t.Fatalf("theme = %q", s.LyricsTheme)
	}
	if !strings.Contains(ack.Text, "vocals on") {
		t.Fatalf("ack = %q", ack.Text)
	}

	ack = Steer(s, "no vocals please")
	if s.Vocal {
		t.Fatal("vocal flag not cleared")
	}
	if !strings.Contains(ack.Text, "vocals off") {
		t.Fatalf("ack = %q", ack.Text)
	}

	Steer(s, "make it instrumental")
	if s.Vocal {
		t.Fatal("instrumental should clear vocals")
	}
}

func TestSteerTweakMappingAndPassThrough(t *testing.T) {
	s := session.New()
	Steer(s, "make it more energetic")
	if len(s.Tweaks) != 1 || !strings.Contains(s.Tweaks[0].Interpreted, "energetic") {
		t.Fatalf("tweaks = %+v", s.Tweaks)
	}
	Steer(s, "add a saxophone solo")
	if len(s.Tweaks) != 2 || !strings.Contains(s.Tweaks[1].Interpreted, "saxophone solo") {
		t.Fatalf("pass-through failed: %+v", s.Tweaks[1])
	}
	// Everything typed lands in history.
	if len(s.History) != 2 {
		t.Fatalf("history = %d; want 2", len(s.History))
	}
}

func TestSteerNegationSemantics(t *testing.T) {
	s := session.New()
	s.BasePrompt = "lofi beats, warm guitars, soft piano"

	// Only the hard markers (no/without/remove/drop) eliminate.
	ack := Steer(s, "no guitars more synths")
	if !ack.ContextChanged {
		t.Fatalf("ack = %+v", ack)
	}
	if !strings.Contains(ack.Text, "avoiding guitars") {
		t.Fatalf("hard negation ack = %q", ack.Text)
	}
	found := false
	for _, n := range s.Spec.Negatives {
		if strings.Contains(n, "guitar") {
			found = true
		}
	}
	if !found {
		t.Fatalf("guitars not negated: %+v", s.Spec.Negatives)
	}
	if s.Spec.Instruments["synths"] != 1 {
		t.Fatalf("synths not strengthened: %+v", s.Spec.Instruments)
	}

	r := Render(s)
	if strings.Contains(r.Caption, "guitar") {
		t.Fatalf("caption still mentions guitars: %q", r.Caption)
	}
	if strings.Contains(r.Caption, "less") {
		t.Fatalf("negation word leaked into the caption: %q", r.Caption)
	}
	if !strings.Contains(r.Caption, "synths") {
		t.Fatalf("caption misses synths: %q", r.Caption)
	}
	if !strings.Contains(r.NegativePrompt, "guitar") {
		t.Fatalf("negative prompt misses guitars: %q", r.NegativePrompt)
	}
	if r.LMCfgScale <= 2.5 {
		t.Fatalf("lm guidance not raised with negatives: %v", r.LMCfgScale)
	}

	// Emphasis grows with repetition and renders as reinforcement.
	Steer(s, "more synths")
	if s.Spec.Instruments["synths"] != 2 {
		t.Fatalf("weight = %d; want 2", s.Spec.Instruments["synths"])
	}
	if r := Render(s); !strings.Contains(r.Caption, "rich synths") {
		t.Fatalf("no reinforcement in caption: %q", r.Caption)
	}

	// "more guitars" un-negates them.
	Steer(s, "more guitars")
	for _, n := range s.Spec.Negatives {
		if strings.Contains(n, "guitar") {
			t.Fatalf("guitars still negated after asking for more: %+v", s.Spec.Negatives)
		}
	}
}

func TestSteerLessenDialsDownWithoutNegating(t *testing.T) {
	s := session.New()
	s.BasePrompt = "energetic electronic rock, driving beat"

	// "less X more Y": the soft op never touches negatives or raises
	// the planner guidance, and acks as dialing back, not avoiding.
	ack := Steer(s, "less electronic more synths")
	if !ack.ContextChanged {
		t.Fatalf("ack = %+v", ack)
	}
	if !strings.Contains(ack.Text, "dialing back electronic") || !strings.Contains(ack.Text, "more synths") {
		t.Fatalf("soft ack = %q", ack.Text)
	}
	if strings.Contains(ack.Text, "avoiding") {
		t.Fatalf("soft op acked as elimination: %q", ack.Text)
	}
	if len(s.Spec.Negatives) != 0 {
		t.Fatalf("soft op filled negatives: %+v", s.Spec.Negatives)
	}
	r := Render(s)
	if r.NegativePrompt != "" || r.LMCfgScale != 0 {
		t.Fatalf("soft op reached engine negatives: %+v", r)
	}
	// The base prompt survives; the hedge appears in the caption.
	if !strings.Contains(r.Caption, "energetic electronic rock") {
		t.Fatalf("base prompt segment dropped by soft op: %q", r.Caption)
	}
	if !strings.Contains(r.Caption, "subtle electronic") {
		t.Fatalf("caption carries no hedge: %q", r.Caption)
	}

	// An emphasized instrument steps down one weight level.
	Steer(s, "more synths") // weight 2
	if s.Spec.Instruments["synths"] != 2 {
		t.Fatalf("setup weight = %d", s.Spec.Instruments["synths"])
	}
	ack = Steer(s, "fewer synths")
	if !strings.Contains(ack.Text, "dialing back synths") || s.Spec.Instruments["synths"] != 1 {
		t.Fatalf("decrement: ack=%q weight=%d", ack.Text, s.Spec.Instruments["synths"])
	}
	if r := Render(s); strings.Contains(r.Caption, "rich synths") {
		t.Fatalf("reinforcement survived the dial-down: %q", r.Caption)
	}
	// The synths themselves are still in the caption; only "no synths"
	// would remove them.
	if r := Render(s); !strings.Contains(r.Caption, "synths") {
		t.Fatalf("soft op eliminated the instrument: %q", r.Caption)
	}
	// "more X" strips the hedge again.
	Steer(s, "more electronic")
	if r := Render(s); strings.Contains(r.Caption, "subtle electronic") {
		t.Fatalf("hedge survived a strengthen: %q", r.Caption)
	}
}

func TestSteerLessenFloorIsNoOp(t *testing.T) {
	s := session.New()
	Steer(s, "more synths") // weight 1
	if ack := Steer(s, "less synths"); !ack.ContextChanged {
		t.Fatalf("first dial-down must change the context: %+v", ack)
	}
	if _, ok := s.Spec.Instruments["synths"]; ok {
		t.Fatalf("weight-1 instrument should be gone after a dial-down: %+v", s.Spec.Instruments)
	}
	// The next dial-down hedges; the one after that is the floor.
	if ack := Steer(s, "less synths"); !ack.ContextChanged {
		t.Fatalf("hedging dial-down must change the context: %+v", ack)
	}
	ack := Steer(s, "less synths")
	if ack.ContextChanged || !strings.Contains(ack.Text, "nothing changed") {
		t.Fatalf("floor must be the standard no-op: %+v", ack)
	}
	if len(s.Spec.Negatives) != 0 {
		t.Fatalf("repeated dial-downs negated: %+v", s.Spec.Negatives)
	}
	// The floor no-op is not recorded as a tweak.
	if n := len(s.Tweaks); n != 3 {
		t.Fatalf("tweaks = %d; want 3", n)
	}
}

func TestSteerNoOpKeepsContext(t *testing.T) {
	s := session.New()
	ack := Steer(s, "no drums")
	if !ack.ContextChanged {
		t.Fatal("first negation must change the context")
	}
	ack = Steer(s, "no drums")
	if ack.ContextChanged {
		t.Fatalf("repeat negation must be a no-op: %+v", ack)
	}
	// The no-op is not recorded as a tweak.
	if len(s.Tweaks) != 1 {
		t.Fatalf("tweaks = %d; want 1", len(s.Tweaks))
	}
}

func TestSteerAbsoluteSettings(t *testing.T) {
	s := session.New()
	Steer(s, "120 bpm in c minor")
	if s.Spec.BPM != 120 || s.Spec.KeyScale != "C minor" {
		t.Fatalf("spec = %+v", s.Spec)
	}
	Steer(s, "faster")
	if s.Spec.BPM != 135 {
		t.Fatalf("bpm after faster = %d; want 135", s.Spec.BPM)
	}
	Steer(s, "add vocals in spanish about victory")
	if !s.Vocal || s.Spec.VocalLanguage != "es" {
		t.Fatalf("vocal language = %q vocal=%v", s.Spec.VocalLanguage, s.Vocal)
	}
	r := Render(s)
	if r.BPM != 135 || r.KeyScale != "C minor" || r.VocalLanguage != "es" {
		t.Fatalf("rendered fields = %+v", r)
	}
	if !strings.Contains(r.Caption, "135 bpm") {
		t.Fatalf("bpm missing from caption: %q", r.Caption)
	}
}

func TestOldSessionsMigrateToSpec(t *testing.T) {
	// A session saved before specs existed: recorded tweaks, nil spec.
	s := session.New()
	s.BasePrompt = "lofi beats, soft drums"
	s.Tweaks = []session.Entry{
		{Raw: "calmer", Interpreted: "calmer, softer, more gentle"},
		{Raw: "no drums", Interpreted: "beatless, no drums, no percussion"},
		{Raw: "less guitars", Interpreted: "fewer guitars"},
	}
	s.Spec = nil

	r := Render(s)
	if strings.Contains(r.Caption, "drums") {
		t.Fatalf("negated drums still in caption: %q", r.Caption)
	}
	if !strings.Contains(r.Caption, "calm") {
		t.Fatalf("replayed mood missing: %q", r.Caption)
	}
	if !strings.Contains(r.NegativePrompt, "drums") {
		t.Fatalf("negative prompt missing drums: %q", r.NegativePrompt)
	}
	// A stored "less" tweak replays as the soft dial-down, never as an
	// elimination.
	if strings.Contains(r.NegativePrompt, "guitar") {
		t.Fatalf("stored soft tweak replayed as a negative: %q", r.NegativePrompt)
	}
	// Steering keeps working on the migrated spec.
	if ack := Steer(s, "no drums"); ack.ContextChanged {
		t.Fatal("migrated negation should already be in effect")
	}
}

func TestSessionFromPrompt(t *testing.T) {
	s := SessionFromPrompt("dark techno")
	if s.BasePrompt != "dark techno" || s.Vocal || s.Mode != session.ModeMusic {
		t.Fatalf("plain prompt session wrong: %+v", s)
	}
	if !strings.HasPrefix(s.Name, "prompt-dark-techno") {
		t.Fatalf("session name = %q", s.Name)
	}

	v := SessionFromPrompt("energetic rock with vocals about winning")
	if !v.Vocal || v.LyricsTheme != "winning" {
		t.Fatalf("vocal prompt session wrong: vocal=%v theme=%q", v.Vocal, v.LyricsTheme)
	}

	n := SessionFromPrompt("brown noise")
	if n.Mode != session.ModeNoise || n.NoiseColor != "brown" {
		t.Fatalf("noise prompt session wrong: %+v", n)
	}

	empty := SessionFromPrompt("   ")
	if empty.BasePrompt == "" {
		t.Fatal("empty prompt should keep the default base prompt")
	}
}
