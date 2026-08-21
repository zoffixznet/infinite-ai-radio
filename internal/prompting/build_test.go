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
	failFills bool
	slow      time.Duration
	chats     atomic.Int32
	fills     atomic.Int32
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
			Stream *bool `json:"stream"`
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
		if f.slow > 0 {
			time.Sleep(f.slow)
		}
		if f.failFills {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"message": map[string]string{"role": "assistant", "content": f.reply},
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

func TestBuildSpecDeterministicFirstThenPolished(t *testing.T) {
	f := &fakeOllama{reply: "dreamy lofi, slow tempo, soft piano"}
	srv := f.server(t)
	defer srv.Close()
	b := probedBuilder(t, srv)
	s := session.New()
	s.BasePrompt = "lofi beats"
	Steer(s, "dreamier")

	// First build never waits: deterministic result, helper fill kicked
	// off in the background.
	spec := b.BuildSpec(context.Background(), s, 120)
	if !strings.HasPrefix(spec.Prompt, "lofi beats") {
		t.Fatalf("first build must be deterministic, got %q", spec.Prompt)
	}
	if spec.Lyrics != engine.InstrumentalLyrics {
		t.Fatalf("lyrics = %q; want instrumental", spec.Lyrics)
	}

	// Once the background fill lands, the polished prompt is used.
	waitCond(t, "helper fill", func() bool { return f.fills.Load() >= 1 })
	waitCond(t, "polished prompt", func() bool {
		return b.BuildSpec(context.Background(), s, 120).Prompt == f.reply
	})
	if f.fills.Load() != 1 {
		t.Fatalf("helper called %d times for one context; want 1", f.fills.Load())
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
		Steer(s, "tweak number "+string(rune('a'+i)))
		b.BuildSpec(context.Background(), s, 60)
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
	if f.chats.Load() != 0 {
		t.Fatalf("helper consulted %d times without a successful probe", f.chats.Load())
	}
}
