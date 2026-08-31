package prompting

import (
	"context"
	"strings"
	"testing"

	"iar/internal/session"
)

func TestLanguageCodeResolution(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		{"English", "en"},
		{"english", "en"},
		{"  Russian  ", "ru"},
		{"Mandarin", "zh"},
		{"Brazilian Portuguese", "pt"},
		{"ru", "ru"},
		{"yue", "yue"},
		{"Filipino", "tl"},
		// A name that qualifies itself resolves on either half.
		{"Norwegian (Bokmal)", "no"},
		{"Tagalog / Filipino", "tl"},
		// Not in the engine's published list, but it accepts the tag
		// and the model behind it writes Cebuano for it.
		{"Bisaya (Cebuano)", "ceb"},
		// The engine has no tag and no near relative for these; the
		// lyrics still get written in them, so an empty code is the
		// correct answer, not an error.
		{"Klingon", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := LanguageCode(c.name); got != c.want {
			t.Errorf("LanguageCode(%q) = %q; want %q", c.name, got, c.want)
		}
	}
}

func TestParseLanguagesKeepsOrderAndDropsRepeats(t *testing.T) {
	got := ParseLanguages([]string{"English", " russian ", "English", "", "Klingon"})
	if len(got) != 3 {
		t.Fatalf("parsed %d languages: %+v", len(got), got)
	}
	if got[0].Name != "English" || got[0].Code != "en" {
		t.Errorf("first = %+v", got[0])
	}
	if got[1].Name != "russian" || got[1].Code != "ru" {
		t.Errorf("second = %+v", got[1])
	}
	// A language the engine has no tag for is still kept: the words get
	// written in it and sung without a hint.
	if got[2].Name != "Klingon" || got[2].Engine() {
		t.Errorf("third should be kept without an engine tag: %+v", got[2])
	}
}

func TestParseLanguagesIsBounded(t *testing.T) {
	var names []string
	for i := 0; i < maxLanguages+10; i++ {
		names = append(names, strings.Repeat("x", i%9+1)+string(rune('a'+i%26))+string(rune('0'+i%10)))
	}
	if got := len(ParseLanguages(names)); got != maxLanguages {
		t.Errorf("kept %d languages; want the %d cap", got, maxLanguages)
	}
}

func TestEnabledLanguagesDefaultsToOn(t *testing.T) {
	cat := ParseLanguages([]string{"English", "Russian", "French"})
	// A language the session says nothing about is on, so adding one to
	// the configuration starts using it straight away.
	if got := EnabledLanguages(cat, nil); len(got) != 3 {
		t.Fatalf("unset session should sing in all three, got %+v", got)
	}
	got := EnabledLanguages(cat, map[string]bool{"Russian": false, "English": true})
	if len(got) != 2 || got[0].Name != "English" || got[1].Name != "French" {
		t.Fatalf("switched-off language still enabled: %+v", got)
	}
	off := EnabledLanguages(cat, map[string]bool{"English": false, "Russian": false, "French": false})
	if len(off) != 0 {
		t.Fatalf("all switched off should leave nothing: %+v", off)
	}
}

func TestPickLanguageDrawsAtRandom(t *testing.T) {
	cat := ParseLanguages([]string{"English", "Russian", "French", "Bisaya (Cebuano)"})
	seen := map[string]int{}
	repeat := false
	prev := ""
	for i := 0; i < 400; i++ {
		l, ok := pickLanguage(cat)
		if !ok {
			t.Fatalf("draw %d: no language", i)
		}
		seen[l.Name]++
		if l.Name == prev {
			repeat = true
		}
		prev = l.Name
	}
	for _, l := range cat {
		if seen[l.Name] == 0 {
			t.Errorf("%s was never drawn in 400 tries", l.Name)
		}
	}
	// Drawing at random is what was asked for, repeats included: a draw
	// that never repeats is a rotation wearing a disguise.
	if !repeat {
		t.Error("400 draws from four languages never repeated one back to back")
	}
	// One language is the only answer, and nothing enabled hands the
	// choice back to the engine.
	one := ParseLanguages([]string{"Russian"})
	if l, ok := pickLanguage(one); !ok || l.Name != "Russian" {
		t.Errorf("single-language draw = %q, %v", l.Name, ok)
	}
	if _, ok := pickLanguage(nil); ok {
		t.Error("an empty enabled set must not produce a language")
	}
}

func TestBuildSpecDrawsAConfiguredLanguagePerTrack(t *testing.T) {
	b := NewBuilder(nil, nil)
	b.SetLanguages([]string{"English", "Russian", "Bisaya (Cebuano)"})
	s := session.New()
	s.BasePrompt = "island pop"
	s.Vocal = true

	seen := map[string]int{}
	for i := 0; i < 300; i++ {
		spec := b.BuildSpec(context.Background(), s, 150)
		seen[spec.VocalLanguage]++
	}
	// Every configured language turns up and nothing else does.
	for _, code := range []string{"en", "ru", "ceb"} {
		if seen[code] == 0 {
			t.Errorf("language %q was never drawn in 300 tracks", code)
		}
	}
	if len(seen) != 3 {
		t.Fatalf("drew languages outside the list: %+v", seen)
	}
}

