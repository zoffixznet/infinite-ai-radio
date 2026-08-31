package prompting

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"iar/internal/prosody"
)

// Scribe is the craft-aware lyric writer. It splits the job the way the
// research says a small local model needs: the model only ever proposes
// words while Go owns everything checkable - the song's structure, the
// rhyme plan (concrete end-word candidates from the pronouncing
// dictionary, not "please rhyme"), syllable discipline, repetition
// budgets and topic grounding. Sections are written one call at a time,
// validated deterministically, and rewritten with specific numeric
// feedback (a bounded number of tries) before the best attempt is
// kept: small models cannot judge their own writing, but they follow
// concrete corrections.
type Scribe struct{}

// Name implements LyricsGenerator.
func (*Scribe) Name() string { return "scribe" }

// Blurb implements LyricsGenerator.
func (*Scribe) Blurb() string {
	return "plans, drafts and revises; varied rhyme, controlled repetition"
}

// Timeout implements LyricsGenerator. The pipeline runs several model
// calls (plus repairs) strictly in the background, so the bound is
// generous; a slow write only delays how soon fresh words replace the
// previous track's.
func (*Scribe) Timeout() time.Duration { return 5 * time.Minute }

// scribeBrief is the concept the planning call produces: the topic
// pinned down to concrete, singable material.
type scribeBrief struct {
	Title   string   `json:"title"`
	Hook    string   `json:"hook"`
	Setting string   `json:"setting"`
	Story   string   `json:"story"`
	Nouns   []string `json:"nouns"`
	Verbs   []string `json:"verbs"`
	Images  []string `json:"images"`
	Moods   []string `json:"moods"`
}

