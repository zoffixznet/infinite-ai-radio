package prompting

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"iar/internal/session"
)

// This file implements structured steering: free-text inputs become
// mutations of the session's PromptSpec ("less guitars" removes guitars
// from every positive slot and adds them to Negatives), and one
// renderer turns the spec into the caption plus the request fields the
// engine honours. The deterministic path here is complete on its own;
// the optional helper model only proposes additional spec updates.

// EnsureSpec makes sure the session carries a spec, rebuilding it from
// the recorded tweaks for sessions saved before specs existed.
func EnsureSpec(s *session.Session) {
	if s.Spec != nil {
		return
	}
	s.Spec = &session.PromptSpec{}
	for _, tw := range s.Tweaks {
		applyText(s, strings.ToLower(strings.TrimSpace(tw.Raw)))
	}
}

// clause is one operation parsed out of a steering input.
type clause struct {
	op   string // "more", "less" or "" for plain descriptions
	text string
}

// markers introduce an operation; each maps to the canonical op.
var markers = map[string]string{
	"more": "more", "add": "more", "extra": "more",
	"less": "less", "fewer": "less", "no": "less", "without": "less",
	"remove": "less", "drop": "less",
}

// fillerWords carry no meaning on their own inside a clause.
var fillerWords = map[string]bool{
	"make": true, "it": true, "the": true, "a": true, "bit": true,
	"much": true, "some": true, "please": true, "music": true,
	"sound": true, "and": true, "of": true, "lot": true, "way": true,
	"more": true, "little": true,
}

// parseClauses splits a steering input into operations:
// "less guitars more synths" -> (less, guitars), (more, synths).
func parseClauses(text string) []clause {
	words := strings.FieldsFunc(text, func(r rune) bool {
		return r == ' ' || r == ',' || r == ';' || r == '.' || r == '!'
	})
	var out []clause
	cur := clause{}
	flush := func() {
		cur.text = strings.TrimSpace(cur.text)
		if cur.text != "" || cur.op != "" {
			if cleaned := cleanClauseText(cur.text); cleaned != "" {
				out = append(out, clause{op: cur.op, text: cleaned})
			}
		}
		cur = clause{}
	}
	for _, w := range words {
		if op, ok := markers[w]; ok {
			flush()
			cur.op = op
			continue
		}
		if cur.text != "" {
			cur.text += " "
		}
		cur.text += w
	}
	flush()
	return out
}

// cleanClauseText strips filler so "make it a bit calmer" becomes
// "calmer".
func cleanClauseText(text string) string {
	var kept []string
	for _, w := range strings.Fields(text) {
		if !fillerWords[w] {
			kept = append(kept, w)
		}
	}
	return strings.Join(kept, " ")
}

// moodRule maps recognized words to mood mutations. Adding a mood
// removes its opposites, which is what "calmer" actually means.
type moodRule struct {
	re      *regexp.Regexp
	add     []string
	remove  []string
	descr   string
	tempoUp int // bpm nudge, when the word implies one
}

var moodRules = []moodRule{
	{regexp.MustCompile(`\b(energetic|energy|upbeat|intense|harder|pumped?|driving)\b`),
		[]string{"energetic", "driving"}, []string{"calm", "soft", "gentle", "mellow"}, "more energetic", 10},
	{regexp.MustCompile(`\b(calmer|calm|chill(er)?|relax(ed|ing)?|softer|gentler|mellow)\b`),
		[]string{"calm", "soft", "gentle"}, []string{"energetic", "driving", "aggressive", "heavy"}, "calmer", -10},
	{regexp.MustCompile(`\b(happier|happy|brighter|bright|cheerful|uplifting)\b`),
		[]string{"bright", "uplifting"}, []string{"dark", "melancholic"}, "brighter", 0},
	{regexp.MustCompile(`\b(darker|dark|sadder|sad|moody|melancholi[ck])\b`),
		[]string{"dark", "melancholic"}, []string{"bright", "uplifting", "cheerful"}, "darker", 0},
	{regexp.MustCompile(`\b(dreamy|spacey|atmospheric|ambient feel)\b`),
		[]string{"dreamy", "atmospheric", "spacious"}, nil, "dreamier", 0},
	{regexp.MustCompile(`\b(heavier|heavy|aggressive|distorted)\b`),
		[]string{"heavy", "aggressive", "distorted"}, []string{"calm", "soft", "gentle"}, "heavier", 0},
}

