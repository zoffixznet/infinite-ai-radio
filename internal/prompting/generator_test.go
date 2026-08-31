package prompting

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"iar/internal/session"
)

func TestGeneratorRegistry(t *testing.T) {
	gens := Generators()
	if len(gens) < 2 {
		t.Fatalf("want at least scribe and smoothbrain, got %d", len(gens))
	}
	if gens[0].Name() != DefaultGeneratorName {
		t.Fatalf("default generator must be listed first, got %s", gens[0].Name())
	}
	for _, name := range []string{"scribe", "SCRIBE", " smoothbrain "} {
		if _, ok := GeneratorByName(name); !ok {
			t.Fatalf("lookup failed for %q", name)
		}
	}
	if _, ok := GeneratorByName("bogus"); ok {
		t.Fatal("bogus generator resolved")
	}
	for _, g := range gens {
		if g.Blurb() == "" || g.Timeout() <= 0 {
			t.Fatalf("%s needs a blurb and a positive timeout", g.Name())
		}
	}
}

func TestBuilderGeneratorResolution(t *testing.T) {
	b := NewBuilder(nil, nil)
	s := session.New()
	if got := b.GeneratorName(s); got != DefaultGeneratorName {
		t.Fatalf("builtin default = %s", got)
	}
	b.SetDefaultGenerator("smoothbrain")
	if got := b.GeneratorName(s); got != "smoothbrain" {
		t.Fatalf("configured default = %s", got)
	}
	s.LyricsGenerator = "scribe"
	if got := b.GeneratorName(s); got != "scribe" {
		t.Fatalf("session override = %s", got)
	}
	s.LyricsGenerator = "gone-generator"
	if got := b.GeneratorName(s); got != "smoothbrain" {
		t.Fatalf("unknown session name must fall back to the default, got %s", got)
	}
}

// TestBuildLyricsRotatesFreshSheets: consecutive tracks in one steering
// context get fresh lyrics, not one cached sheet forever.
func TestBuildLyricsRotatesFreshSheets(t *testing.T) {
	f := &fakeOllama{replies: []string{
		"[verse]\nfirst take\n[chorus]\nhook one",
		"[verse]\nsecond take\n[chorus]\nhook two",
	}}
	srv := f.server(t)
	defer srv.Close()
	b := probedBuilder(t, srv)
	s := session.New()
	s.Vocal = true
	s.LyricsTheme = "winning"
	s.LyricsGenerator = "smoothbrain"

	if spec := b.BuildSpec(context.Background(), s, 90); spec.SampleQuery == "" {
		t.Fatalf("first vocal build should fall back to the planner, got %+v", spec)
	}
	waitCond(t, "first sheet", func() bool {
		return strings.Contains(b.BuildSpec(context.Background(), s, 90).Lyrics, "hook one")
	})
	// Consuming the first sheet starts the next write; a later track
	// picks up different words.
	waitCond(t, "second sheet", func() bool {
		return strings.Contains(b.BuildSpec(context.Background(), s, 90).Lyrics, "hook two")
	})
}

// TestBuildLyricsReusesLastWhileWriting: when no fresh sheet is ready,
// the previous one is reused rather than dropping to machine-invented
// planner lyrics.
func TestBuildLyricsReusesLastWhileWriting(t *testing.T) {
	f := &fakeOllama{
		replies:  []string{"[verse]\nonly take\n[chorus]\nthe hook"},
		maxFills: 1,
	}
	srv := f.server(t)
	defer srv.Close()
	b := probedBuilder(t, srv)
	s := session.New()
	s.Vocal = true
	s.LyricsTheme = "winning"
	s.LyricsGenerator = "smoothbrain"

	b.BuildSpec(context.Background(), s, 90)
	waitCond(t, "sheet ready", func() bool {
		return strings.Contains(b.BuildSpec(context.Background(), s, 90).Lyrics, "the hook")
	})
	spec := b.BuildSpec(context.Background(), s, 90)
	if !strings.Contains(spec.Lyrics, "the hook") || spec.SampleQuery != "" {
		t.Fatalf("stale sheet must be reused while the next write runs, got %+v", spec)
	}
}

// fakeLLM drives a generator directly, without an HTTP fake.
type fakeLLM struct {
	chat     func(system, user string) (string, error)
	chatJSON func(system, user string, schema any) (string, error)
	chatWith func(system, user string, opts ChatOpts) (string, error)
}