// scribeBriefSchema is a raw ordered schema: constrained decoding emits
// keys in schema order, so the brainstorm fields (setting, story,
// nouns, verbs, images) come before the hook and title that should be
// distilled FROM them. A Go map would marshal alphabetically and force
// the hook out first, unbrainstormed.
var scribeBriefSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"setting": {"type": "string"},
		"story":   {"type": "string"},
		"nouns":   {"type": "array", "items": {"type": "string"}},
		"verbs":   {"type": "array", "items": {"type": "string"}},
		"images":  {"type": "array", "items": {"type": "string"}},
		"moods":   {"type": "array", "items": {"type": "string"}},
		"hook":    {"type": "string"},
		"title":   {"type": "string"}
	},
	"required": ["setting", "story", "nouns", "verbs", "images", "moods", "hook", "title"]
}`)

const scribeBriefSystem = `You are planning a song. Reply with JSON only.
Ground every field in the SONG TOPIC. Use plain, everyday, concrete
words a person can picture: real things, places, actions. No fantasy
imagery; never use neon, shadows, echoes, embers or twilight.
Fill the fields in this order:
"setting": where the song happens, one phrase.
"story": one sentence saying what verse 1 shows, then what verse 2 shows.
"nouns": 6-10 single common nouns you would actually see in the scene.
"verbs": 4-8 simple actions happening there.
"images": 3-5 short concrete phrases from the scene, 3-5 words each.
"moods": 2-4 feeling words that fit the music style.
"hook": one short singable line of 3 to 7 simple words that names the
topic plainly, built from the best of your nouns (it becomes the first
chorus line).
"title": 2-4 words.`

const scribeSectionSystem = `You write song lyrics one short section at a time, to order.
Output ONLY the section's lyric lines: one line per output line, no
numbering, no [tags], no quotes, no commentary.
House style: concrete everyday words (things you can see, hold, do);
one clear thought per line; short lines that sing; verbs over
adjectives; plain speech beats a forced rhyme; never use these words:
neon, shadows, echoes, whispers, embers, twilight, symphony, tapestry,
shimmering.
Follow every RULE in the request exactly.`

// think enables the reasoning phase on models that support it; both
// planning and writing benefit from it, and the latency is invisible
// in a background write.
var think = true

// scribeKeepAlive keeps the model loaded briefly after each helper
// call, so the calls that cluster around one track - the lyric write
// and the title that follows seconds later - share a single model load
// instead of paying a full load each (5+ GB moved twice per track,
// and minutes instead of seconds when the model runs on the CPU). It
// is also the total exposure after the cluster, or after an abandoned
// pipeline: the daemon evicts the model by itself this many seconds
// after the last call, giving the memory back to the music engine
// without an explicit unload racing the next caller.
const scribeKeepAlive = 30

// Generate implements LyricsGenerator.
func (s *Scribe) Generate(ctx context.Context, llm LLM, req LyricsRequest) (string, error) {
	if !req.English() {
		return s.simple(ctx, llm, req)
	}
	form := scribeForm(req.Seconds)
	brief := s.brief(ctx, llm, req)

	topic := req.Theme
	if topic == "" {
		topic = "a scene matching the mood of the music"
	}

	// The rhyme plan: one seed word per rhyming pair, each pair on its
	// own vowel sound so no two sections rhyme on the same sound (the
	// mono-rhyme failure), candidates expanded from the dictionary.
	usedEnds := map[string]bool{}
	hook := s.usableHook(brief, req)

	written := make([][]string, len(form))
	var summary []string
	// Write the chorus first (it carries the hook and gets stamped into
	// every repeat), then the rest in song order.
	var order []int
	for i, sec := range form {
		if sec.CopyOf == -1 && sec.Kind == "chorus" {
			order = append(order, i)
		}
	}
	for i, sec := range form {
		if sec.CopyOf == -1 && sec.Kind != "chorus" {
			order = append(order, i)
		}
	}
	for _, idx := range order {
		sec := form[idx]
		spec := sectionSpec{kind: sec.Kind, lines: sec.Lines, pairs: sectionRhyme(sec.Kind, sec.Lines)}
		var rules []string
		rules = append(rules, fmt.Sprintf("exactly %d lines", sec.Lines))
		rules = append(rules, fmt.Sprintf("%d-%d syllables per line (aim for 7 or 8)", scribeSyllableMin, scribeSyllableMax))

		spec.allowed = map[int][]string{}
		spec.fixed = map[int]string{}
		// Rhyme menus go LAST in the rule list: small models weight the
		// end of the prompt most, and the menus are the hardest rule.
		var rhymeRules []string
		switch sec.Kind {
		case "chorus":
			seed := ""
			if hook != "" {
				spec.fixed[0] = hook
				rules = append(rules, "line 1 must be exactly: "+hook)
				seed = prosody.EndWord(hook)
			}
			spec.mentions = s.topicMentions(brief, req, 3)
			if hook == "" && len(spec.mentions) > 0 {
				rules = append(rules, "mention at least one of: "+strings.Join(spec.mentions, ", "))
			}
			rhymeRules = s.pairRules(&spec, rhymePair{0, 1}, seed, rhymeRules, usedEnds)
			rhymeRules = s.pairRules(&spec, rhymePair{2, 3}, "", rhymeRules, usedEnds)
		case "verse":
			spec.mentions = s.verseMentions(brief, sec.Tag)
			if len(spec.mentions) > 0 {
				rules = append(rules, "mention at least one of: "+strings.Join(spec.mentions, ", "))
			}
			spec.avoidWords = contentWordsOf(flattenLines(written))
			spec.avoidLines = flattenLines(written)
			rhymeRules = s.pairRules(&spec, rhymePair{1, 3}, "", rhymeRules, usedEnds)
			rhymeRules = append(rhymeRules, "lines 1 and 3 must not rhyme with anything")
		case "bridge":
			rules = append(rules, "a shift in perspective: step back or look ahead", "no rhyme needed")
			spec.mentions = s.topicMentions(brief, req, 2)
			spec.avoidWords = contentWordsOf(flattenLines(written))
			spec.avoidLines = flattenLines(written)
		}
		rules = append(rules, "end each line on a different word")
		rules = append(rules, rhymeRules...)

		lines := s.writeSection(ctx, llm, req, brief, topic, sec, spec, rules, summary, usedEnds)
		written[idx] = lines
		for _, l := range lines {
			if e := prosody.EndWord(l); e != "" {
				usedEnds[e] = true
			}
		}
		summary = append(summary, sec.Tag+"\n  "+strings.Join(lines, "\n  "))
	}

	// Assemble: Title Case tags, blank-line separated, [Intro] to
	// [Outro], chorus repeats stamped verbatim - the trained-on shape
	// of the engine's own example corpus.
	var b strings.Builder
	empty := 0
	b.WriteString("[Intro]\n")
	for i, sec := range form {
		lines := written[i]
		if sec.CopyOf >= 0 {
			lines = written[sec.CopyOf]
		}
		if len(lines) == 0 {
			empty++
			continue
		}
		b.WriteString("\n" + sec.Tag + "\n")
		for _, l := range lines {
			b.WriteString(polishLine(l) + "\n")
		}
	}
	b.WriteString("\n[Outro]\n")
	// A mostly-empty song means the model calls failed; erroring here
	// lets the builder count the failure and keep the previous sheet
	// (or the engine's planner) instead of singing a skeleton.
	if empty*2 > len(form) {
		return "", fmt.Errorf("scribe wrote only %d of %d sections", len(form)-empty, len(form))
	}
	return b.String(), nil
}

// pairRules assigns end-word candidates to a rhyme pair and renders the
// rule text. seed may be empty, in which case a fresh seed is picked
// from the brief on an unused vowel sound.
func (s *Scribe) pairRules(spec *sectionSpec, pair rhymePair, seed string, rules []string, usedEnds map[string]bool) []string {
	usedNuclei := map[string]bool{}
	for e := range usedEnds {
		if n := prosody.Nucleus(e); n != "" {
			usedNuclei[n] = true
		}
	}
	for _, a := range spec.allowed {
		for _, w := range a {
			if n := prosody.Nucleus(w); n != "" {
				usedNuclei[n] = true
			}
		}
	}
	fixedSeed := seed != ""
	if seed == "" {
		seed = pickRhymeSeed(specSeedPool(*spec), usedNuclei, usedEnds)
	}
	if seed == "" {
		rules = append(rules, fmt.Sprintf("lines %d and %d must rhyme with each other (a near rhyme is fine)", pair[0]+1, pair[1]+1))
		spec.pairs = appendPair(spec.pairs, pair)
		return rules
	}
	cands := prosody.FindRhymes(seed, usedEnds).Candidates(6)
	if len(cands) < 2 {
		rules = append(rules, fmt.Sprintf("lines %d and %d must rhyme with each other (a near rhyme is fine)", pair[0]+1, pair[1]+1))
		spec.pairs = appendPair(spec.pairs, pair)
		return rules
	}
	if fixedSeed {
		// The first line's end is already fixed (the hook); only the
		// partner line chooses from the candidates.
		spec.allowed[pair[1]] = cands
		rules = append(rules, fmt.Sprintf("line %d must end with one of: %s", pair[1]+1, strings.Join(cands, ", ")))
	} else {
		pool := append([]string{seed}, cands...)
		spec.allowed[pair[0]] = pool
		spec.allowed[pair[1]] = pool
		rules = append(rules, fmt.Sprintf("lines %d and %d each end with a different word from: %s", pair[0]+1, pair[1]+1, strings.Join(pool, ", ")))
	}
	spec.pairs = appendPair(spec.pairs, pair)
	return rules
}

func appendPair(pairs []rhymePair, p rhymePair) []rhymePair {
	for _, have := range pairs {
		if have == p {
			return pairs
		}
	}
	return append(pairs, p)
}

// specSeedPool lists candidate seed words: hardy common fallbacks; the
// caller's brief words are tried first via pickRhymeSeed's pool order.
func specSeedPool(spec sectionSpec) []string {
	return append(append([]string{}, spec.mentions...),
		"day", "road", "home", "town", "door", "ground", "line", "side", "sound", "way")
}

// pickRhymeSeed finds a seed word whose vowel sound is not yet used in
// the song and that has enough rhyme candidates to hand the model a
// real choice.
func pickRhymeSeed(pool []string, usedNuclei, usedEnds map[string]bool) string {
	for _, w := range pool {
		w = prosody.Normalize(w)
		if w == "" || usedEnds[w] || !prosody.Known(w) || !prosody.Common(w) {
			continue
		}
		if n, _ := prosody.Syllables(w); n > 2 {
			continue
		}
		nuc := prosody.Nucleus(w)
		if nuc == "" || usedNuclei[nuc] {
			continue
		}
		if len(prosody.FindRhymes(w, usedEnds).Candidates(6)) >= 3 {
			return w
		}
	}
	return ""
}

// brief runs the planning call, retrying once, and falls back to a
// Go-built brief from the raw theme so the pipeline always has
// something concrete to write from.
func (s *Scribe) brief(ctx context.Context, llm LLM, req LyricsRequest) scribeBrief {
	topic := req.Theme
	if topic == "" {
		topic = "no set topic: invent one plain, everyday scene that fits the music's mood, and name it in the hook"
	}
	user := "MUSIC STYLE: " + req.Style + "\nSONG TOPIC: " + topic
	if len(req.AvoidHooks) > 0 {
		user += "\nHooks already used recently, do not reuse or echo them: " + strings.Join(req.AvoidHooks, " | ")
	}
	for attempt, temp := range []float64{0.4, 0.8} {
		raw, err := llm.ChatWith(ctx, scribeBriefSystem, user, ChatOpts{
			Format: scribeBriefSchema, Temperature: temp, Think: &think,
			NumCtx: 8192, NumPredict: 700, KeepAliveSeconds: scribeKeepAlive,
		})
		if err != nil {
			continue
		}
		var b scribeBrief
		if json.Unmarshal([]byte(raw), &b) != nil {
			continue
		}
		sanitizeBrief(&b)
		if briefUsable(b, req) || attempt == 1 {
			return b
		}
	}
	return fallbackBrief(req)
}

// sanitizeBrief trims and filters the model's plan to real, common
// words.
func sanitizeBrief(b *scribeBrief) {
	b.Title = sanitizeLine(b.Title, 48)
	b.Hook = sanitizeLine(strings.Trim(b.Hook, `."!?`), 64)
	b.Setting = sanitizeLine(b.Setting, 80)
	b.Story = sanitizeLine(b.Story, 160)
	clean := func(list []string, max int) []string {
		var out []string
		for _, w := range list {
			w = prosody.Normalize(w)
			if w != "" && prosody.Known(w) && len(out) < max {
				out = append(out, w)
			}
		}
		return out
	}
	b.Nouns = clean(b.Nouns, 10)
	b.Verbs = clean(b.Verbs, 8)
	b.Moods = clean(b.Moods, 4)
	var images []string
	for _, im := range b.Images {
		im = sanitizeLine(im, 48)
		if im != "" && len(images) < 5 {
			images = append(images, im)
		}
	}
	b.Images = images
}

