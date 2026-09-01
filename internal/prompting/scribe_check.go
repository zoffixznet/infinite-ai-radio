package prompting

import (
	"fmt"
	"slices"
	"strings"

	"iar/internal/prosody"
)

// This file is Scribe's deterministic quality gate. Research on small
// language models is unambiguous: they cannot reliably judge or fix
// their own writing, but they respond well to a regeneration request
// carrying specific, numeric violations. So every rule lives here as
// plain code over the CMU pronouncing dictionary, and the model only
// ever sees the resulting messages.

// scribeCliches always reject a line. Seeded from the community lists
// of words AI lyrics overuse.
var scribeCliches = []string{
	"neon", "embers", "kaleidoscope", "tapestry", "twilight",
	"symphony of", "dance of", "shimmering", "echoes", "whispers",
	"heart on my sleeve", "moth to a flame", "time stands still",
	"stars in your eyes", "lost in the moment", "forever and always",
}

// scribeBannedRhymes are rhyme pairs so worn they mark a lyric as
// machine-written; either order rejects.
var scribeBannedRhymes = map[string]string{
	"fire": "desire", "heart": "apart", "rain": "pain", "true": "blue",
	"love": "above", "night": "light", "cry": "why", "sky": "high",
	"dream": "seem", "tears": "years", "arms": "charms",
	"forever": "together", "knees": "please",
}

func bannedRhymePair(a, b string) bool {
	return scribeBannedRhymes[a] == b || scribeBannedRhymes[b] == a
}

// checkResult grades one section attempt. Hard violations force a
// rewrite; soft ones only lower the score used to pick the best
// attempt.
type checkResult struct {
	hard []string
	soft []string
	// wrongShape marks a section with the wrong line count; it must
	// never win best-attempt selection over a complete section.
	wrongShape bool
}

func (c checkResult) penalty() int {
	p := len(c.hard)*10 + len(c.soft)
	if c.wrongShape {
		p += 1 << 20
	}
	return p
}
func (c checkResult) ok() bool { return len(c.hard) == 0 }

// problems renders the violations as rewrite instructions.
func (c checkResult) problems() string {
	var b strings.Builder
	for _, m := range append(append([]string{}, c.hard...), c.soft...) {
		b.WriteString("- " + m + "\n")
	}
	return b.String()
}

// sectionSpec is everything checkSection needs to grade an attempt.
type sectionSpec struct {
	kind  string
	lines int
	// pairs lists line pairs that must rhyme; allowed constrains those
	// lines' end words when non-empty.
	pairs   []rhymePair
	allowed map[int][]string
	// fixed pins a line's exact text (the chorus hook).
	fixed map[int]string
	// mentions: at least one must appear somewhere in the section.
	mentions []string
	// avoidWords: content words that must NOT appear (verse-2 overlap
	// guard).
	avoidWords []string
	// avoidLines: lines already written elsewhere; near-copies reject.
	avoidLines []string
}

