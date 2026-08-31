package prompting

// This file is Scribe's deterministic song architecture: the section
// skeleton for a given duration, and each section's rhyme scheme. Go
// owns structure and chorus repetition outright, so the language model
// can never loop a phrase across the song: it only ever writes one
// short section at a time and every chorus repeat is stamped verbatim
// by code. Line and word budgets follow the densities measured over
// ACE-Step 1.5's own example corpus (~17 sung lines and ~93 words per
// 150 seconds, 6-10 syllables per line).

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

// scribeForm picks the song skeleton for a track duration. Sung-line
// counts target the engine's measured density (17 lines per 150 s)
// while keeping the chorus recurring and the bridge reserved for
// longer tracks.
func scribeForm(seconds int) []scribeSection {
	verse := func(n int) scribeSection {
		return scribeSection{Tag: "[Verse " + string(rune('0'+n)) + "]", Kind: "verse", Lines: 4, CopyOf: -1}
	}
	chorus := scribeSection{Tag: "[Chorus]", Kind: "chorus", Lines: 4, CopyOf: -1}
	switch {
	case seconds < 110:
		// 8 sung lines.
		return []scribeSection{verse(1), chorus}
	case seconds < 140:
		// 16 sung lines, chorus twice.
		return []scribeSection{verse(1), chorus, verse(2), {Tag: "[Chorus]", Kind: "chorus", Lines: 4, CopyOf: 1}}
	case seconds < 170:
		// 18 sung lines: two verses, a two-line bridge, chorus twice.
		return []scribeSection{
			verse(1), chorus, verse(2),
			{Tag: "[Bridge]", Kind: "bridge", Lines: 2, CopyOf: -1},
			{Tag: "[Chorus]", Kind: "chorus", Lines: 4, CopyOf: 1},
		}
	default:
		// 22 sung lines: chorus three times, closing as a final chorus.
		return []scribeSection{
			verse(1), chorus, verse(2),
			{Tag: "[Chorus]", Kind: "chorus", Lines: 4, CopyOf: 1},
			{Tag: "[Bridge]", Kind: "bridge", Lines: 2, CopyOf: -1},
			{Tag: "[Final Chorus]", Kind: "chorus", Lines: 4, CopyOf: 1},
		}
	}
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