// briefUsable checks the plan grounds the requested theme: enough of
// the theme's content words survive into the plan, and the hook is a
// real singable line.
func briefUsable(b scribeBrief, req LyricsRequest) bool {
	if len(b.Nouns) < 3 || b.Hook == "" {
		return false
	}
	hw := prosody.Words(b.Hook)
	if len(hw) < 2 || len(hw) > 8 {
		return false
	}
	for _, w := range hw {
		if !prosody.Known(w) {
			return false
		}
	}
	theme := prosody.ContentWords(req.Theme)
	if len(theme) == 0 {
		return true
	}
	all := strings.Join(b.Nouns, " ") + " " + strings.Join(b.Verbs, " ") + " " +
		strings.Join(b.Images, " ") + " " + b.Hook + " " + b.Setting + " " + b.Story
	stems := map[string]bool{}
	for _, w := range prosody.Words(all) {
		stems[prosody.Stem(w)] = true
	}
	hits := 0
	for _, w := range theme {
		if stems[prosody.Stem(w)] {
			hits++
		}
	}
	return hits*2 >= len(theme)
}

// fallbackBrief builds a minimal plan from the theme alone, so a failed
// planning call still yields grounded, on-topic writing.
func fallbackBrief(req LyricsRequest) scribeBrief {
	words := prosody.ContentWords(req.Theme)
	b := scribeBrief{Setting: req.Theme, Story: req.Theme}
	for _, w := range words {
		if prosody.Known(w) && len(b.Nouns) < 8 {
			b.Nouns = append(b.Nouns, w)
		}
	}
	return b
}

