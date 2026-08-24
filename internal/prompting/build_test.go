package prompting

import (
	"context"
	"encoding/json"
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
		if f.failFills {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		reply := f.reply
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
	b := NewBuilder(NewOllama(srv.URL, ""), nil)
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

func TestSanitizeUpdateClampsHostileValues(t *testing.T) {
	u := SpecUpdate{
		Genre:         []string{" TECHNO ", "", strings.Repeat("x", 300)},
		Instruments:   map[string]int{"kick": 9, "hats": -2, "": 1},
		BPM:           9999,
		KeyScale:      "Q weird",
		TimeSignature: "17",
		VocalLanguage: "klingon",
	}
	sanitizeUpdate(&u)
	if u.Instruments["kick"] != 3 {
		t.Fatalf("weight not clamped: %+v", u.Instruments)
	}
	if _, ok := u.Instruments["hats"]; ok {
		t.Fatal("non-positive weight kept as instrument")
	}
	found := false
	for _, n := range u.Negatives {
		if n == "hats" {
			found = true
		}
	}
	if !found {
		t.Fatalf("negative-weight instrument not negated: %+v", u.Negatives)
	}
	if u.BPM != 300 || u.KeyScale != "" || u.TimeSignature != "" || u.VocalLanguage != "" {
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

func TestBuilderDisablesHelperAfterRepeatedFailures(t *testing.T) {
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
	waitCond(t, "helper disabled", func() bool { return !b.helperUsable() })
	if f.fills.Load() > maxOllamaFailures {
		t.Fatalf("helper called %d times; should stop after %d failures", f.fills.Load(), maxOllamaFailures)
	}
}

func TestBuilderUnusableWithoutProbe(t *testing.T) {
	f := &fakeOllama{reply: "never used"}
	srv := f.server(t)
	defer srv.Close()
	// No ProbeAsync: the helper must not be consulted at all.
	b := NewBuilder(NewOllama(srv.URL, ""), nil)
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
