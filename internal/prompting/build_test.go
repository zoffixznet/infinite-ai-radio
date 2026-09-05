package prompting

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"iar/internal/engine"
	"iar/internal/session"
)

// fakeOllama serves the endpoints the client uses. The probe chat always
// succeeds; later chats behave per the mode fields.
type fakeOllama struct {
	reply     string
	jsonReply string
	failFills bool
	slow      time.Duration
	chats     atomic.Int32
	fills     atomic.Int32
	jsonCalls atomic.Int32
	// replies, when set, serves fill n the n-th entry (the last one
	// repeats). maxFills, when set, fails every fill beyond it.
	replies  []string
	maxFills int32
}

func (f *fakeOllama) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/tags", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"models": []map[string]string{{"name": "test-model"}},
		})
	})
	mux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		f.chats.Add(1)
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
			Stream *bool           `json:"stream"`
			Format json.RawMessage `json:"format"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("bad chat request: %v", err)
		}
		if req.Stream == nil || *req.Stream {
			t.Error("stream must be explicitly false")
		}
		isProbe := len(req.Messages) > 1 && strings.Contains(req.Messages[1].Content, "Reply with exactly")
		if isProbe {
			json.NewEncoder(w).Encode(map[string]any{
				"message": map[string]string{"role": "assistant", "content": "OK"},
			})
			return
		}
		f.fills.Add(1)
		if len(req.Format) > 0 {
			f.jsonCalls.Add(1)
		}
		if f.slow > 0 {
			time.Sleep(f.slow)
		}
		if f.failFills || (f.maxFills > 0 && f.fills.Load() > f.maxFills) {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		reply := f.reply
		if n := int(f.fills.Load()) - 1; len(f.replies) > 0 {
			if n >= len(f.replies) {
				n = len(f.replies) - 1
			}
			reply = f.replies[n]
		}
		if len(req.Format) > 0 && f.jsonReply != "" {
			reply = f.jsonReply
		}
		json.NewEncoder(w).Encode(map[string]any{
			"message": map[string]string{"role": "assistant", "content": reply},
		})
	})
	return httptest.NewServer(mux)
}

// probedBuilder returns a builder whose usability probe has completed.
func probedBuilder(t *testing.T, srv *httptest.Server) *Builder {
	t.Helper()
	b := NewBuilder(NewOllama(srv.URL, "", 0), nil)
	b.ProbeAsync(context.Background())
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b.helperUsable() {
			return b
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("helper never became usable")
	return nil
}

func waitCond(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestBuildSpecDeterministicAndRefineMerges(t *testing.T) {
	f := &fakeOllama{jsonReply: `{"genre":["chillhop"],"instruments":{"rhodes piano":2},"mood":["nostalgic"],"bpm":80,"negatives":["harsh highs"]}`}
	srv := f.server(t)
	defer srv.Close()
	b := probedBuilder(t, srv)
	s := session.New()
	s.BasePrompt = "lofi beats"
	Steer(s, "dreamier")

	// The build is deterministic and instant, whatever the helper does.
	spec := b.BuildSpec(context.Background(), s, 120)
	if !strings.HasPrefix(spec.Prompt, "lofi beats") {
		t.Fatalf("first build must be deterministic, got %q", spec.Prompt)
	}
	if spec.Lyrics != engine.InstrumentalLyrics {
		t.Fatalf("lyrics = %q; want instrumental", spec.Lyrics)
	}

	// The helper proposes a schema-constrained JSON update; the merge is
	// validated and lands asynchronously.
	got := make(chan SpecUpdate, 1)
	b.RefineAsync(s.Snapshot(), "dreamier", func(u SpecUpdate) { got <- u })
	var u SpecUpdate
	select {
	case u = <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("refine result never arrived")
	}
	if f.jsonCalls.Load() != 1 {
		t.Fatalf("json-constrained calls = %d; want 1", f.jsonCalls.Load())
	}
	if !MergeUpdate(s, u) {
		t.Fatal("merge changed nothing")
	}
	r := Render(s)
	if !strings.Contains(r.Caption, "chillhop") || !strings.Contains(r.Caption, "rich rhodes piano") {
		t.Fatalf("caption after merge = %q", r.Caption)
	}
	if r.BPM != 80 || !strings.Contains(r.NegativePrompt, "harsh highs") {
		t.Fatalf("fields after merge: bpm=%d neg=%q", r.BPM, r.NegativePrompt)
	}
	// A user-negated item can never come back through a helper update.
	Steer(s, "no piano")
	MergeUpdate(s, u)
	if r := Render(s); strings.Contains(r.Caption, "piano") {
		t.Fatalf("helper resurrected a negated instrument: %q", r.Caption)
	}
}

func TestHelperUpdatesNeverInstallALanguage(t *testing.T) {
	// The language has a control of its own. A background guess that
	// writes one is invisible, sticky, and outranks every list the
	// listener edits afterwards, so the schema must not even offer it.
	if _, ok := specUpdateSchema["properties"].(map[string]any)["vocal_language"]; ok {
		t.Fatal("the helper is still offered a vocal_language field")
	}
	s := session.New()
	s.BasePrompt = "chanson"
	if MergeUpdate(s, SpecUpdate{Genre: []string{"chanson"}}); s.Spec.VocalLanguage != "" {
		t.Fatalf("a helper update set the language to %q", s.Spec.VocalLanguage)
	}
	if s.Spec.LanguagePinned {
		t.Fatal("a helper update pinned the language")
	}
}

func TestSanitizeUpdateClampsHostileValues(t *testing.T) {
	u := SpecUpdate{
		Genre:         []string{" TECHNO ", "", strings.Repeat("x", 300)},
		Instruments:   map[string]int{"kick": 9, "hats": -2, "": 1},
		BPM:           9999,
		KeyScale:      "Q weird",
		TimeSignature: "17",
	}
	sanitizeUpdate(&u)
	if u.Instruments["kick"] != 3 {
		t.Fatalf("weight not clamped: %+v", u.Instruments)
	}
	if _, ok := u.Instruments["hats"]; ok {
		t.Fatal("non-positive weight kept as instrument")
	}
	found := false
	for _, n := range u.Reduce {
		if n == "hats" {
			found = true
		}
	}
	if !found || len(u.Negatives) != 0 {
		t.Fatalf("non-positive weight must dial down, not negate: reduce=%+v negatives=%+v", u.Reduce, u.Negatives)
	}
	if u.BPM != 300 || u.KeyScale != "" || u.TimeSignature != "" {
		t.Fatalf("invalid fields kept: %+v", u)
	}
	if len(u.Genre) != 2 || u.Genre[0] != "techno" || len(u.Genre[1]) > 40 {
		t.Fatalf("genre not cleaned: %+v", u.Genre)
	}
}

func TestBuildSpecNeverBlocksOnSlowHelper(t *testing.T) {
	f := &fakeOllama{reply: "slow reply", slow: 2 * time.Second}
	srv := f.server(t)
	defer srv.Close()
	b := probedBuilder(t, srv)
	s := session.New()
	Steer(s, "calmer")

	start := time.Now()
	spec := b.BuildSpec(context.Background(), s, 60)
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Fatalf("BuildSpec took %s; must not wait for the helper", d)
	}
	if !strings.HasPrefix(spec.Prompt, s.BasePrompt) {
		t.Fatalf("prompt = %q", spec.Prompt)
	}
}

func TestBuildSpecVocalLyricsArriveInBackground(t *testing.T) {
	f := &fakeOllama{reply: "[verse]\nclimb the hill\n[chorus]\nwin the day"}
	srv := f.server(t)
	defer srv.Close()
	b := probedBuilder(t, srv)
	s := session.New()
	s.Vocal = true
	s.LyricsTheme = "winning"
	s.LyricsGenerator = "smoothbrain"

	// First vocal build falls back to the engine's planner.
	spec := b.BuildSpec(context.Background(), s, 90)
	if spec.SampleQuery == "" || !strings.Contains(spec.SampleQuery, "winning") {
		t.Fatalf("first vocal build should use sample query, got %+v", spec)
	}
	// After the background lyric write, real lyrics are used.
	waitCond(t, "lyrics ready", func() bool {
		sp := b.BuildSpec(context.Background(), s, 90)
		return strings.Contains(sp.Lyrics, "[chorus]") && sp.SampleQuery == ""
	})
}

func TestBuildSpecVocalWithoutHelperUsesSampleQuery(t *testing.T) {
	b := NewBuilder(nil, nil)
	s := session.New()
	s.Vocal = true
	s.LyricsTheme = "never giving up"
	spec := b.BuildSpec(context.Background(), s, 90)
	if spec.SampleQuery == "" || !strings.Contains(spec.SampleQuery, "never giving up") {
		t.Fatalf("sample query wrong: %q", spec.SampleQuery)
	}
	if !spec.Vocal() {
		t.Fatal("spec should report vocal")
	}
}

func TestBuilderRestsHelperAfterRepeatedFailures(t *testing.T) {
	f := &fakeOllama{failFills: true}
	srv := f.server(t)
	defer srv.Close()
	b := probedBuilder(t, srv)
	s := session.New()
	for i := 0; i < 5; i++ {
		raw := "tweak number " + string(rune('a'+i))
		Steer(s, raw)
		b.RefineAsync(s.Snapshot(), raw, func(SpecUpdate) {})
		time.Sleep(50 * time.Millisecond)
	}
	waitCond(t, "helper resting", func() bool { return !b.helperUsable() })
	if f.fills.Load() > maxOllamaFailures {
		t.Fatalf("helper called %d times; should stop after %d failures", f.fills.Load(), maxOllamaFailures)
	}
	// The rest must expire. Standing the helper down for the rest of the
	// run turns one busy minute on a shared graphics card into an
	// evening of engine-invented lyrics, in whatever language it likes.
	b.mu.Lock()
	b.restAfter = time.Now().Add(-time.Second)
	b.mu.Unlock()
	if !b.helperUsable() {
		t.Fatal("the helper never came back after its rest")
	}
}

func TestBuilderUnusableWithoutProbe(t *testing.T) {
	f := &fakeOllama{reply: "never used"}
	srv := f.server(t)
	defer srv.Close()
	// No ProbeAsync: the helper must not be consulted at all.
	b := NewBuilder(NewOllama(srv.URL, "", 0), nil)
	s := session.New()
	Steer(s, "calmer")
	spec := b.BuildSpec(context.Background(), s, 60)
	if !strings.HasPrefix(spec.Prompt, s.BasePrompt) {
		t.Fatalf("prompt = %q", spec.Prompt)
	}
	b.RefineAsync(s.Snapshot(), "calmer", func(SpecUpdate) { t.Error("refine ran without a probe") })
	time.Sleep(100 * time.Millisecond)
	if f.chats.Load() != 0 {
		t.Fatalf("helper consulted %d times without a successful probe", f.chats.Load())
	}
}

// TestPresetSpecReachesEngine asserts a preset's structured constraints
// (tempo, language, negatives) survive all the way into the engine
// request, for both instrumental and vocal presets.
func TestPresetSpecReachesEngine(t *testing.T) {
	b := NewBuilder(nil, nil)

	drive, err := session.LookupPreset("night-drive")
	if err != nil {
		t.Fatal(err)
	}
	spec := b.BuildSpec(context.Background(), session.FromPreset(drive), 120)
	// Presets pin no tempo - the model chooses per track, part of what
	// keeps a station varied - so bpm stays unset here.
	if spec.BPM != 0 {
		t.Fatalf("night-drive bpm = %d; want the model's own choice", spec.BPM)
	}
	if !strings.Contains(spec.Prompt, "synthwave") {
		t.Fatalf("night-drive prompt = %q", spec.Prompt)
	}

	nu, err := session.LookupPreset("nu-metal")
	if err != nil {
		t.Fatal(err)
	}
	spec = b.BuildSpec(context.Background(), session.FromPreset(nu), 120)
	if spec.BPM != 0 || spec.VocalLanguage != "en" {
		t.Fatalf("nu-metal fields: bpm=%d lang=%q", spec.BPM, spec.VocalLanguage)
	}
	if !strings.Contains(spec.NegativePrompt, "acoustic guitar") {
		t.Fatalf("nu-metal negatives lost: %q", spec.NegativePrompt)
	}
	if spec.LMCfgScale <= 2.5 {
		t.Fatalf("lm guidance not raised with preset negatives: %v", spec.LMCfgScale)
	}
	if !spec.Vocal() {
		t.Fatal("nu-metal should request vocals")
	}
}

// TestMergeUpdateReduceIsSoft asserts a helper "reduce" proposal takes
// the soft path: weights step down and nothing lands in negatives.
func TestMergeUpdateReduceIsSoft(t *testing.T) {
	s := session.New()
	Steer(s, "more guitars")
	Steer(s, "more guitars") // weight 2
	if !MergeUpdate(s, SpecUpdate{Reduce: []string{"guitars"}}) {
		t.Fatal("reduce merge changed nothing")
	}
	if s.Spec.Instruments["guitars"] != 1 {
		t.Fatalf("weight after reduce = %d; want 1", s.Spec.Instruments["guitars"])
	}
	if len(s.Spec.Negatives) != 0 {
		t.Fatalf("reduce landed in negatives: %+v", s.Spec.Negatives)
	}
	if r := Render(s); r.LMCfgScale != 0 {
		t.Fatalf("reduce raised planner guidance: %v", r.LMCfgScale)
	}
}

// TestTrackTitleDeterministicFallback: with no helper at all, every
// track still gets a finished-looking short name.
func TestTrackTitleDeterministicFallback(t *testing.T) {
	title, subtitle := TrackTitle("lofi chill beats, mellow, warm analog, relaxed, soft piano, calm background music")
	if title != "Lofi Chill Beats" {
		t.Fatalf("title = %q", title)
	}
	if subtitle != "mellow, warm analog, relaxed, soft piano" {
		t.Fatalf("subtitle = %q", subtitle)
	}
	if n := len(strings.Fields(subtitle)); n > 6 {
		t.Fatalf("subtitle has %d words; max 6", n)
	}
	// Long first segments shorten word-aware.
	title, _ = TrackTitle("a very long and winding first segment that keeps going, dark")
	if len(title) > 32 {
		t.Fatalf("title too long: %q", title)
	}
	// Degenerate input still yields something displayable.
	if title, _ := TrackTitle(""); title == "" {
		t.Fatal("empty prompt produced an empty title")
	}
	// A builder with no helper names nothing; the fallback is what
	// ships.
	b := NewBuilder(nil, nil)
	if title, _ := b.titleSong(context.Background(), "dark techno", "[Verse]\nrain on the wire"); title != "" {
		t.Fatalf("helperless builder produced a model title: %q", title)
	}
}

// The song's name arrives through the schema-constrained JSON path,
// sanitized - and it arrives while the words are being written, in the
// same call sequence, so a song is never handed to a listener nameless
// and never renamed under one.
func TestSongIsNamedWhereItsWordsAreWritten(t *testing.T) {
	f := &fakeOllama{jsonReply: `{"title":"  \"Neon Rain\"  ","subtitle":"dark driving techno"}`}
	srv := f.server(t)
	defer srv.Close()
	b := probedBuilder(t, srv)
	title, subtitle := b.titleSong(context.Background(), "dark techno, driving", "[Verse]\nneon rain on the wire")
	if title != "Neon Rain" || subtitle != "dark driving techno" {
		t.Fatalf("model title = %q / %q", title, subtitle)
	}
	if f.jsonCalls.Load() != 1 {
		t.Fatalf("json calls = %d; want 1", f.jsonCalls.Load())
	}
	// An instrumental has no words to name from.
	if got, _ := b.titleSong(context.Background(), "dark techno", engine.InstrumentalLyrics); got != "" {
		t.Fatalf("instrumental named %q", got)
	}
}

// A language named by hand outranks the configured catalogue. The bug
// this covers: the export command set the session's language code but
// not its pin, so the lyric writer went on drawing from the configured
// list - a Spanish export came back sung in whatever the list held.
func TestPinnedLanguageOutranksTheConfiguredList(t *testing.T) {
	b := NewBuilder(nil, nil)
	b.SetLanguages([]string{"Tagalog"})
	s := session.New()
	s.Vocal = true

	// Unpinned: the catalogue decides.
	if got := b.chooseLanguage(s, Render(s)); got.Name != "Tagalog" {
		t.Fatalf("unpinned language = %q, want the configured Tagalog", got.Name)
	}

	// Pinned: the named language wins, and only sheets in it are kept.
	s.Spec.VocalLanguage = "es"
	s.Spec.LanguagePinned = true
	r := Render(s)
	if got := b.chooseLanguage(s, r); got.Code != "es" {
		t.Fatalf("pinned language = %q, want es", got.Code)
	}
	ok := b.langAcceptable(s, r)
	if !ok(Language{Code: "es", Name: "Spanish"}) {
		t.Fatal("a Spanish sheet was rejected under a Spanish pin")
	}
	if ok(Language{Code: "tl", Name: "Tagalog"}) {
		t.Fatal("a Tagalog sheet was accepted under a Spanish pin")
	}
}

// An instrumental session used to reach the engine under the identical
// terse tag list for every track, while every vocal song arrived with a
// description written for it alone. The writer now describes
// instrumentals too, and each track takes one of its own.
func TestInstrumentalTracksGetTheirOwnDescription(t *testing.T) {
	var asked int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/tags") {
			json.NewEncoder(w).Encode(map[string]any{"models": []map[string]any{{"name": "m"}}})
			return
		}
		asked++
		json.NewEncoder(w).Encode(map[string]any{"message": map[string]string{
			"role": "assistant", "content": fmt.Sprintf("A rolling groove number %d, brushed drums and a warm bass walking under a muted keys line.", asked),
		}})
	}))
	defer srv.Close()

	b := NewBuilder(NewOllama(srv.URL, "m", 0), nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.ProbeAsync(ctx)
	if !b.AwaitHelper(ctx, 5*time.Second) {
		t.Fatal("helper never became usable")
	}
	s := session.New()
	s.Vocal = false

	// Nothing stocked: planning should wait rather than send the tag
	// list again, and the spec falls back to the steering caption.
	if !b.AwaitingInstrumentalCaptions(s) {
		t.Fatal("an empty shelf did not report as awaiting")
	}
	bare := b.BuildSpec(ctx, s, 150)
	if bare.Lyrics != engine.InstrumentalLyrics {
		t.Fatalf("instrumental lyrics = %q", bare.Lyrics)
	}

	if wrote := b.StockInstrumentalCaptions(ctx, s, 2, nil); wrote != 2 {
		t.Fatalf("stocked %d descriptions, want 2", wrote)
	}
	if b.AwaitingInstrumentalCaptions(s) {
		t.Fatal("a stocked shelf still reported as awaiting")
	}

	first := b.BuildSpec(ctx, s, 150)
	second := b.BuildSpec(ctx, s, 150)
	if first.Prompt == bare.Prompt {
		t.Fatal("the track was still described by the terse steering caption")
	}
	if first.Prompt == second.Prompt {
		t.Fatalf("two tracks shared one description: %q", first.Prompt)
	}
	for _, spec := range []engine.Spec{first, second} {
		if spec.Lyrics != engine.InstrumentalLyrics {
			t.Fatalf("a described instrumental lost its instrumental marker: %q", spec.Lyrics)
		}
		if spec.ExactSeconds {
			t.Fatal("an ordinary instrumental asked for an exact length")
		}
	}
	// Drained again, the terse caption stands rather than nothing.
	if third := b.BuildSpec(ctx, s, 150); third.Prompt != bare.Prompt {
		t.Fatalf("a bare shelf did not fall back to the steering caption: %q", third.Prompt)
	}
}

// A vocal session must not be touched by any of it.
func TestVocalSessionsIgnoreTheInstrumentalShelf(t *testing.T) {
	b := NewBuilder(nil, nil)
	s := session.New()
	s.Vocal = true
	if b.AwaitingInstrumentalCaptions(s) {
		t.Fatal("a vocal session reported as awaiting instrumental descriptions")
	}
	if n := b.StockInstrumentalCaptions(context.Background(), s, 2, nil); n != 0 {
		t.Fatalf("wrote %d instrumental descriptions for a vocal session", n)
	}
}