var (
	fasterRe  = regexp.MustCompile(`\b(faster|quicker|speed)\b`)
	slowerRe  = regexp.MustCompile(`\b(slower|slow)\b`)
	bpmRe     = regexp.MustCompile(`\b(\d{2,3})\s*bpm\b`)
	keyRe     = regexp.MustCompile(`\b(?:in|key of)\s+([a-g][#b♯♭]?)\s*(major|minor)\b`)
	timeSigRe = regexp.MustCompile(`\b([2346])/4(?:\s*time)?\b`)
	langRe    = regexp.MustCompile(`\b(?:vocals?|sing(?:ing)?|lyrics|sung)\s+in\s+([a-z]+)\b`)
)

// vocalLanguages maps spoken language names to the engine's ISO codes.
var vocalLanguages = map[string]string{
	"english": "en", "spanish": "es", "french": "fr", "german": "de",
	"italian": "it", "portuguese": "pt", "russian": "ru", "ukrainian": "uk",
	"polish": "pl", "japanese": "ja", "chinese": "zh", "mandarin": "zh",
	"korean": "ko", "hindi": "hi", "arabic": "ar", "turkish": "tr",
	"dutch": "nl", "swedish": "sv", "norwegian": "no", "finnish": "fi",
	"czech": "cs", "greek": "el", "hebrew": "he", "thai": "th",
	"vietnamese": "vi", "indonesian": "id", "romanian": "ro",
}

// applyText applies one lowered steering input to the session's spec,
// returning a human description of what changed.
func applyText(s *session.Session, text string) string {
	spec := s.Spec
	var descr []string
	note := func(d string) { descr = append(descr, d) }

	// Absolute settings anywhere in the input.
	if m := bpmRe.FindStringSubmatch(text); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil {
			spec.BPM = clampBPM(n)
			note(fmt.Sprintf("%d bpm", spec.BPM))
		}
	}
	if m := keyRe.FindStringSubmatch(text); m != nil {
		key := strings.ToUpper(m[1][:1]) + m[1][1:]
		key = strings.ReplaceAll(strings.ReplaceAll(key, "♯", "#"), "♭", "b")
		spec.KeyScale = key + " " + m[2]
		note("key " + spec.KeyScale)
	}
	if m := timeSigRe.FindStringSubmatch(text); m != nil {
		spec.TimeSignature = m[1]
		note(m[1] + "/4 time")
	} else if strings.Contains(text, "waltz") {
		spec.TimeSignature = "3"
		note("waltz time")
	}
	if m := langRe.FindStringSubmatch(text); m != nil {
		if code, ok := vocalLanguages[m[1]]; ok {
			spec.VocalLanguage = code
			note(m[1] + " vocals")
		}
	}

	for _, c := range parseClauses(text) {
		switch c.op {
		case "less":
			note(applyNegate(spec, c.text))
		case "more":
			note(applyStrengthen(spec, c.text))
		default:
			if d := applyPlain(spec, c.text); d != "" {
				note(d)
			}
		}
	}
	// Drop empty entries.
	kept := descr[:0]
	for _, d := range descr {
		if d != "" {
			kept = append(kept, d)
		}
	}
	if len(kept) == 0 {
		return ""
	}
	return strings.Join(kept, "; ")
}

// applyNegate removes a thing from every positive slot and records it
// in Negatives.
func applyNegate(spec *session.PromptSpec, noun string) string {
	if noun == "" {
		return ""
	}
	// Drums imply their beat: describe the result positively too.
	if strings.Contains(noun, "drum") || strings.Contains(noun, "percussion") {
		addWord(&spec.Mood, "beatless")
		removeMatching(spec, "drums")
		removeMatching(spec, "percussion")
		addNegative(spec, "drums")
		addNegative(spec, "percussion")
		return "no drums or percussion"
	}
	removeMatching(spec, noun)
	addNegative(spec, noun)
	return "avoiding " + noun
}

