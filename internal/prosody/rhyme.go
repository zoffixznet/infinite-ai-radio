package prosody

import (
	"sort"
	"strings"
	"sync"
)

// This file finds rhyme candidates: common words sharing a rime or
// nucleus with a seed word, for handing a lyric-writing model concrete
// end-word options instead of asking it to invent rhymes.

// familyCoda groups final consonants into family-rhyme sets (plosives,
// fricatives, nasals). Swapping the coda within a set keeps the rhyme's
// feel while escaping cliché pairs.
var familyCoda = map[string]int{
	"P": 1, "B": 1, "T": 1, "D": 1, "K": 1, "G": 1,
	"F": 2, "V": 2, "TH": 2, "DH": 2, "S": 2, "Z": 2, "SH": 2, "ZH": 2,
	"M": 3, "N": 3, "NG": 3,
}

// RhymeSet is a seed word's usable rhyme candidates.
type RhymeSet struct {
	Seed string
	// Perfect share the full rime; Family swap the final consonant
	// within its articulation family; Assonance share only the vowel.
	Perfect   []string
	Family    []string
	Assonance []string
}

// Candidates flattens the set into one mixed list capped at n: perfect
// and family rhymes interleaved (a mixed diet reads less nursery-rhyme
// than all-perfect), assonance as filler.
func (rs RhymeSet) Candidates(n int) []string {
	var out []string
	p, f := rs.Perfect, rs.Family
	for len(out) < n && (len(p) > 0 || len(f) > 0) {
		if len(p) > 0 {
			out, p = append(out, p[0]), p[1:]
		}
		if len(out) < n && len(f) > 0 {
			out, f = append(out, f[0]), f[1:]
		}
	}
	for _, w := range rs.Assonance {
		if len(out) >= n {
			break
		}
		out = append(out, w)
	}
	return out
}

var (
	rhymeOnce sync.Once
	// byNucleus maps a stressed-vowel nucleus to the common words that
	// end on it (1-3 syllables), the candidate pool for rhyming.
	byNucleus map[string][]string
)

func buildRhymeIndex() {
	rhymeOnce.Do(func() {
		load()
		byNucleus = map[string][]string{}
		for w := range common {
			if strings.ContainsRune(w, '\'') {
				continue
			}
			if n, ok := Syllables(w); !ok || n > 3 {
				continue
			}
			if nuc := Nucleus(w); nuc != "" {
				byNucleus[nuc] = append(byNucleus[nuc], w)
			}
		}
		for _, list := range byNucleus {
			sort.Strings(list)
		}
	})
}

// codaFamily returns the family class of a rime's final consonant
// (0 when it ends on the vowel).
func codaFamily(rimeKey string) int {
	phs := strings.Fields(rimeKey)
	if len(phs) == 0 {
		return 0
	}
	return familyCoda[phs[len(phs)-1]]
}

// FindRhymes collects rhyme candidates for a seed word among the common
// vocabulary, excluding the seed itself and anything in used. Empty
// result when the seed has no known pronunciation.
func FindRhymes(seed string, used map[string]bool) RhymeSet {
	buildRhymeIndex()
	rs := RhymeSet{Seed: seed}
	seedKey := RimeKey(seed)
	nuc := Nucleus(seed)
	if nuc == "" {
		return rs
	}
	seedFam := codaFamily(seedKey)
	for _, w := range byNucleus[nuc] {
		if w == seed || used[w] || Stem(w) == Stem(seed) {
			continue
		}
		key := RimeKey(w)
		switch {
		case key == seedKey:
			rs.Perfect = append(rs.Perfect, w)
		case seedFam != 0 && codaFamily(key) == seedFam && sameRimeShape(seedKey, key):
			rs.Family = append(rs.Family, w)
		default:
			rs.Assonance = append(rs.Assonance, w)
		}
	}
	return rs
}

// sameRimeShape reports whether two rimes have the same phone count, so
// a family swap replaces the coda rather than restructuring the rime.
func sameRimeShape(a, b string) bool {
	return len(strings.Fields(a)) == len(strings.Fields(b))
}
