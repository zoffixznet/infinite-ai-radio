package acestep

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"iar/internal/audio"
	"iar/internal/engine"
)

// fakeServer mimics the ACE-Step API: release_task, query_result (pending
// once, then done) and audio download.
type fakeServer struct {
	t        *testing.T
	polls    int
	pending  int  // how many times to report pending before success
	failTask bool // report status 2
	lastReq  GenerateRequest
	wavBytes []byte
	prompt   string
	lyrics   string
}

func wrap(data any) map[string]any {
	return map[string]any{"data": data, "code": 200, "error": nil, "timestamp": 0}
}

func (f *fakeServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(wrap(map[string]any{
			"status": "ok", "models_initialized": true,
			"loaded_model": "acestep-v15-turbo",
		}))
	})
	mux.HandleFunc("/release_task", func(w http.ResponseWriter, r *http.Request) {
		f.lastReq = GenerateRequest{}
		if err := json.NewDecoder(r.Body).Decode(&f.lastReq); err != nil {
			f.t.Errorf("bad release_task body: %v", err)
		}
		json.NewEncoder(w).Encode(wrap(map[string]any{"task_id": "task-1", "status": "queued"}))
	})
	mux.HandleFunc("/query_result", func(w http.ResponseWriter, r *http.Request) {
		f.polls++
		status := 0
		result := ""
		if f.failTask {
			status = 2
		} else if f.polls > f.pending {
			status = 1
			inner, _ := json.Marshal([]map[string]any{{
				"file":       "/v1/audio?path=%2Ftmp%2Fout.wav",
				"status":     1,
				"prompt":     f.prompt,
				"lyrics":     f.lyrics,
				"seed_value": "42",
			}})
			result = string(inner)
		}
		json.NewEncoder(w).Encode(wrap([]map[string]any{{
			"task_id": "task-1", "status": status, "result": result,
		}}))
	})
	mux.HandleFunc("/v1/audio", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("path") == "" {
			http.Error(w, "no path", 400)
			return
		}
		w.Write(f.wavBytes)
	})
	return mux
}

func testWAV(frames int) []byte {
	samples := make([]int16, frames*audio.Channels)
	for i := range samples {
		samples[i] = int16(i % 3000)
	}
	return audio.EncodeWAV(samples)
}

