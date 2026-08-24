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

	ack := Steer(s, "less guitars more synths")
	if !ack.ContextChanged {
		t.Fatalf("ack = %+v", ack)
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
