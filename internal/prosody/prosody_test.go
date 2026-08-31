package prosody

import (
	"strings"
	"testing"
)

func TestSyllables(t *testing.T) {
	cases := []struct {
		word string
		want int
	}{
		{"day", 1}, {"sunlight", 2}, {"morning", 2}, {"beautiful", 3},
		{"escalator", 4}, {"cat", 1}, {"fire", 2},
	}
	for _, c := range cases {
		got, ok := Syllables(c.word)
		if !ok || got != c.want {
			t.Errorf("Syllables(%q) = %d,%v want %d", c.word, got, ok, c.want)
		}
	}
}

func TestSyllablesInflections(t *testing.T) {
	// Surface forms that may miss the dictionary still resolve via
	// their base word.
	for _, w := range []string{"escalators", "smiling", "walked"} {
		if n, _ := Syllables(w); n < 1 {
			t.Errorf("Syllables(%q) = %d", w, n)
		}
		if !Known(w) {
			t.Errorf("Known(%q) = false", w)
		}
	}
}

func TestLineSyllablesFlagsGibberish(t *testing.T) {
	n, unknown := LineSyllables("My name is Mandu doo doo")
	if n < 6 {
		t.Errorf("syllable count = %d", n)
	}
	found := false
	for _, w := range unknown {
		if w == "mandu" {
			found = true
		}
	}
	if !found {
		t.Errorf("gibberish not flagged: %v", unknown)
	}
	// Vocables are not gibberish.
	if _, unk := LineSyllables("oh yeah la la whoa"); len(unk) != 0 {
		t.Errorf("vocables flagged: %v", unk)
	}
}

func TestRimeAndRhyme(t *testing.T) {
	if RimeKey("book") != RimeKey("look") {
		t.Error("book/look must share a rime")
	}
	if RimeKey("book") == RimeKey("day") {
		t.Error("book/day must not share a rime")
	}
	if !PerfectRhyme("book", "look") {
		t.Error("book/look perfect rhyme")
	}
	if PerfectRhyme("book", "book") {
		t.Error("a word does not rhyme with itself")
	}
	// Family rhyme: same nucleus, coda swapped in-family (assonance
	// at minimum must hold).
	if !Rhymes("cat", "bad") {
		t.Error("cat/bad assonance expected")
	}
	if Nucleus("day") != Nucleus("way") {
		t.Error("day/way nucleus")
	}
}

func TestEndWordAndFunctionWords(t *testing.T) {
	if got := EndWord("I put my coat on a hook,"); got != "hook" {
		t.Errorf("EndWord = %q", got)
	}
	if EndWord("[Chorus]") != "chorus" {
		// Tag lines are filtered by the caller; EndWord just reads words.
		t.Errorf("EndWord tag = %q", EndWord("[Chorus]"))
	}
	if !FunctionWord("the") || FunctionWord("mall") {
		t.Error("function word classification")
	}
}

func TestContentWords(t *testing.T) {
	got := ContentWords("a positive song about going to the mall to do some shopping")
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "mall") || !strings.Contains(joined, "shopping") {
		t.Errorf("content words = %v", got)
	}
	for _, w := range got {
		if w == "song" || w == "positive" || w == "the" {
			t.Errorf("stopword leaked: %v", got)
		}
	}
}

func TestCommonVocabulary(t *testing.T) {
	for _, w := range []string{"day", "mall", "shopping", "sunshine"} {
		if !Common(w) {
			t.Errorf("Common(%q) = false", w)
		}
	}
	for _, w := range []string{"mandu", "xylophonic"} {
		if Common(w) {
			t.Errorf("Common(%q) = true", w)
		}
	}
}

func TestFindRhymes(t *testing.T) {
	rs := FindRhymes("day", map[string]bool{"way": true})
	cands := rs.Candidates(8)
	if len(cands) < 4 {
		t.Fatalf("too few candidates for day: %v", cands)
	}
	for _, w := range cands {
		if w == "way" {
			t.Error("excluded word returned")
		}
		if w == "days" {
			t.Error("same-stem word returned")
		}
		if !Rhymes("day", w) {
			t.Errorf("%q does not rhyme with day", w)
		}
	}
	// Perfect candidates share the full rime.
	for _, w := range rs.Perfect {
		if !PerfectRhyme("day", w) {
			t.Errorf("%q not a perfect rhyme of day", w)
		}
	}
	if got := FindRhymes("mandu", nil); len(got.Candidates(5)) != 0 {
		t.Errorf("unknown seed produced candidates: %v", got)
	}
}

func TestStem(t *testing.T) {
	if Stem("shopping") != "shop" {
		t.Errorf("Stem(shopping) = %q", Stem("shopping"))
	}
	if Stem("shops") != "shop" {
		t.Errorf("Stem(shops) = %q", Stem("shops"))
	}
	if Stem("malls") != "mall" {
		t.Errorf("Stem(malls) = %q", Stem("malls"))
	}
}