func TestClientGenerateFullFlow(t *testing.T) {
	f := &fakeServer{t: t, pending: 2, wavBytes: testWAV(4800), prompt: "used prompt", lyrics: "[Instrumental]"}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	c := NewClient(srv.URL)
	c.PollInterval = 10 * time.Millisecond

	res, err := c.Generate(context.Background(), GenerateRequest{
		Prompt: "lofi", Lyrics: "[Instrumental]", AudioFormat: "wav",
		AudioDuration: 30, InferenceSteps: 8, BatchSize: 1, Thinking: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Samples) != 4800*audio.Channels {
		t.Fatalf("samples = %d", len(res.Samples))
	}
	if res.Prompt != "used prompt" || res.Seed != "42" {
		t.Fatalf("metadata = %+v", res)
	}
	if f.lastReq.AudioFormat != "wav" {
		t.Fatalf("audio format sent = %q; must request wav explicitly", f.lastReq.AudioFormat)
	}
	if f.polls < 3 {
		t.Fatalf("polled %d times; want at least 3", f.polls)
	}
}

func TestClientGenerateTaskFailure(t *testing.T) {
	f := &fakeServer{t: t, failTask: true, wavBytes: testWAV(10)}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	c := NewClient(srv.URL)
	c.PollInterval = 10 * time.Millisecond
	_, err := c.Generate(context.Background(), GenerateRequest{AudioFormat: "wav"})
	if err == nil {
		t.Fatal("failed task reported success")
	}
}

func TestClientGenerateContextCancel(t *testing.T) {
	f := &fakeServer{t: t, pending: 1000, wavBytes: testWAV(10)}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	c := NewClient(srv.URL)
	c.PollInterval = 10 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := c.Generate(ctx, GenerateRequest{AudioFormat: "wav"}); err == nil {
		t.Fatal("canceled generate returned success")
	}
}

func TestClientHealth(t *testing.T) {
	f := &fakeServer{t: t}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	h, err := NewClient(srv.URL).Health(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !h.ModelsInitialized || h.LoadedModel != "acestep-v15-turbo" {
		t.Fatalf("health = %+v", h)
	}
}

func TestEngineMapsSpecs(t *testing.T) {
	f := &fakeServer{t: t, wavBytes: testWAV(100), prompt: "p", lyrics: "some lyrics"}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	client := NewClient(srv.URL)
	client.PollInterval = 10 * time.Millisecond
	sidecar := NewSidecar(SidecarConfig{Port: 1}, client, discardLogger())
	sidecar.mu.Lock()
	sidecar.ready = true
	sidecar.mu.Unlock()
	eng := NewEngine(sidecar, Options{InferenceSteps: 8, Thinking: true})

	// Instrumental spec. A piece is as long as it wants to be, so no
	// duration is sent: the server turns one into a hard quota on the
	// audio-code stream and cuts the arrangement to fit it.
	_, err := eng.Generate(context.Background(), engine.Spec{
		Prompt: "lofi", Lyrics: engine.InstrumentalLyrics, Seconds: 60, Seed: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if f.lastReq.Lyrics != engine.InstrumentalLyrics || f.lastReq.SampleMode {
		t.Fatalf("instrumental request wrong: %+v", f.lastReq)
	}
	if !f.lastReq.UseRandomSeed {
		t.Fatal("seed -1 should use random seed")
	}
	if f.lastReq.AudioDuration != 0 {
		t.Fatalf("instrumental sent duration %v; want none, so the engine picks a length", f.lastReq.AudioDuration)
	}

	// The one caller that does need a length says so: the hurry-up
	// first track, which trades a fitting length for playing sooner.
	_, err = eng.Generate(context.Background(), engine.Spec{
		Prompt: "lofi", Lyrics: engine.InstrumentalLyrics, Seconds: 60, Seed: -1, ExactSeconds: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if f.lastReq.AudioDuration != 60 {
		t.Fatalf("an exact-length request sent duration %v; want 60", f.lastReq.AudioDuration)
	}

	// Vocal with written lyrics: the words are the song, so no
	// duration is sent and the engine derives the length from them.
	_, err = eng.Generate(context.Background(), engine.Spec{
		Prompt: "rock", Lyrics: "[Verse 1]\nreal words here", Seconds: 60, Seed: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if f.lastReq.AudioDuration != 0 {
		t.Fatalf("vocal lyric track sent duration %v; want none", f.lastReq.AudioDuration)
	}
	if f.lastReq.SampleMode {
		t.Fatalf("vocal lyric track went to sample mode: %+v", f.lastReq)
	}

	// Vocal via sample query (no local lyrics).
	_, err = eng.Generate(context.Background(), engine.Spec{
		SampleQuery: "rock with vocals about winning", Seconds: 60, Seed: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !f.lastReq.SampleMode || f.lastReq.SampleQuery == "" || !f.lastReq.Thinking {
		t.Fatalf("sample mode request wrong: %+v", f.lastReq)
	}

	// Structured constraints and negatives reach the request; the
	// negatives ride the planner LM's negative prompt with raised
	// guidance, never the caption.
	_, err = eng.Generate(context.Background(), engine.Spec{
		Prompt: "techno, synths", Lyrics: engine.InstrumentalLyrics, Seconds: 60, Seed: -1,
		BPM: 128, KeyScale: "C minor", TimeSignature: "4", VocalLanguage: "en",
		NegativePrompt: "guitars, guitar", LMCfgScale: 3.25,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := f.lastReq
	if req.BPM != 128 || req.KeyScale != "C minor" || req.TimeSignature != "4" || req.VocalLanguage != "en" {
		t.Fatalf("structured fields wrong: %+v", req)
	}
	if req.LMNegativePrompt != "guitars, guitar" || req.LMCfgScale != 3.25 {
		t.Fatalf("negative conditioning wrong: %+v", req)
	}
	if strings.Contains(req.Prompt, "guitar") {
		t.Fatalf("negatives leaked into the caption: %q", req.Prompt)
	}

	// Without thinking there is no negative lever: the fields drop out.
	engNoThink := NewEngine(sidecar, Options{InferenceSteps: 8, Thinking: false})
	_, err = engNoThink.Generate(context.Background(), engine.Spec{
		Prompt: "techno", Lyrics: engine.InstrumentalLyrics, Seconds: 60, Seed: -1,
		NegativePrompt: "guitars", LMCfgScale: 3.25,
	})
	if err != nil {
		t.Fatal(err)
	}
	if f.lastReq.LMNegativePrompt != "" || f.lastReq.LMCfgScale != 0 {
		t.Fatalf("negative fields sent without thinking: %+v", f.lastReq)
	}
}

// discardLogger returns a logger that drops everything.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// The engine reports why a job failed inside the encoded result
// payload, not beside it. Losing that made every failure look alike,
// which is why the known-fatal device fault went unrecognized.
func TestClientCarriesTheEngineFailureReason(t *testing.T) {
	cases := []struct {
		name    string
		entry   map[string]any
		wantSub string
	}{
		{
			name: "reason inside the result payload",
			entry: map[string]any{
				"task_id": "t1", "status": 2,
				"result": `[{"file":"","status":2,"error":"create_sample failed: RuntimeError: Expected all tensors to be on the same device, but found at least two devices, cuda:0 and cpu!"}]`,
			},
			wantSub: "Expected all tensors to be on the same device",
		},
		{
			name: "an out-of-memory reason survives too",
			entry: map[string]any{
				"task_id": "t1", "status": 2,
				"result": `[{"file":"","status":2,"error":"OutOfMemoryError: CUDA out of memory. Tried to allocate 20.00 MiB"}]`,
			},
			wantSub: "CUDA out of memory",
		},
		{
			name: "the last log line stands in when nothing else has it",
			entry: map[string]any{
				"task_id": "t1", "status": 2,
				"result":        `[{"file":"","status":2,"error":null}]`,
				"progress_text": "  the engine gave up  ",
			},
			wantSub: "the engine gave up",
		},
		{
			name: "a top-level error still wins",
			entry: map[string]any{
				"task_id": "t1", "status": 2,
				"error":  "plain reason",
				"result": `[{"file":"","status":2,"error":"ignored"}]`,
			},
			wantSub: "plain reason",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if strings.HasSuffix(r.URL.Path, "/release_task") {
					json.NewEncoder(w).Encode(wrap(map[string]any{"task_id": "t1"}))
					return
				}
				json.NewEncoder(w).Encode(wrap([]map[string]any{c.entry}))
			}))
			defer srv.Close()
			cl := NewClient(srv.URL)
			cl.PollInterval = 5 * time.Millisecond
			_, err := cl.Generate(context.Background(), GenerateRequest{AudioFormat: "wav"})
			if err == nil {
				t.Fatal("failed task reported success")
			}
			if !strings.Contains(err.Error(), c.wantSub) {
				t.Fatalf("reason lost: %q does not contain %q", err.Error(), c.wantSub)
			}
		})
	}
}
