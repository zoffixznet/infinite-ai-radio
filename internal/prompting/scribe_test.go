package prompting

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"iar/internal/prosody"
)

// scribeFake plays an obedient model: it reads the rule menus out of
// the request and follows them. Driving Scribe with it proves the
// prompts and the validators agree about what a good section is.
type scribeFake struct {
	briefErr   bool
	briefCalls int
	writeCalls int
	requests   []string
	freeEnds   []string
	prefixAt   int
}

func newScribeFake() *scribeFake {
	return &scribeFake{freeEnds: []string{
		"paper", "morning", "corner", "window", "table", "coffee",
		"pocket", "summer", "winter", "garden", "market", "shoulder",
		"butter", "copper", "dinner", "ladder", "mirror", "carpet",
		"jacket", "kitchen", "engine", "hammer", "harbor", "meadow",
	}}
}

func (f *scribeFake) nextFree() string {
	w := f.freeEnds[0]
	f.freeEnds = append(f.freeEnds[1:], w+"x")
	return w
}

// nextPrefix generates a distinct 4-word line opener each call (mixed
// radix over subject x verb x preposition), so the near-copy validator
// stays happy the way a real writer varying phrasing would.
func (f *scribeFake) nextPrefix() string {
	subjects := []string{"We", "They", "You"}
	verbs := []string{"walk", "ride", "wait", "stand", "lean", "look", "stop", "sit", "run", "turn"}
	preps := []string{"beside", "behind", "below", "around", "before", "against"}
	i := f.prefixAt
	f.prefixAt++
	return subjects[i%3] + " " + verbs[(i/3)%10] + " " + preps[(i/30)%6] + " the"
}

var (
	linesRe    = regexp.MustCompile(`exactly (\d+) lines`)
	fixedRe    = regexp.MustCompile(`line 1 must be exactly: (.+)`)
	menuOneRe  = regexp.MustCompile(`line (\d+) must end with one of: (.+)`)
	menuPairRe = regexp.MustCompile(`lines (\d+) and (\d+) each end with a different word from: (.+)`)
	bareRe     = regexp.MustCompile(`lines (\d+) and (\d+) must rhyme with each other`)
	mentionRe  = regexp.MustCompile(`mention at least one of: (.+)`)
)

func (f *scribeFake) Chat(_ context.Context, system, user string) (string, error) {
	return f.ChatWith(context.Background(), system, user, ChatOpts{})
}

func (f *scribeFake) ChatJSON(_ context.Context, system, user string, schema any) (string, error) {
	return f.ChatWith(context.Background(), system, user, ChatOpts{Format: schema})
}

func (f *scribeFake) ChatWith(_ context.Context, system, user string, opts ChatOpts) (string, error) {
	f.requests = append(f.requests, user)
	if opts.Format != nil {
		f.briefCalls++
		if f.briefErr {
			return "", fmt.Errorf("brief boom")
		}
		b := scribeBrief{
			Title:   "Mall Day",
			Hook:    "We are shopping at the mall",
			Setting: "a busy shopping mall",
			Story:   "verse one arrives at the mall, verse two heads home with bags",
			Verses:  2,
			Bridge:  true,
			Nouns:   []string{"mall", "bags", "stores", "shoes", "coffee", "doors"},
			Verbs:   []string{"shop", "walk", "laugh", "buy"},
			Images:  []string{"bright lights down the hall"},
			Moods:   []string{"happy", "light"},
		}
		out, _ := json.Marshal(b)
		return string(out), nil
	}
	f.writeCalls++
	n := 4
	if m := linesRe.FindStringSubmatch(user); m != nil {
		fmt.Sscanf(m[1], "%d", &n)
	}
	lines := make([]string, n)
	used := map[int]bool{}
	if m := fixedRe.FindStringSubmatch(user); m != nil {
		lines[0] = strings.TrimSpace(m[1])
		used[0] = true
	}
	mention := ""
	if m := mentionRe.FindStringSubmatch(user); m != nil {
		mention = strings.TrimSpace(strings.Split(m[1], ",")[0])
	}
	takeMention := func() string {
		if mention == "" {
			return ""
		}
		w := mention
		mention = ""
		return w + " "
	}
	pickFrom := func(menu string, avoid map[string]bool) string {
		for _, w := range strings.Split(menu, ",") {
			w = strings.TrimSpace(w)
			if w != "" && !avoid[w] {
				return w
			}
		}
		return "road"
	}
	taken := map[string]bool{}
	for _, m := range menuOneRe.FindAllStringSubmatch(user, -1) {
		var idx int
		fmt.Sscanf(m[1], "%d", &idx)
		w := pickFrom(m[2], taken)
		taken[w] = true
		lines[idx-1] = f.nextPrefix() + " " + takeMention() + w
		used[idx-1] = true
	}
	for _, m := range menuPairRe.FindAllStringSubmatch(user, -1) {
		var i, j int
		fmt.Sscanf(m[1], "%d", &i)
		fmt.Sscanf(m[2], "%d", &j)
		w1 := pickFrom(m[3], taken)
		taken[w1] = true
		w2 := pickFrom(m[3], taken)
		taken[w2] = true
		lines[i-1] = f.nextPrefix() + " " + takeMention() + w1
		lines[j-1] = f.nextPrefix() + " " + w2
		used[i-1], used[j-1] = true, true
	}
	for _, m := range bareRe.FindAllStringSubmatch(user, -1) {
		var i, j int
		fmt.Sscanf(m[1], "%d", &i)
		fmt.Sscanf(m[2], "%d", &j)
		if !used[i-1] {
			lines[i-1] = "We ride the early train"
			used[i-1] = true
		}
		if !used[j-1] {
			lines[j-1] = "Out in the summer rain"
			used[j-1] = true
		}
	}
	for i := range lines {
		if used[i] {
			continue
		}
		if m := takeMention(); m != "" {
			lines[i] = f.nextPrefix() + " " + m + f.nextFree()
			continue
		}
		lines[i] = f.nextPrefix() + " " + f.nextFree()
	}
	return strings.Join(lines, "\n"), nil
}