func (f *fakeLLM) Chat(_ context.Context, system, user string) (string, error) {
	return f.chat(system, user)
}

func (f *fakeLLM) ChatJSON(_ context.Context, system, user string, schema any) (string, error) {
	return f.chatJSON(system, user, schema)
}

func (f *fakeLLM) ChatWith(_ context.Context, system, user string, opts ChatOpts) (string, error) {
	if f.chatWith != nil {
		return f.chatWith(system, user, opts)
	}
	if opts.Format != nil && f.chatJSON != nil {
		return f.chatJSON(system, user, opts.Format)
	}
	return f.chat(system, user)
}

func TestSmoothbrainKeepsLegacyPrompt(t *testing.T) {
	var gotSystem, gotUser string
	llm := &fakeLLM{chat: func(system, user string) (string, error) {
		gotSystem, gotUser = system, user
		return "[verse]\nok", nil
	}}
	sb := &Smoothbrain{}
	out, err := sb.Generate(context.Background(), llm, LyricsRequest{
		Style: "lofi beats, calm", Theme: "winning", Seconds: 90,
	})
	if err != nil || out == "" {
		t.Fatalf("generate: %v %q", err, out)
	}
	if !strings.Contains(gotSystem, "8-14 short lines") {
		t.Fatalf("system prompt changed: %q", gotSystem)
	}
	if gotUser != "Music style: lofi beats, calm\nLyrics theme: winning" {
		t.Fatalf("user prompt = %q", gotUser)
	}
	sb.Generate(context.Background(), llm, LyricsRequest{Style: "lofi"})
	if !strings.Contains(gotUser, "matching the mood of the music") {
		t.Fatalf("empty theme must keep the legacy default, got %q", gotUser)
	}
}

func TestHookLine(t *testing.T) {
	cases := []struct{ lyrics, want string }{
		{"[verse]\nline a\nline b\n[chorus]\nthe hook\nmore", "the hook"},
		{"[verse]\nonly verse lines", "only verse lines"},
		{"[Chorus - big]\nshouted hook", "shouted hook"},
		{"", ""},
	}
	for _, c := range cases {
		if got := hookLine(c.lyrics); got != c.want {
			t.Fatalf("hookLine(%q) = %q, want %q", c.lyrics, got, c.want)
		}
	}
}

func TestOllamaSkipsSpecialPurposeModels(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/tags", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"models": []map[string]any{
				{"name": "qwen2.5-coder:7b"},
				{"name": "nomic-embed-text"},
				{"name": "ornith:9b", "capabilities": []string{"completion", "thinking"}},
			},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	o := NewOllama(srv.URL, "", 0)
	if !o.Available(context.Background()) {
		t.Fatal("daemon should be available")
	}
	if o.Model() != "ornith:9b" {
		t.Fatalf("resolved %q; must skip coder and embedding models", o.Model())
	}
	if !o.thinkOK {
		t.Error("thinking capability not recorded")
	}
	// A pinned model name is always respected.
	o2 := NewOllama(srv.URL, "qwen2.5-coder:7b", 0)
	o2.Available(context.Background())
	if o2.Model() != "qwen2.5-coder:7b" {
		t.Fatalf("pinned model overridden: %q", o2.Model())
	}
}

func TestChatWithPinsHelperOffTheGPU(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/chat" {
			json.NewDecoder(r.Body).Decode(&got)
			json.NewEncoder(w).Encode(map[string]any{"message": map[string]string{"content": "ok"}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"models": []map[string]any{{"name": "m"}}})
	}))
	defer srv.Close()

	o := NewOllama(srv.URL, "m", 0)
	if _, err := o.Chat(context.Background(), "s", "u"); err != nil {
		t.Fatal(err)
	}
	opts, _ := got["options"].(map[string]any)
	if v, ok := opts["num_gpu"]; !ok || v != float64(0) {
		t.Fatalf("num_gpu = %v (present=%v); want 0 on every call", v, ok)
	}

	got = nil
	free := NewOllama(srv.URL, "m", -1)
	if _, err := free.Chat(context.Background(), "s", "u"); err != nil {
		t.Fatal(err)
	}
	opts, _ = got["options"].(map[string]any)
	if _, ok := opts["num_gpu"]; ok {
		t.Fatal("num_gpu sent despite gpu_layers=-1; want the daemon left to decide")
	}
}
