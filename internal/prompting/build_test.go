package prompting

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"bgm/internal/engine"
	"bgm/internal/session"
)

// fakeOllama serves the two endpoints the client uses.
func fakeOllama(t *testing.T, reply string, calls *int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/tags", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"models": []map[string]string{{"name": "test-model"}},
		})
	})
	mux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		*calls++
		var req struct {
			Model    string `json:"model"`
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
		json.NewEncoder(w).Encode(map[string]any{
			"message": map[string]string{"role": "assistant", "content": reply},
		})
	})
	return httptest.NewServer(mux)
}

func TestBuildSpecInstrumentalWithOllamaRewrite(t *testing.T) {
	calls := 0
	srv := fakeOllama(t, "dreamy lofi, slow tempo, soft piano", &calls)
	defer srv.Close()
	oll := NewOllama(srv.URL, "")
	if !oll.Available(context.Background()) {
		t.Fatal("fake ollama not available")
	}
	b := NewBuilder(oll, nil)
	s := session.New()
	Steer(s, "dreamier")
	spec := b.BuildSpec(context.Background(), s, 120)
	if spec.Prompt != "dreamy lofi, slow tempo, soft piano" {
		t.Fatalf("prompt = %q", spec.Prompt)
	}
	if spec.Lyrics != engine.InstrumentalLyrics {
		t.Fatalf("lyrics = %q; want instrumental", spec.Lyrics)
	}
	if spec.Seconds != 120 {
		t.Fatalf("seconds = %d", spec.Seconds)
	}
	// Second build with unchanged context must hit the cache.
	b.BuildSpec(context.Background(), s, 120)
	if calls != 1 {
		t.Fatalf("ollama called %d times; want 1 (cached)", calls)
	}
}

func TestBuildSpecVocalWithOllamaLyrics(t *testing.T) {
	calls := 0
	srv := fakeOllama(t, "[verse]\nclimb the hill\n[chorus]\nwin the day", &calls)
	defer srv.Close()
	oll := NewOllama(srv.URL, "")
	oll.Available(context.Background())
	b := NewBuilder(oll, nil)
	s := session.New()
	s.Vocal = true
	s.LyricsTheme = "winning"
	spec := b.BuildSpec(context.Background(), s, 90)
	if !strings.Contains(spec.Lyrics, "[chorus]") {
		t.Fatalf("lyrics = %q", spec.Lyrics)
	}
	if !spec.Vocal() {
		t.Fatal("spec should be vocal")
	}
	if spec.SampleQuery != "" {
		t.Fatal("sample query should be empty when lyrics were written")
	}
}

func TestBuildSpecVocalWithoutOllamaUsesSampleQuery(t *testing.T) {
	b := NewBuilder(nil, nil)
	s := session.New()
	s.Vocal = true
	s.LyricsTheme = "never giving up"
	spec := b.BuildSpec(context.Background(), s, 90)
	if spec.SampleQuery == "" {
		t.Fatal("sample query empty; vocals need the engine's planner")
	}
	if !strings.Contains(spec.SampleQuery, "never giving up") {
		t.Fatalf("theme missing from query: %q", spec.SampleQuery)
	}
	if !spec.Vocal() {
		t.Fatal("spec should report vocal")
	}
}

func TestBuilderDisablesOllamaAfterRepeatedFailures(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/tags") {
			json.NewEncoder(w).Encode(map[string]any{"models": []map[string]string{{"name": "m"}}})
			return
		}
		calls++
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	oll := NewOllama(srv.URL, "")
	oll.Available(context.Background())
	b := NewBuilder(oll, nil)
	s := session.New()
	Steer(s, "calmer")
	for i := 0; i < 5; i++ {
		Steer(s, "tweak number "+string(rune('a'+i)))
		b.BuildSpec(context.Background(), s, 60)
	}
	if calls > maxOllamaFailures {
		t.Fatalf("ollama called %d times; should be disabled after %d failures", calls, maxOllamaFailures)
	}
	if b.ollamaUsable() {
		t.Fatal("builder should have disabled ollama")
	}
}

func TestBuildSpecFallsBackWhenOllamaFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/tags") {
			json.NewEncoder(w).Encode(map[string]any{"models": []map[string]string{{"name": "m"}}})
			return
		}
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	oll := NewOllama(srv.URL, "")
	oll.Available(context.Background())
	b := NewBuilder(oll, nil)
	s := session.New()
	s.BasePrompt = "ambient pads"
	Steer(s, "calmer")
	spec := b.BuildSpec(context.Background(), s, 60)
	if !strings.HasPrefix(spec.Prompt, "ambient pads") {
		t.Fatalf("deterministic fallback not used: %q", spec.Prompt)
	}
}
