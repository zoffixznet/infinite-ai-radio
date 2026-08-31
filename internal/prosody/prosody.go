// Package prosody answers phonetic questions about English words and
// lyric lines: syllable counts, lexical stress, rhyme keys and
// vocabulary membership. It embeds the CMU Pronouncing Dictionary
// (Carnegie Mellon University, BSD-licensed) and a frequency-ranked
// common-word list, so every check is deterministic and offline.
package prosody

import (
	_ "embed"
	"strings"
	"sync"
)

//go:embed data/cmudict-trim.txt
var cmudictRaw string

//go:embed data/commonwords.txt
var commonRaw string

// vocables are singable non-words lyric writers legitimately use.
var vocables = map[string]bool{
	"oh": true, "ooh": true, "oooh": true, "ah": true, "aah": true,
	"yeah": true, "hey": true, "na": true, "la": true, "whoa": true,
	"woah": true, "mm": true, "mmm": true, "hmm": true, "uh": true,
	"ay": true, "oo": true, "da": true, "ba": true, "sha": true,
}

var (
	once   sync.Once
	prons  map[string]string // word -> space-joined ARPABET phones
	common map[string]bool   // top-frequency words
)

func load() {
	once.Do(func() {
		prons = make(map[string]string, 130000)
		for _, line := range strings.Split(cmudictRaw, "\n") {
			if i := strings.IndexByte(line, ' '); i > 0 {
				prons[line[:i]] = line[i+1:]
			}
		}
		common = make(map[string]bool, 21000)
		for _, w := range strings.Split(commonRaw, "\n") {
			if w != "" {
				common[w] = true
			}
		}
	})
}

// Normalize lowercases a token and strips surrounding punctuation and a
// trailing possessive, returning the bare word ("" when nothing
// word-like remains).
func Normalize(token string) string {
	w := strings.ToLower(strings.TrimSpace(token))
	w = strings.TrimFunc(w, func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && r != '\''
	})
	w = strings.Trim(w, "'")
	w = strings.TrimSuffix(w, "'s")
	return w
}

// Words splits a lyric line into normalized words.
func Words(line string) []string {
	var out []string
	for _, tok := range strings.Fields(line) {
		if w := Normalize(tok); w != "" {
			out = append(out, w)
		}
	}
	return out
}

// Known reports whether the word (or an obvious inflection of it) has a
// dictionary pronunciation or is an accepted vocable.
func Known(word string) bool {
	_, ok := phones(word)
	return ok || vocables[word]
}

// Common reports whether the word is within the embedded top-frequency
// vocabulary (or a vocable). Inflections of a common base word count.
func Common(word string) bool {
	load()
	if common[word] || vocables[word] {
		return true
	}
	for _, base := range inflectionBases(word) {
		if common[base] {
			return true
		}
	}
	return false
}

// phones returns the ARPABET phones for a word, trying obvious
// inflections when the exact form is missing.
func phones(word string) ([]string, bool) {
	load()
	if p, ok := prons[word]; ok {
		return strings.Fields(p), true
	}
	for _, base := range inflectionBases(word) {
		if p, ok := prons[base]; ok {
			ph := strings.Fields(p)
			switch {
			case strings.HasSuffix(word, "s") && !strings.HasSuffix(base, "s"):
				ph = append(ph, "Z")
			case strings.HasSuffix(word, "ing"):
				ph = append(ph, "IH0", "NG")
			}
			return ph, true
		}
	}
	return nil, false
}

// inflectionBases proposes dictionary base forms for a surface word.
func inflectionBases(word string) []string {
	var out []string
	add := func(w string) {
		if len(w) >= 2 {
			out = append(out, w)
		}
	}
	if strings.HasSuffix(word, "s") {
		add(strings.TrimSuffix(word, "s"))
		add(strings.TrimSuffix(word, "es"))
	}
	if strings.HasSuffix(word, "ing") {
		add(strings.TrimSuffix(word, "ing"))
		add(strings.TrimSuffix(word, "ing") + "e")
	}
	if strings.HasSuffix(word, "ed") {
		add(strings.TrimSuffix(word, "ed"))
		add(strings.TrimSuffix(word, "d"))
	}
	if strings.HasSuffix(word, "in'") {
		add(strings.TrimSuffix(word, "in'") + "ing")
	}
	return out
}

// isVowel reports whether an ARPABET phone is a vowel (they carry the
// stress digit).
func isVowel(ph string) bool {
	return len(ph) > 0 && ph[len(ph)-1] >= '0' && ph[len(ph)-1] <= '2'
}

// Syllables counts a word's syllables. Dictionary words count vowel
// phones; unknown words fall back to a vowel-group heuristic. ok is
// false for the heuristic path.
func Syllables(word string) (n int, ok bool) {
	if ph, found := phones(word); found {
		for _, p := range ph {
			if isVowel(p) {
				n++
			}
		}
		if n == 0 {
			n = 1
		}
		return n, true
	}
	return heuristicSyllables(word), vocables[word]
}

// heuristicSyllables counts vowel-letter groups.
func heuristicSyllables(word string) int {
	n := 0
	prev := false
	for _, r := range word {
		v := strings.ContainsRune("aeiouy", r)
		if v && !prev {
			n++
		}
		prev = v
	}
	if strings.HasSuffix(word, "e") && !strings.HasSuffix(word, "le") && n > 1 {
		n--
	}
	if n == 0 {
		n = 1
	}
	return n
}