// applyStrengthen raises emphasis on a thing, un-negating it first.
func applyStrengthen(spec *session.PromptSpec, noun string) string {
	if noun == "" {
		return ""
	}
	spec.Negatives = removeWord(spec.Negatives, noun)
	for _, r := range moodRules {
		if r.re.MatchString(noun) {
			return applyMoodRule(spec, r)
		}
	}
	if strings.Contains(noun, "bass") || strings.Contains(noun, "low end") {
		noun = "deep bass"
	}
	w := spec.Instruments[noun]
	if w < 3 {
		w++
	}
	if spec.Instruments == nil {
		spec.Instruments = map[string]int{}
	}
	spec.Instruments[noun] = w
	if w > 1 {
		return fmt.Sprintf("more %s (x%d)", noun, w)
	}
	return "more " + noun
}

// applyPlain interprets a description without an explicit more/less.
func applyPlain(spec *session.PromptSpec, text string) string {
	if fasterRe.MatchString(text) {
		return nudgeTempo(spec, +15)
	}
	if slowerRe.MatchString(text) {
		return nudgeTempo(spec, -15)
	}
	for _, r := range moodRules {
		if r.re.MatchString(text) {
			return applyMoodRule(spec, r)
		}
	}
	// Texture and production words have their own slot.
	if strings.Contains(text, "vinyl") || strings.Contains(text, "tape") ||
		strings.Contains(text, "analog") || strings.Contains(text, "reverb") ||
		strings.Contains(text, "lo-fi") || strings.Contains(text, "lofi") {
		addWord(&spec.Production, text)
		return "production: " + text
	}
	if bpmRe.MatchString(text) || keyRe.MatchString(text) || timeSigRe.MatchString(text) || langRe.MatchString(text) {
		// Already handled as an absolute setting.
		return ""
	}
	addWord(&spec.Extra, text)
	return text
}

func applyMoodRule(spec *session.PromptSpec, r moodRule) string {
	for _, w := range r.add {
		addWord(&spec.Mood, w)
	}
	for _, w := range r.remove {
		spec.Mood = removeWord(spec.Mood, w)
	}
	if r.tempoUp != 0 && spec.BPM != 0 {
		spec.BPM = clampBPM(spec.BPM + r.tempoUp)
	}
	return r.descr
}

// nudgeTempo shifts BPM when one is set and uses tempo words otherwise.
func nudgeTempo(spec *session.PromptSpec, delta int) string {
	if spec.BPM != 0 {
		spec.BPM = clampBPM(spec.BPM + delta)
		return fmt.Sprintf("tempo to %d bpm", spec.BPM)
	}
	if delta > 0 {
		spec.TempoWords = removeWord(spec.TempoWords, "slow tempo")
		addWord(&spec.TempoWords, "fast tempo")
		return "faster tempo"
	}
	spec.TempoWords = removeWord(spec.TempoWords, "fast tempo")
	addWord(&spec.TempoWords, "slow tempo")
	return "slower tempo"
}

func clampBPM(n int) int {
	if n < 30 {
		return 30
	}
	if n > 300 {
		return 300
	}
	return n
}

// addWord appends w unless present (case-insensitive).
func addWord(list *[]string, w string) {
	for _, have := range *list {
		if strings.EqualFold(have, w) {
			return
		}
	}
	*list = append(*list, w)
}

// removeWord drops entries that contain w as a word.
func removeWord(list []string, w string) []string {
	kept := list[:0:0]
	for _, have := range list {
		if !wordMatch(have, w) {
			kept = append(kept, have)
		}
	}
	return kept
}

// addNegative records a negative once.
func addNegative(spec *session.PromptSpec, w string) {
	addWord(&spec.Negatives, w)
}