// usableHook returns the brief's hook when it can anchor the chorus:
// singable length and (when a theme exists) carrying a theme word.
func (s *Scribe) usableHook(b scribeBrief, req LyricsRequest) string {
	if b.Hook == "" {
		return ""
	}
	n, unknown := prosody.LineSyllables(b.Hook)
	if len(unknown) > 0 || n < 3 || n > scribeSyllableMax {
		return ""
	}
	if e := prosody.EndWord(b.Hook); e == "" || prosody.FunctionWord(e) || !prosody.Known(e) {
		return ""
	}
	theme := prosody.ContentWords(req.Theme)
	if len(theme) > 0 && !mentionsAny([]string{b.Hook}, theme) && !mentionsAny([]string{b.Hook}, b.Nouns) {
		return ""
	}
	return b.Hook
}

// topicMentions picks the words a section must ground itself in: theme
// content words first, then the brief's nouns.
func (s *Scribe) topicMentions(b scribeBrief, req LyricsRequest, n int) []string {
	words := prosody.ContentWords(req.Theme)
	for _, w := range b.Nouns {
		if len(words) >= n {
			break
		}
		if !slices.Contains(words, w) {
			words = append(words, w)
		}
	}
	if len(words) > n {
		words = words[:n]
	}
	return words
}