// LineSyllables counts a line's syllables and reports the words with no
// dictionary pronunciation (vocables excluded).
func LineSyllables(line string) (n int, unknown []string) {
	for _, w := range Words(line) {
		s, ok := Syllables(w)
		n += s
		if !ok && !vocables[w] {
			unknown = append(unknown, w)
		}
	}
	return n, unknown
}

// RimeKey returns the word's rhyme identity: the phones from the last
// stressed vowel on, stress digits stripped ("" when unknown). Words
// sharing a RimeKey rhyme perfectly.
func RimeKey(word string) string {
	ph, ok := phones(word)
	if !ok {
		return ""
	}
	start := -1
	for i, p := range ph {
		if !isVowel(p) {
			continue
		}
		switch p[len(p)-1] {
		case '1', '2':
			start = i
		case '0':
			if start == -1 {
				start = i
			}
		}
	}
	if start == -1 {
		return ""
	}
	var b strings.Builder
	for _, p := range ph[start:] {
		if isVowel(p) {
			p = p[:len(p)-1]
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(p)
	}
	return b.String()
}

// Nucleus returns the vowel of the word's last stressed syllable
// (stress digit stripped), the sound end-rhymes are heard on.
func Nucleus(word string) string {
	key := RimeKey(word)
	if key == "" {
		return ""
	}
	if i := strings.IndexByte(key, ' '); i > 0 {
		return key[:i]
	}
	return key
}

// Rhymes reports whether two words rhyme at least by assonance (same
// final stressed nucleus). Perfect rhyme also matches; unknown words
// never rhyme.
func Rhymes(a, b string) bool {
	na, nb := Nucleus(a), Nucleus(b)
	return na != "" && na == nb
}

// PerfectRhyme reports whether two different words share a full rime.
func PerfectRhyme(a, b string) bool {
	ka, kb := RimeKey(a), RimeKey(b)
	return a != b && ka != "" && ka == kb
}

// EndWord returns the last word of a lyric line ("" for tag or empty
// lines).
func EndWord(line string) string {
	ws := Words(line)
	if len(ws) == 0 {
		return ""
	}
	return ws[len(ws)-1]
}

// functionWords are weak line-enders (articles, prepositions, …).
var functionWords = map[string]bool{
	"the": true, "a": true, "an": true, "of": true, "and": true,
	"to": true, "in": true, "it": true, "is": true, "for": true,
	"but": true, "or": true, "at": true, "by": true, "on": true,
	"with": true, "as": true, "if": true, "than": true, "so": true,
	"was": true, "are": true, "be": true, "am": true, "my": true,
	"your": true, "that": true, "this": true,
}

// FunctionWord reports whether a word is a weak function word.
func FunctionWord(w string) bool { return functionWords[w] }

// StopWord reports words that carry no topical content (function words
// plus common pronouns and auxiliaries); used for topic matching.
var stopWords = map[string]bool{
	"i": true, "you": true, "he": true, "she": true, "we": true,
	"they": true, "me": true, "him": true, "her": true, "us": true,
	"them": true, "will": true, "would": true, "can": true,
	"could": true, "have": true, "has": true, "had": true, "do": true,
	"does": true, "did": true, "not": true, "no": true, "yes": true,
	"there": true, "here": true, "when": true, "what": true,
	"who": true, "how": true, "all": true, "some": true, "just": true,
	"about": true, "into": true, "up": true, "down": true, "out": true,
	"go": true, "going": true, "get": true, "got": true, "like": true,
	"very": true, "really": true, "positive": true, "song": true,
	"lyrics": true, "vocals": true, "music": true,
}

// ContentWords returns the normalized non-stopword, non-function words
// of a text, deduplicated in order.
func ContentWords(text string) []string {
	seen := map[string]bool{}
	var out []string
	for _, w := range Words(text) {
		if functionWords[w] || stopWords[w] || seen[w] || len(w) < 2 {
			continue
		}
		seen[w] = true
		out = append(out, w)
	}
	return out
}

// Stem crudely reduces a word to a comparable base (plural and -ing/-ed
// stripped, doubled final consonant collapsed) for topic matching, so
// "shopping" and "shops" meet at "shop".
func Stem(w string) string {
	for _, suf := range []string{"ing", "ed", "es", "s"} {
		if !strings.HasSuffix(w, suf) || len(w)-len(suf) < 3 {
			continue
		}
		w = strings.TrimSuffix(w, suf)
		// English doubles the final consonant before -ing/-ed
		// ("shopping"); collapse it so "shopping" and "shops" meet
		// at "shop". Plain plurals ("malls") never doubled.
		if suf == "ing" || suf == "ed" {
			if n := len(w); n >= 4 && w[n-1] == w[n-2] && !strings.ContainsRune("aeiou", rune(w[n-1])) {
				w = w[:n-1]
			}
		}
		break
	}
	return w
}

// Vocable reports whether a word is a singable filler sound ("la",
// "ooh") rather than a real word.
func Vocable(w string) bool { return vocables[w] }