// removeMatching purges a noun from every positive slot of the spec.
func removeMatching(spec *session.PromptSpec, noun string) {
	spec.Genre = removeWord(spec.Genre, noun)
	spec.Mood = removeWord(spec.Mood, noun)
	spec.Production = removeWord(spec.Production, noun)
	spec.Extra = removeWord(spec.Extra, noun)
	spec.VocalStyle = removeWord(spec.VocalStyle, noun)
	for k := range spec.Instruments {
		if wordMatch(k, noun) {
			delete(spec.Instruments, k)
		}
	}
}

// wordMatch reports whether text contains noun as a whole word,
// tolerating a trailing plural s on either side. Multi-word nouns match
// as phrases.
func wordMatch(text, noun string) bool {
	noun = strings.ToLower(strings.TrimSpace(noun))
	if strings.ContainsRune(noun, ' ') {
		return strings.Contains(strings.ToLower(text), noun)
	}
	noun = strings.TrimSuffix(noun, "s")
	if noun == "" {
		return false
	}
	for _, w := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-')
	}) {
		if strings.TrimSuffix(w, "s") == noun {
			return true
		}
	}
	return false
}

// Rendered is a spec turned into concrete generation inputs.
type Rendered struct {
	// Caption is the positive text conditioning; negated things never
	// appear in it.
	Caption string
	// The structured request fields the engine honours directly.
	BPM           int
	KeyScale      string
	TimeSignature string
	VocalLanguage string
	// NegativePrompt is the comma-joined negatives (with plural
	// variants) for the planner LM; LMCfgScale is raised when negatives
	// exist (0 otherwise).
	NegativePrompt string
	LMCfgScale     float64
}

// negativeLMCfgScale is the raised planner guidance used whenever a
// negative prompt is sent (engine default is 2.5).
const negativeLMCfgScale = 3.25

// Render turns the session's spec into the caption and request fields.
// This is the single choke point between steering state and the engine.
func Render(s *session.Session) Rendered {
	EnsureSpec(s)
	spec := s.Spec
	var parts []string
	add := func(p string) {
		p = strings.TrimSpace(p)
		if p == "" {
			return
		}
		for _, have := range parts {
			if strings.EqualFold(have, p) {
				return
			}
		}
		parts = append(parts, p)
	}

	// Base prompt segments survive unless negated.
	for _, seg := range strings.Split(s.BasePrompt, ",") {
		if negated(spec, seg) {
			continue
		}
		add(seg)
	}
	for _, g := range spec.Genre {
		add(g)
	}
	// Instruments in stable order, weight rendered as repetition
	// reinforcement (the engine-documented way to emphasize).
	insts := make([]string, 0, len(spec.Instruments))
	for k := range spec.Instruments {
		insts = append(insts, k)
	}
	sort.Strings(insts)
	for _, k := range insts {
		add(k)
		if spec.Instruments[k] >= 2 {
			add("rich " + k)
		}
		if spec.Instruments[k] >= 3 {
			add("prominent " + k)
		}
	}
	for _, m := range spec.Mood {
		add(m)
	}
	for _, t := range spec.TempoWords {
		add(t)
	}
	if spec.BPM != 0 {
		add(fmt.Sprintf("%d bpm", spec.BPM))
	}
	for _, p := range spec.Production {
		add(p)
	}
	for _, v := range spec.VocalStyle {
		add(v)
	}
	for _, e := range spec.Extra {
		add(e)
	}

	out := Rendered{
		Caption:       strings.Join(parts, ", "),
		BPM:           spec.BPM,
		KeyScale:      spec.KeyScale,
		TimeSignature: spec.TimeSignature,
		VocalLanguage: spec.VocalLanguage,
	}
	if len(spec.Negatives) > 0 {
		var negs []string
		for _, n := range spec.Negatives {
			negs = append(negs, n)
			if !strings.HasSuffix(n, "s") {
				negs = append(negs, n+"s")
			} else {
				negs = append(negs, strings.TrimSuffix(n, "s"))
			}
		}
		out.NegativePrompt = strings.Join(negs, ", ")
		out.LMCfgScale = negativeLMCfgScale
	}
	return out
}

// negated reports whether a caption segment mentions any negative.
func negated(spec *session.PromptSpec, segment string) bool {
	for _, n := range spec.Negatives {
		if wordMatch(segment, n) {
			return true
		}
	}
	return false
}