// checkSection grades a section attempt against its spec and the end
// words already spent elsewhere in the song.
func checkSection(lines []string, spec sectionSpec, usedEnds map[string]bool) checkResult {
	var res checkResult
	hard := func(f string, a ...any) { res.hard = append(res.hard, fmt.Sprintf(f, a...)) }
	soft := func(f string, a ...any) { res.soft = append(res.soft, fmt.Sprintf(f, a...)) }

	if len(lines) != spec.lines {
		res.wrongShape = true
		hard("write exactly %d lines (got %d)", spec.lines, len(lines))
		return res
	}
	ends := make([]string, len(lines))
	prevSyl := -1
	for i, line := range lines {
		ln := i + 1
		if fixed, ok := spec.fixed[i]; ok {
			if !strings.EqualFold(strings.TrimSpace(line), strings.TrimSpace(fixed)) {
				hard("line %d must be exactly: %s", ln, fixed)
			}
			ends[i] = prosody.EndWord(fixed)
			prevSyl, _ = prosody.LineSyllables(fixed)
			continue
		}
		syl, unknown := prosody.LineSyllables(line)
		if len(unknown) > 0 {
			hard("line %d uses made-up or obscure words (%s); use plain real words", ln, strings.Join(unknown, ", "))
		}
		switch {
		case syl > scribeSyllableHard || syl < scribeSyllableMin-2:
			hard("line %d has %d syllables; write %d-%d", ln, syl, scribeSyllableMin, scribeSyllableMax)
		case syl > scribeSyllableMax || syl < scribeSyllableMin:
			soft("line %d has %d syllables; %d-%d sings better", ln, syl, scribeSyllableMin, scribeSyllableMax)
		}
		if prevSyl >= 0 && abs(syl-prevSyl) > 3 {
			soft("lines %d and %d differ by %d syllables; keep neighbours within 3", ln-1, ln, abs(syl-prevSyl))
		}
		prevSyl = syl
		longWords := 0
		for _, w := range prosody.Words(line) {
			switch n, _ := prosody.Syllables(w); {
			case n >= 5:
				hard("line %d: %q has %d syllables; use shorter words", ln, w, n)
			case n >= 3:
				longWords++
			}
		}
		if longWords > 1 {
			soft("line %d has several long words; keep most words to 1-2 syllables", ln)
		}
		lower := strings.ToLower(line)
		for _, c := range scribeCliches {
			if strings.Contains(lower, c) {
				hard("line %d uses the cliché %q; replace it with something concrete from the song's setting", ln, c)
			}
		}
		if strings.ContainsAny(line, "0123456789") {
			hard("line %d contains digits; spell numbers out as words", ln)
		}
		if chantLine(line) {
			hard("line %d carries no words (filler sounds, or one word repeated); write real lyrics", ln)
		}
		if ws := prosody.Words(line); len(ws) > 0 && strings.HasPrefix(ws[0], "n") {
			soft("start line %d with a different word (the singer garbles lines that start with n)", ln)
		}
		for i, tok := range strings.Fields(line) {
			if i == 0 || tok == "I" || tok == "I'm" || tok == "I'll" || tok == "I've" || tok == "I'd" {
				continue
			}
			r := []rune(tok)
			if r[0] >= 'A' && r[0] <= 'Z' && !prosody.Common(prosody.Normalize(tok)) {
				soft("line %d names %q; avoid names and places not in the topic", ln, tok)
			}
		}
		ends[i] = prosody.EndWord(line)
	}

	// End words: no reuse inside the section or across the song.
	seen := map[string]int{}
	for i, e := range ends {
		if e == "" {
			continue
		}
		if j, dup := seen[e]; dup {
			hard("lines %d and %d both end on %q; end every line on a different word", j+1, i+1, e)
		}
		seen[e] = i
		if _, ok := spec.fixed[i]; ok {
			continue
		}
		if usedEnds[e] {
			hard("line %d ends on %q, already used earlier in the song; pick another end word", i+1, e)
		}
		if prosody.FunctionWord(e) {
			soft("line %d ends on the weak word %q; end on a concrete word instead", i+1, e)
		}
	}

	// Rhyme scheme: required pairs rhyme (and stay off the banned
	// list); everything else must NOT rhyme with a required pair. A
	// line that rhymes on a word outside its suggested menu is fine -
	// the menu is a means, the rhyme is the rule.
	inPair := map[int]bool{}
	for _, p := range spec.pairs {
		inPair[p[0]], inPair[p[1]] = true, true
		a, b := ends[p[0]], ends[p[1]]
		if a == "" || b == "" {
			continue
		}
		if bannedRhymePair(a, b) {
			hard("the rhyme %s/%s is worn out; use a fresher pair", a, b)
			continue
		}
		if identicalRhyme(a, b) {
			hard("%s/%s is the same word rhymed with itself; use different words", a, b)
			continue
		}
		if prosody.Rhymes(a, b) || !prosody.Known(a) || !prosody.Known(b) {
			continue
		}
		for _, li := range []int{p[0], p[1]} {
			if list := spec.allowed[li]; len(list) > 0 {
				if _, isFixed := spec.fixed[li]; !isFixed && !slices.Contains(list, ends[li]) {
					hard("line %d must end with one of: %s", li+1, strings.Join(list, ", "))
				}
			}
		}
		hard("lines %d and %d must rhyme (line %d ends on %q, line %d on %q)", p[0]+1, p[1]+1, p[0]+1, a, p[1]+1, b)
	}
	for i, e := range ends {
		if inPair[i] || e == "" {
			continue
		}
		for _, p := range spec.pairs {
			if pe := ends[p[0]]; pe != "" && prosody.PerfectRhyme(e, pe) {
				hard("line %d must not rhyme with lines %d and %d; end it on a different sound", i+1, p[0]+1, p[1]+1)
				break
			}
		}
	}

	// Near-copies of lines already written elsewhere in the song.
	for i, line := range lines {
		if _, ok := spec.fixed[i]; ok {
			continue
		}
		for _, prev := range spec.avoidLines {
			if nearCopy(line, prev) {
				hard("line %d nearly repeats an earlier line (%q); write something new", i+1, prev)
				break
			}
		}
	}

	// Topic grounding.
	if len(spec.mentions) > 0 && !mentionsAny(lines, spec.mentions) {
		msg := "mention at least one of: " + strings.Join(spec.mentions, ", ")
		if spec.kind == "bridge" {
			soft("%s", msg)
		} else {
			hard("%s", msg)
		}
	}
	if len(spec.avoidWords) > 0 {
		avoid := map[string]bool{}
		for _, w := range spec.avoidWords {
			avoid[prosody.Stem(strings.ToLower(w))] = true
		}
		mine := prosody.ContentWords(strings.Join(lines, " "))
		reused := 0
		for _, w := range mine {
			if avoid[prosody.Stem(w)] {
				reused++
			}
		}
		if len(mine) > 0 && reused*100 > len(mine)*40 {
			soft("too much of this section reuses earlier images and words; bring in new details")
		}
	}
	return res
}

