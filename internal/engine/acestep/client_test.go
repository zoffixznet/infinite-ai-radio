package acestep

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"bgm/internal/audio"
	"bgm/internal/engine"
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

	// Instrumental spec.
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
	if f.lastReq.AudioDuration != 60 {
		t.Fatalf("duration = %v", f.lastReq.AudioDuration)
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
}

// discardLogger returns a logger that drops everything.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
