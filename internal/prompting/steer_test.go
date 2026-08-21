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
	if len(s.Tweaks) != 2 || s.Tweaks[1].Interpreted != "add a saxophone solo" {
		t.Fatalf("pass-through failed: %+v", s.Tweaks[1])
	}
	// Everything typed lands in history.
	if len(s.History) != 2 {
		t.Fatalf("history = %d; want 2", len(s.History))
	}
}

func TestMergePromptDeterministic(t *testing.T) {
	s := session.New()
	s.BasePrompt = "lofi beats"
	Steer(s, "calmer")
	Steer(s, "no drums")
	got := mergePrompt(s)
	if !strings.HasPrefix(got, "lofi beats, ") {
		t.Fatalf("base prompt missing: %q", got)
	}
	if !strings.Contains(got, "calmer") || !strings.Contains(got, "no drums") {
		t.Fatalf("tweaks missing: %q", got)
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