func TestScribeFullPipeline(t *testing.T) {
	fake := newScribeFake()
	out, err := (&Scribe{}).Generate(context.Background(), fake, LyricsRequest{
		Style: "upbeat pop, bright, energetic",
		Theme: "going to the mall to do some shopping",
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	for _, tag := range []string{"[Intro]", "[Verse 1]", "[Chorus]", "[Verse 2]", "[Bridge]", "[Outro]"} {
		if !strings.Contains(out, tag) {
			t.Errorf("missing %s in:\n%s", tag, out)
		}
	}
	if n := strings.Count(out, "[Chorus]"); n != 2 {
		t.Errorf("chorus tag count = %d", n)
	}
	if n := strings.Count(out, "We are shopping at the mall"); n < 2 {
		t.Errorf("hook must be stamped into every chorus, found %d:\n%s", n, out)
	}
	// The chorus repeats are verbatim.
	blocks := sectionBodies(out)
	if len(blocks["[Chorus]"]) != 2 || blocks["[Chorus]"][0] != blocks["[Chorus]"][1] {
		t.Errorf("chorus repeats differ:\n%s", out)
	}
	// Assembled sung lines are clean and pass the song check.
	sung := sungLines(out)
	if len(sung) < 14 {
		t.Errorf("only %d sung lines:\n%s", len(sung), out)
	}
	for _, l := range sung {
		if l[0] >= 'a' && l[0] <= 'z' {
			t.Errorf("line not capitalized: %q", l)
		}
		if strings.HasSuffix(l, ".") {
			t.Errorf("trailing period kept: %q", l)
		}
	}
	if probs := checkSong(sung); len(probs) != 0 {
		t.Errorf("song check: %v\n%s", probs, out)
	}
	if fake.briefCalls != 1 {
		t.Errorf("brief calls = %d", fake.briefCalls)
	}
	// Unique sections for the planned two-verse-with-bridge form:
	// chorus, V1, V2, bridge = 4 writes when every first attempt
	// passes.
	if fake.writeCalls > 8 {
		t.Errorf("too many write calls: %d", fake.writeCalls)
	}
}

func TestScribeFallsBackWithoutBrief(t *testing.T) {
	fake := newScribeFake()
	fake.briefErr = true
	out, err := (&Scribe{}).Generate(context.Background(), fake, LyricsRequest{
		Style: "lofi chill",
		Theme: "walking home in the rain at night",
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if !strings.Contains(out, "[Chorus]") || !strings.Contains(out, "[Verse 1]") {
		t.Fatalf("fallback lost structure:\n%s", out)
	}
	if fake.briefCalls != 2 {
		t.Errorf("brief should be retried once, calls = %d", fake.briefCalls)
	}
}

// A thinking model can spend its whole num_predict budget reasoning
// and answer with nothing at all. When that happens the plan must
// still land - a song that falls back to the standard shape loses the
// verse count and bridge its story asked for, and comes out short.
func TestScribeBriefSurvivesAThinkingModelThatAnswersNothing(t *testing.T) {
	var thinkFlags []bool
	brief := scribeBrief{
		Title: "Mall Day", Hook: "We are shopping at the mall",
		Setting: "a busy shopping mall",
		Story:   "verse one arrives, verse two heads home, verse three unpacks",
		Verses:  3, Bridge: true,
		Nouns:  []string{"mall", "bags", "stores", "shoes", "coffee", "doors"},
		Verbs:  []string{"shop", "walk", "laugh", "buy"},
		Images: []string{"bright lights down the hall"},
		Moods:  []string{"happy", "light"},
	}
	encoded, _ := json.Marshal(brief)
	llm := &fakeLLM{chatWith: func(system, user string, opts ChatOpts) (string, error) {
		if opts.Format == nil {
			return "[Verse 1]\nwe walk the hall", nil
		}
		think := opts.Think != nil && *opts.Think
		thinkFlags = append(thinkFlags, think)
		if think {
			// Exactly what a thinking model does when its budget goes
			// entirely to the thinking channel.
			return "", fmt.Errorf("ollama: empty reply")
		}
		return string(encoded), nil
	}}

	got := (&Scribe{}).brief(context.Background(), llm, LyricsRequest{
		Style: "pop-punk", Theme: "summer nights with friends",
	})
	if got.Title != "Mall Day" || got.Verses != 3 || !got.Bridge {
		t.Fatalf("the plan did not land: %+v", got)
	}
	if len(thinkFlags) != 2 || !thinkFlags[0] || thinkFlags[1] {
		t.Fatalf("attempts asked for thinking %v; want [true false]", thinkFlags)
	}
}

// The thinking attempt needs room to both reason and answer; the
// budget that only fits an answer is what starved it.
func TestScribeBriefGivesTheThinkingAttemptRoom(t *testing.T) {
	var predicts []int
	llm := &fakeLLM{chatWith: func(system, user string, opts ChatOpts) (string, error) {
		if opts.Format == nil {
			return "[Verse 1]\nwe walk the hall", nil
		}
		predicts = append(predicts, opts.NumPredict)
		return "", fmt.Errorf("ollama: empty reply")
	}}
	(&Scribe{}).brief(context.Background(), llm, LyricsRequest{Style: "pop", Theme: "rain"})
	if len(predicts) != 2 || predicts[0] <= 700 {
		t.Fatalf("thinking attempt budget = %v; the thinking attempt needs more than the answer alone", predicts)
	}
}

func TestScribeNonEnglishUsesSimplePath(t *testing.T) {
	var gotSystem string
	llm := &fakeLLM{chatWith: func(system, user string, opts ChatOpts) (string, error) {
		gotSystem = system
		return "[Intro]\n\n[Verse 1]\nZeile eins\n\n[Outro]", nil
	}}
	out, err := (&Scribe{}).Generate(context.Background(), llm, LyricsRequest{
		Style: "schlager", Theme: "der Sommer", Language: "de",
	})
	if err != nil || !strings.Contains(out, "[Verse 1]") {
		t.Fatalf("simple path: %v %q", err, out)
	}
	if !strings.Contains(gotSystem, "German") {
		t.Errorf("language missing from system prompt: %q", gotSystem)
	}
}

// A language the engine has no tag for still reaches the writer by
// name: the words get written in it and the engine sings them untagged.
func TestScribeUntaggedLanguageReachesTheWriter(t *testing.T) {
	var gotSystem string
	llm := &fakeLLM{chatWith: func(system, user string, opts ChatOpts) (string, error) {
		gotSystem = system
		return "[Intro]\n\n[Verse 1]\nUsa ka linya\n\n[Outro]", nil
	}}
	_, err := (&Scribe{}).Generate(context.Background(), llm, LyricsRequest{
		Style: "island pop", LanguageName: "Bisaya (Cebuano)",
	})
	if err != nil {
		t.Fatalf("simple path: %v", err)
	}
	if !strings.Contains(gotSystem, "Bisaya (Cebuano)") {
		t.Errorf("language missing from system prompt: %q", gotSystem)
	}
}

func TestScribeFormFromPlan(t *testing.T) {
	cases := []struct {
		verses   int
		bridge   bool
		sections int
		sung     int
		choruses int
	}{
		{1, false, 3, 12, 2}, // closing chorus stamped to land twice
		{1, true, 4, 14, 2},
		{2, false, 4, 16, 2},
		{2, true, 5, 18, 2},
		{3, false, 6, 24, 3},
		{3, true, 7, 26, 3},
		{0, true, 5, 18, 2}, // nonsense plans fall back to two verses
		{9, false, 4, 16, 2},
	}
	for _, c := range cases {
		form := scribeFormFromPlan(c.verses, c.bridge)
		if len(form) != c.sections {
			t.Errorf("form(%d,%v) has %d sections, want %d", c.verses, c.bridge, len(form), c.sections)
		}
		sung, choruses := 0, 0
		for _, s := range form {
			sung += s.Lines
			if s.Kind == "chorus" {
				choruses++
			}
		}
		if sung != c.sung {
			t.Errorf("form(%d,%v) sings %d lines, want %d", c.verses, c.bridge, sung, c.sung)
		}
		if choruses != c.choruses {
			t.Errorf("form(%d,%v) has %d choruses, want %d", c.verses, c.bridge, choruses, c.choruses)
		}
		if last := form[len(form)-1]; last.Kind != "chorus" {
			t.Errorf("form(%d,%v) does not close on a chorus", c.verses, c.bridge)
		}
	}
}

// sectionBodies maps tag -> the joined body of each occurrence.
func sectionBodies(lyrics string) map[string][]string {
	out := map[string][]string{}
	var tag string
	var body []string
	flush := func() {
		if tag != "" && len(body) > 0 {
			out[tag] = append(out[tag], strings.Join(body, "\n"))
		}
		body = nil
	}
	for _, line := range strings.Split(lyrics, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			flush()
			tag = line
			continue
		}
		if line != "" {
			body = append(body, line)
		}
	}
	flush()
	return out
}

// sungLines lists all non-tag, non-empty lines.
func sungLines(lyrics string) []string {
	var out []string
	for _, line := range strings.Split(lyrics, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "[") {
			out = append(out, line)
		}
	}
	return out
}

func TestCheckSectionRejectsMonoRhyme(t *testing.T) {
	spec := sectionSpec{kind: "verse", lines: 4, pairs: []rhymePair{{1, 3}}}
	res := checkSection([]string{
		"I went to read a book",
		"It was on a nook",
		"I put my coat there on a hook",
		"And gave that book a look",
	}, spec, map[string]bool{})
	if res.ok() {
		t.Fatal("book/nook/hook/look must be rejected")
	}
	found := false
	for _, h := range res.hard {
		if strings.Contains(h, "must not rhyme") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a mono-rhyme violation, got %v", res.hard)
	}
}

func TestCheckSectionRejectsGibberishAndLongLines(t *testing.T) {
	spec := sectionSpec{kind: "verse", lines: 2}
	res := checkSection([]string{
		"My name is Mandu",
		"This line has entirely too many syllables to ever be sung by anyone",
	}, spec, map[string]bool{})
	if res.ok() {
		t.Fatal("gibberish and overlong lines must be rejected")
	}
	joined := strings.Join(res.hard, " | ")
	if !strings.Contains(joined, "mandu") {
		t.Errorf("mandu not flagged: %s", joined)
	}
	if !strings.Contains(joined, "syllables") {
		t.Errorf("syllables not flagged: %s", joined)
	}
}

func TestCheckSectionRejectsBannedRhymesAndCliches(t *testing.T) {
	spec := sectionSpec{kind: "chorus", lines: 2, pairs: []rhymePair{{0, 1}}}
	res := checkSection([]string{
		"We dance into the night",
		"Everything will be light",
	}, spec, map[string]bool{})
	if res.ok() {
		t.Fatal("night/light must be rejected")
	}
	res = checkSection([]string{
		"The neon signs are humming",
		"A better day is coming",
	}, spec, map[string]bool{})
	if res.ok() {
		t.Fatal("neon must be rejected")
	}
}

func TestCheckSectionEndWordDiscipline(t *testing.T) {
	spec := sectionSpec{kind: "verse", lines: 2}
	res := checkSection([]string{
		"We walk along the road",
		"We carry down the road",
	}, spec, map[string]bool{})
	if res.ok() {
		t.Fatal("repeated end word must be rejected")
	}
	res = checkSection([]string{
		"We walk along the road",
		"The morning sun is warm",
	}, spec, map[string]bool{"road": true})
	if res.ok() {
		t.Fatal("end word used elsewhere in the song must be rejected")
	}
}

func TestCheckSectionRequiresMentions(t *testing.T) {
	spec := sectionSpec{kind: "verse", lines: 2, mentions: []string{"mall", "shopping"}}
	res := checkSection([]string{
		"We are in California",
		"Singing all day long",
	}, spec, map[string]bool{})
	if res.ok() {
		t.Fatal("off-topic section must be rejected")
	}
	res = checkSection([]string{
		"We are at the shopping mall",
		"Singing all day long",
	}, spec, map[string]bool{})
	for _, h := range res.hard {
		if strings.Contains(h, "mention") {
			t.Fatalf("on-topic section flagged: %v", res.hard)
		}
	}
}

func TestParseSectionLines(t *testing.T) {
	raw := "Here are the lyrics:\n```\n1. First line here\n2) Second line here\n- Third line here\n[Chorus]\n\"Fourth line here\"\nextra line beyond\n```"
	got := parseSectionLines(raw, 4)
	want := []string{"First line here", "Second line here", "Third line here", "Fourth line here"}
	if len(got) != 4 {
		t.Fatalf("parsed %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q want %q", i, got[i], want[i])
		}
	}
}

func TestPolishLine(t *testing.T) {
	if got := polishLine("never let it go. "); got != "Never let it go" {
		t.Errorf("polishLine = %q", got)
	}
}

func TestCheckSong(t *testing.T) {
	if probs := checkSong([]string{"Same line", "Same line", "Same line"}); len(probs) == 0 {
		t.Error("triple repeat must be reported")
	}
	lines := make([]string, 17)
	for i := range lines {
		lines[i] = fmt.Sprintf("A different line number %d here", i)
	}
	if probs := checkSong(lines); len(probs) != 0 {
		t.Errorf("healthy song flagged: %v", probs)
	}
}

func TestScribeSectionsPassValidatorsWithMenus(t *testing.T) {
	// The obedient fake follows the menus; every section should pass
	// on the first attempt, so write calls == unique sections.
	fake := newScribeFake()
	_, err := (&Scribe{}).Generate(context.Background(), fake, LyricsRequest{
		Style: "pop", Theme: "a good day in the sun",
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if fake.writeCalls != 4 {
		reqs := strings.Join(fake.requests, "\n=====\n")
		t.Errorf("write calls = %d (repairs happened); requests:\n%s", fake.writeCalls, reqs)
	}
}

func TestScribeAvoidHooksReachBrief(t *testing.T) {
	fake := newScribeFake()
	(&Scribe{}).Generate(context.Background(), fake, LyricsRequest{
		Style: "pop", Theme: "shopping",
		AvoidHooks: []string{"We are shopping at the mall"},
	})
	if len(fake.requests) == 0 || !strings.Contains(fake.requests[0], "do not reuse") {
		t.Error("avoid-hooks must reach the planning call")
	}
	_ = prosody.Known
}

// The engine sang "Dumdam dumdam dumdam" through the [Intro], [Bridge]
// and [Outro] of every track for a day. The filler check only knew a
// fixed list of vocables (oh, ooh, na, la, ...), and an invented
// syllable is not on any list - the tell is that the line is one token
// repeated, which reads the same in every language.
func TestChantLine(t *testing.T) {
	for _, tc := range []struct {
		line string
		want bool
	}{
		{"Dumdam dumdam dumdam", true},
		{"Dumdam dumdam dumdam dumdam", true},
		{"doo doo doo", true},
		{"oh oh oh yeah", true},
		{"la la la", true},
		// Two of a word is a hook, not filler.
		{"Run, run", false},
		{"go go", false},
		// Real lines, in the languages the radio actually sings in.
		{"Tumakbo ako, sumira sa mga kadena", false},
		{"I break the chains they welded shut", false},
		{"Ang apoy sa dibdib ko ay laging", false},
		{"", false},
		// A repeated word among others is ordinary emphasis.
		{"run run into the light", false},
	} {
		if got := chantLine(tc.line); got != tc.want {
			t.Errorf("chantLine(%q) = %v, want %v", tc.line, got, tc.want)
		}
	}
}