// chantLine reports a line that carries no words: either only singable
// filler ("doo doo doo") or one token repeated ("dumdam dumdam
// dumdam"). The second shape needs its own rule because the writer
// invents its own syllables and no fixed list of vocables can name them
// all - what gives it away is the repetition, and that reads the same
// in every language the radio sings in.
func chantLine(line string) bool {
	ws := prosody.Words(line)
	if len(ws) == 0 {
		return false
	}
	if allVocables(ws) {
		return true
	}
	// Two words repeated is a hook ("run, run"); three or more with a
	// single distinct token is filler wearing a word's clothes.
	if len(ws) < 3 {
		return false
	}
	for _, w := range ws[1:] {
		if w != ws[0] {
			return false
		}
	}
	return true
}

// allVocables reports words that are all singable filler ("doo doo
// doo") - the degenerate chant shape.
func allVocables(ws []string) bool {
	if len(ws) == 0 {
		return false
	}
	for _, w := range ws {
		if !prosody.Vocable(w) {
			return false
		}
	}
	return true
}

// identicalRhyme reports compound reuse of one word as its own rhyme
// (day/today, time/daytime): one word ends with the other after a
// prefix of two or more letters. A single-letter difference (all/mall)
// is an honest perfect rhyme.
func identicalRhyme(a, b string) bool {
	if len(a) < len(b) {
		a, b = b, a
	}
	return strings.HasSuffix(a, b) && len(a)-len(b) >= 2
}

// nearCopy reports whether two lines share their shape: identical
// normalized text or the same first four words.
func nearCopy(a, b string) bool {
	wa, wb := prosody.Words(a), prosody.Words(b)
	if len(wa) == 0 || len(wb) == 0 {
		return false
	}
	if strings.Join(wa, " ") == strings.Join(wb, " ") {
		return true
	}
	if len(wa) >= 4 && len(wb) >= 4 {
		return strings.Join(wa[:4], " ") == strings.Join(wb[:4], " ")
	}
	return false
}

// mentionsAny reports whether any of the words (stem-matched) appears
// in the lines.
func mentionsAny(lines []string, words []string) bool {
	stems := map[string]bool{}
	for _, line := range lines {
		for _, w := range prosody.Words(line) {
			stems[prosody.Stem(w)] = true
		}
	}
	for _, w := range words {
		if stems[prosody.Stem(strings.ToLower(w))] {
			return true
		}
	}
	return false
}

// checkSong grades the assembled sung lines as a whole (structure tags
// excluded). It reports observations only: by this point every section
// passed its own gate, and a radio must always ship a track. Chant is
// structurally impossible - Go stamps the chorus and nothing else
// repeats - so this is a tripwire for generator bugs, not a filter.
func checkSong(sungLines []string) []string {
	var problems []string
	if len(sungLines) == 0 {
		return []string{"no sung lines"}
	}
	run := 1
	for i := 1; i < len(sungLines); i++ {
		if strings.EqualFold(sungLines[i], sungLines[i-1]) {
			run++
			if run > 2 {
				problems = append(problems, "a line repeats more than twice in a row")
				break
			}
		} else {
			run = 1
		}
	}
	// No word-vs-duration budget: the song is complete on its own
	// terms and the track is sized to it afterwards, so there is no
	// duration to grade against. Per-line singability is enforced at
	// the section gates.
	return problems
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