// verseMentions spreads the brief's nouns across the verses so verse 2
// reaches for different furniture than verse 1.
func (s *Scribe) verseMentions(b scribeBrief, tag string) []string {
	if len(b.Nouns) == 0 {
		return nil
	}
	half := (len(b.Nouns) + 1) / 2
	if strings.Contains(tag, "2") {
		return b.Nouns[half:]
	}
	first := b.Nouns[:half]
	if len(first) > 3 {
		first = first[:3]
	}
	return first
}

// flattenLines joins every already-written section's lines.
func flattenLines(written [][]string) []string {
	var out []string
	for _, sec := range written {
		out = append(out, sec...)
	}
	return out
}

// contentWordsOf collects the distinct content words of written lines.
func contentWordsOf(lines []string) []string {
	return prosody.ContentWords(strings.Join(lines, " "))
}

// writeSection runs the write-validate-repair loop for one section and
// returns the best attempt's lines (possibly imperfect: the radio never
// stalls on a stubborn section).
func (s *Scribe) writeSection(ctx context.Context, llm LLM, req LyricsRequest, brief scribeBrief, topic string, sec scribeSection, spec sectionSpec, rules []string, summary []string, usedEnds map[string]bool) []string {
	var b strings.Builder
	fmt.Fprintf(&b, "MUSIC STYLE: %s\n", req.Style)
	fmt.Fprintf(&b, "SONG: %q - about %s.", brief.Title, topic)
	if brief.Setting != "" {
		fmt.Fprintf(&b, " Setting: %s.", brief.Setting)
	}
	if brief.Story != "" {
		fmt.Fprintf(&b, " Story: %s", brief.Story)
	}
	b.WriteString("\n")
	if len(brief.Nouns)+len(brief.Verbs) > 0 {
		fmt.Fprintf(&b, "WORDS TO DRAW FROM: %s\n", strings.Join(append(append([]string{}, brief.Nouns...), brief.Verbs...), ", "))
	}
	if len(brief.Images) > 0 {
		fmt.Fprintf(&b, "IMAGES TO DRAW FROM: %s\n", strings.Join(brief.Images, "; "))
	}
	if len(brief.Moods) > 0 {
		fmt.Fprintf(&b, "MOOD: %s\n", strings.Join(brief.Moods, ", "))
	}
	if len(summary) > 0 {
		fmt.Fprintf(&b, "ALREADY WRITTEN:\n%s\n", strings.Join(summary, "\n"))
	}
	fmt.Fprintf(&b, "SECTION TO WRITE: %s - %s\n", strings.Trim(sec.Tag, "[]"), sectionJob(sec))
	b.WriteString("RULES:\n")
	for _, r := range rules {
		b.WriteString("- " + r + "\n")
	}
	request := b.String()

	var bestLines []string
	bestPenalty := 1 << 30
	prompt := request
	// Four tries: write, repair with specific problems, roll fresh,
	// repair once more; then the best-scoring attempt ships (an
	// endless radio never stalls on a stubborn section).
	for attempt := 0; attempt < 4; attempt++ {
		// Thinking stays OFF for writing: num_predict counts thinking
		// tokens too, and a reasoning phase eats the whole budget
		// before a single lyric line comes out (measured: sections
		// came back empty). The brief keeps thinking; the writes rely
		// on concrete menus plus the validators instead.
		noThink := false
		// min-p with top-p off is the measured best creative sampler
		// combination; repetition control stays in the validators.
		opts := ChatOpts{
			Temperature: 1.0, MinP: 0.08, RepeatPenalty: 1.05,
			NumPredict: 300, NumCtx: 8192, Think: &noThink, KeepAliveSeconds: scribeKeepAlive,
		}
		if attempt%2 == 1 {
			opts.Temperature = 0.6
		}
		if attempt == 2 {
			prompt = request
		}
		raw, err := llm.ChatWith(ctx, scribeSectionSystem, prompt, opts)
		if err != nil {
			break
		}
		lines := parseSectionLines(raw, sec.Lines)
		res := checkSection(lines, spec, usedEnds)
		if p := res.penalty(); p < bestPenalty {
			bestPenalty, bestLines = p, lines
		}
		if res.ok() {
			break
		}
		prompt = request + "\nYOUR PREVIOUS ATTEMPT:\n" + strings.Join(lines, "\n") +
			"\nPROBLEMS TO FIX:\n" + res.problems() +
			"Rewrite the whole section, fixing every problem and keeping what worked."
	}
	if len(bestLines) == 0 && len(spec.fixed) > 0 {
		// Salvage: at least the pinned hook line survives.
		bestLines = []string{spec.fixed[0]}
	}
	return bestLines
}

