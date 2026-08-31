package prompting

// This file is Scribe's deterministic song architecture: the section
// skeleton for a planned song, and each section's rhyme scheme. Go owns
// structure and chorus repetition outright, so the language model can
// never loop a phrase across the song: it only ever writes one short
// section at a time and every chorus repeat is stamped verbatim by
// code. The skeleton follows the song's own plan - how many scenes
// (verses) its story needs, and whether it wants a turn (a bridge)
// before the close - never a target duration: the words are written
// first, complete on their own terms, and the track is sized to them
// afterwards by the engine. Section-level budgets keep the densities
// measured over ACE-Step 1.5's example corpus (4-line sections, 6-10
// syllables per line).

// scribeSection is one entry of the song skeleton.
type scribeSection struct {
	// Tag is the engine structure tag, e.g. "[Verse 1]". ACE-Step 1.5's
	// examples use Title Case tags almost exclusively.
	Tag string
	// Kind is "verse", "chorus" or "bridge".
	Kind string
	// Lines is how many sung lines the section carries.
	Lines int
	// CopyOf stamps an earlier section's text verbatim (index into the
	// form); -1 writes fresh text.
	CopyOf int
}

// scribeFormFromPlan builds the skeleton the song's plan asked for:
// verse/chorus alternation, an optional bridge before the close, and
// the chorus landing at least twice (stamped verbatim). A verse count
// outside 1-3 falls back to the standard two.
func scribeFormFromPlan(verses int, bridge bool) []scribeSection {
	verses = clampVerses(verses)
	verse := func(n int) scribeSection {
		return scribeSection{Tag: "[Verse " + string(rune('0'+n)) + "]", Kind: "verse", Lines: 4, CopyOf: -1}
	}
	const chorusAt = 1 // the written chorus always follows verse 1
	copyChorus := func(tag string) scribeSection {
		return scribeSection{Tag: tag, Kind: "chorus", Lines: 4, CopyOf: chorusAt}
	}
	form := []scribeSection{verse(1), {Tag: "[Chorus]", Kind: "chorus", Lines: 4, CopyOf: -1}}
	choruses := 1
	for n := 2; n <= verses; n++ {
		form = append(form, verse(n))
		// The chorus returns after every verse except the one right
		// before the bridge: the bridge supplies that return itself.
		if !(bridge && n == verses) {
			form = append(form, copyChorus("[Chorus]"))
			choruses++
		}
	}
	if bridge {
		form = append(form, scribeSection{Tag: "[Bridge]", Kind: "bridge", Lines: 2, CopyOf: -1})
	}
	// Close on the chorus when the bridge calls for its return or when
	// the alternation has not yet landed it twice.
	if bridge || choruses < 2 {
		tag := "[Chorus]"
		if choruses >= 2 {
			tag = "[Final Chorus]"
		}
		form = append(form, copyChorus(tag))
	}
	return form
}

// clampVerses folds a plan's verse count into the 1-3 range the craft
// machinery supports; anything else becomes the standard two.
func clampVerses(v int) int {
	if v < 1 || v > 3 {
		return 2
	}
	return v
}

// rhymePair is a pair of line indexes (0-based within a section) whose
// end words must rhyme.
type rhymePair [2]int

// sectionRhyme returns the rhyme constraints for a section kind:
// verses use ABCB (only lines 2 and 4 rhyme - the scheme sources single
// out for natural, uncontrived lines), choruses AABB couplets (the hook
// position where couplets belong), bridges are free.
func sectionRhyme(kind string, lines int) []rhymePair {
	switch kind {
	case "verse":
		if lines >= 4 {
			return []rhymePair{{1, 3}}
		}
	case "chorus":
		if lines >= 4 {
			return []rhymePair{{0, 1}, {2, 3}}
		}
	}
	return nil
}

// scribeSyllableMin/Max bound a singable line; the engine's own
// guidance is 6-10 syllables with 12+ fracturing the vocal, and
// adjacent lines drifting more than a couple of syllables apart trigger
// its skip/garble domino.
const (
	scribeSyllableMin  = 5
	scribeSyllableMax  = 10
	scribeSyllableHard = 12
)