func TestCebuanoAsksForCebuano(t *testing.T) {
	b := NewBuilder(nil, nil)
	b.SetLanguages([]string{"Bisaya (Cebuano)"})
	s := session.New()
	s.BasePrompt = "island pop"
	s.Vocal = true
	spec := b.BuildSpec(context.Background(), s, 150)
	// The engine's published list has no Cebuano, but it never checks a
	// requested tag against that list, and the model behind it does
	// know the language. Asking for the nearest listed language instead
	// gets that language's words, not a Cebuano accent.
	if spec.VocalLanguage != "ceb" {
		t.Fatalf("engine tag = %q; want Cebuano's own tag", spec.VocalLanguage)
	}
	if spec.VocalLanguageName != "Bisaya (Cebuano)" {
		t.Fatalf("the track must be recorded as Cebuano; got %q", spec.VocalLanguageName)
	}
	if !strings.Contains(spec.SampleQuery, "sung in Bisaya (Cebuano)") {
		t.Fatalf("the query must also ask for Cebuano words: %q", spec.SampleQuery)
	}
	for _, name := range []string{"Cebuano", "Bisaya", "Binisaya", "Visayan"} {
		if got := LanguageCode(name); got != "ceb" {
			t.Errorf("LanguageCode(%q) = %q", name, got)
		}
	}
	// Tagalog keeps its own tag; the two are different languages.
	if got := LanguageCode("Tagalog"); got != "tl" {
		t.Errorf("Tagalog resolved to %q", got)
	}
}

func TestPresetLanguageDoesNotOverrideTheList(t *testing.T) {
	// Several presets carry vocal_language "en" of their own. That is a
	// default, not a choice the listener made, so a configured list
	// must still be heard - otherwise editing it does nothing at all.
	b := NewBuilder(nil, nil)
	b.SetLanguages([]string{"Russian", "French", "Japanese"})
	s := session.New()
	s.BasePrompt = "hard rock"
	s.Vocal = true
	s.Spec = &session.PromptSpec{VocalLanguage: "en"}

	seen := map[string]int{}
	for i := 0; i < 60; i++ {
		seen[b.BuildSpec(context.Background(), s, 150).VocalLanguage]++
	}
	if seen["en"] > 0 {
		t.Errorf("a preset language outranked the configured list: %+v", seen)
	}
	for _, code := range []string{"ru", "fr", "ja"} {
		if seen[code] == 0 {
			t.Errorf("language %q was never drawn in 60 tracks: %+v", code, seen)
		}
	}
}

func TestBuildSpecCarriesTheLanguageName(t *testing.T) {
	b := NewBuilder(nil, nil)
	b.SetLanguages([]string{"Bisaya (Cebuano)"})
	s := session.New()
	s.BasePrompt = "island pop"
	s.Vocal = true
	spec := b.BuildSpec(context.Background(), s, 150)
	// The engine has no tag for it, so the readable name is the only
	// record of what the track was sung in.
	if spec.VocalLanguageName != "Bisaya (Cebuano)" {
		t.Fatalf("language name = %q", spec.VocalLanguageName)
	}
}

func TestBuildSpecNamesTheLanguageForTheEnginePlanner(t *testing.T) {
	b := NewBuilder(nil, nil)
	b.SetLanguages([]string{"Bisaya (Cebuano)"})
	s := session.New()
	s.BasePrompt = "island pop"
	s.Vocal = true
	spec := b.BuildSpec(context.Background(), s, 150)
	// With no lyrics ready the engine plans them from the query, so the
	// language has to be said in words there too.
	if !strings.Contains(spec.SampleQuery, "sung in Bisaya (Cebuano)") {
		t.Fatalf("sample query lost the language: %q", spec.SampleQuery)
	}
}

func TestBuildSpecLeavesTheLanguageAloneWhenNoneConfigured(t *testing.T) {
	b := NewBuilder(nil, nil)
	s := session.New()
	s.BasePrompt = "nu-metal"
	s.Vocal = true
	spec := b.BuildSpec(context.Background(), s, 150)
	if spec.VocalLanguage != "" {
		t.Errorf("with nothing configured the engine chooses; got %q", spec.VocalLanguage)
	}
	if strings.Contains(spec.SampleQuery, "sung in") {
		t.Errorf("query should not name a language: %q", spec.SampleQuery)
	}
}

func TestSteeredLanguageOutranksTheCatalogue(t *testing.T) {
	b := NewBuilder(nil, nil)
	b.SetLanguages([]string{"Russian"})
	s := session.New()
	s.BasePrompt = "chanson"
	s.Vocal = true
	Steer(s, "vocals in french")
	spec := b.BuildSpec(context.Background(), s, 150)
	if spec.VocalLanguage != "fr" {
		t.Fatalf("a hand-steered language must pin the track; got %q", spec.VocalLanguage)
	}
}

func TestSessionOffSwitchesSilenceALanguage(t *testing.T) {
	b := NewBuilder(nil, nil)
	b.SetLanguages([]string{"English", "Russian"})
	s := session.New()
	s.BasePrompt = "punk"
	s.Vocal = true
	s.Languages = map[string]bool{"Russian": false}
	for i := 0; i < 6; i++ {
		if got := b.BuildSpec(context.Background(), s, 150).VocalLanguage; got != "en" {
			t.Fatalf("draw %d = %q; the only language left on is English", i, got)
		}
	}
}