// sectionJob names what each section is for, so verses advance instead
// of restating.
func sectionJob(sec scribeSection) string {
	switch {
	case sec.Kind == "chorus":
		return "the emotional center; plain, memorable, singable"
	case strings.Contains(sec.Tag, "2"):
		return "move the story forward in time with new details; do not restate verse 1"
	case sec.Kind == "bridge":
		return "a short shift: step back or look ahead"
	default:
		return "set the scene: who, where, what is happening"
	}
}

// parseSectionLines extracts up to n lyric lines from a model reply,
// dropping numbering, tags, fences and commentary.
func parseSectionLines(raw string, n int) []string {
	var out []string
	raw = strings.ReplaceAll(raw, " / ", "\n")
	for _, line := range strings.Split(raw, "\n") {
		line = strings.ReplaceAll(line, "\u2019", "'")
		line = stripBrackets(line)
		line = strings.TrimSpace(line)
		line = strings.TrimPrefix(line, "```")
		if line == "" || strings.HasPrefix(line, "[") || strings.HasPrefix(line, "#") ||
			strings.HasSuffix(line, ":") {
			// Tags, fences and "Here are the lyrics:" commentary.
			continue
		}
		// Strip list markers and "Line 1:" style prefixes.
		line = strings.TrimLeft(line, "-*• \t")
		for i := 0; i < len(line); i++ {
			if line[i] >= '0' && line[i] <= '9' {
				continue
			}
			if (line[i] == '.' || line[i] == ')' || line[i] == ':') && i > 0 && i < 4 {
				line = strings.TrimSpace(line[i+1:])
			}
			break
		}
		line = strings.Trim(line, `"'`)
		if line == "" {
			continue
		}
		out = append(out, line)
		if len(out) == n {
			break
		}
	}
	return out
}

