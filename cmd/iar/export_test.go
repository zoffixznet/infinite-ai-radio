package main

import (
	"testing"

	"iar/internal/session"
)

// The crash this covers: a preset that steers nothing of its own (no
// "spec" block in its JSON) loads with a nil Spec, and --language
// dereferenced it. The pin must also actually be set - without it the
// lyric writer keeps drawing from the configured language list, and a
// Spanish export comes back sung in something else.
func TestPinLanguageOnASpeclessSession(t *testing.T) {
	s := session.New()
	s.Spec = nil
	if err := pinLanguage(s, "Spanish"); err != nil {
		t.Fatalf("pinLanguage: %v", err)
	}
	if s.Spec == nil {
		t.Fatal("no spec was made for a spec-less session")
	}
	if s.Spec.VocalLanguage != "es" {
		t.Fatalf("language = %q, want es", s.Spec.VocalLanguage)
	}
	if !s.Spec.LanguagePinned {
		t.Fatal("the language was set but not pinned")
	}
}

func TestPinLanguageAcceptsNamesCodesAndNothing(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", ""}, {"fr", "fr"}, {"French", "fr"}, {"ENGLISH", "en"},
		// Unlisted codes pass through: the tag is advisory and the
		// lyric writer follows it either way.
		{"ceb", "ceb"},
	} {
		s := session.New()
		if err := pinLanguage(s, tc.in); err != nil {
			t.Fatalf("pinLanguage(%q): %v", tc.in, err)
		}
		got := ""
		if s.Spec != nil {
			got = s.Spec.VocalLanguage
		}
		if got != tc.want {
			t.Errorf("pinLanguage(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if err := pinLanguage(session.New(), "Klingon"); err == nil {
		t.Fatal("an unknown language name was accepted")
	}
}