// stripBrackets removes inline [bracketed] fragments; the model must
// never write a structure tag - Go owns those. A line that is ONLY a
// tag becomes empty and is dropped by the caller.
func stripBrackets(line string) string {
	if !strings.ContainsRune(line, '[') {
		return line
	}
	var b strings.Builder
	depth := 0
	for _, r := range line {
		switch {
		case r == '[':
			depth++
		case r == ']':
			if depth > 0 {
				depth--
			}
		case depth == 0:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// asciiMap folds typographic punctuation to the ASCII the engine's
// lyric tokenizer was trained on.
var asciiMap = strings.NewReplacer(
	"\u2019", "'", "\u2018", "'", "\u201c", `"`, "\u201d", `"`,
	"\u2014", "-", "\u2013", "-", "\u2026", "...",
)

// polishLine normalizes a lyric line for the engine: ASCII punctuation
// only, leading capital (also dodging the engine's lowercase-n bug), no
// trailing period.
func polishLine(line string) string {
	line = asciiMap.Replace(line)
	var clean strings.Builder
	for _, r := range line {
		if r < 128 {
			clean.WriteRune(r)
		}
	}
	line = strings.TrimSpace(clean.String())
	line = strings.TrimRight(line, " .;")
	if line == "" {
		return line
	}
	r := []rune(line)
	if r[0] >= 'a' && r[0] <= 'z' {
		r[0] = r[0] - 'a' + 'A'
	}
	return string(r)
}

// simple is the one-call path for non-English lyrics, where the
// English phonetic machinery does not apply; structure and density
// guidance still follow the engine's conventions.
func (s *Scribe) simple(ctx context.Context, llm LLM, req LyricsRequest) (string, error) {
	lines := 16
	if req.Seconds > 0 {
		lines = req.Seconds * 17 / 150
		if lines < 8 {
			lines = 8
		}
		if lines > 22 {
			lines = 22
		}
	}
	system := fmt.Sprintf(`You write song lyrics for a music model. Output ONLY the lyrics.
Format: section tags in Title Case on their own lines - [Intro], [Verse 1],
[Chorus], [Verse 2], [Bridge], [Outro] - with a blank line between
sections and no other markup. About %d sung lines total; 5-10 syllables
per line; the chorus appears twice with identical words; every other
line is fresh (never repeat one sentence over and over).
Write the lyrics entirely in %s - every sung line, in that language's
own script. Simple concrete words; stay strictly on the given topic.`, lines, req.WriteIn())
	theme := req.Theme
	if theme == "" {
		theme = "matching the mood of the music"
	}
	user := "MUSIC STYLE: " + req.Style + "\nLYRICS TOPIC: " + theme
	return llm.ChatWith(ctx, system, user, ChatOpts{
		Temperature: 0.85, NumCtx: 8192, NumPredict: 700,
		KeepAliveSeconds: scribeKeepAlive,
	})
}
