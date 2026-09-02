package prompting

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// gpuCapture serves the two endpoints ChatWith touches and records the
// num_gpu option of every chat call: the value, and whether it was sent
// at all.
type gpuCapture struct {
	mu   sync.Mutex
	got  []int
	sent []bool
}

func (g *gpuCapture) server() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/tags", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"models": []map[string]any{{"name": "m"}}})
	})
	mux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []any          `json:"messages"`
			Options  map[string]any `json:"options"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if len(req.Messages) == 0 {
			// The eviction request SetEngineBusy(true) fires; not a
			// chat call, so not part of what the tests assert on.
			json.NewEncoder(w).Encode(map[string]any{"message": map[string]string{"role": "assistant", "content": ""}})
			return
		}
		v, ok := req.Options["num_gpu"]
		n := -999
		if ok {
			n = int(v.(float64))
		}
		g.mu.Lock()
		g.got = append(g.got, n)
		g.sent = append(g.sent, ok)
		g.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{
			"message": map[string]string{"role": "assistant", "content": "OK"},
		})
	})
	return httptest.NewServer(mux)
}

func chatOnce(t *testing.T, o *Ollama) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := o.ChatWith(ctx, "sys", "user", ChatOpts{}); err != nil {
		t.Fatal(err)
	}
}

// The default placement (gpu_layers 0) follows the music engine: while
// the engine holds the graphics card the helper is pinned to the CPU;
// the moment the engine hibernates, no num_gpu is sent at all and the
// daemon places the model on the freed card. This is what keeps song
// naming from taking minutes on a busy machine.
func TestHelperPlacementFollowsTheEngine(t *testing.T) {
	g := &gpuCapture{}
	srv := g.server()
	defer srv.Close()
	o := NewOllama(srv.URL, "m", 0)

	o.SetEngineBusy(true)
	chatOnce(t, o)
	o.SetEngineBusy(false)
	chatOnce(t, o)

	if !g.sent[0] || g.got[0] != 0 {
		t.Fatalf("engine busy: num_gpu = %v (sent=%v), want 0", g.got[0], g.sent[0])
	}
	if g.sent[1] {
		t.Fatalf("engine idle: num_gpu was sent (%v); the daemon should place the model", g.got[1])
	}
}

// Explicit settings stay constant regardless of the engine: a positive
// count is always sent, and -1 always leaves placement to the daemon.
func TestHelperPlacementExplicitSettings(t *testing.T) {
	g := &gpuCapture{}
	srv := g.server()
	defer srv.Close()

	pinned := NewOllama(srv.URL, "m", 12)
	pinned.SetEngineBusy(true)
	chatOnce(t, pinned)
	pinned.SetEngineBusy(false)
	chatOnce(t, pinned)

	auto := NewOllama(srv.URL, "m", -1)
	auto.SetEngineBusy(true)
	chatOnce(t, auto)

	if g.got[0] != 12 || g.got[1] != 12 {
		t.Fatalf("pinned layers drifted: %v", g.got[:2])
	}
	if g.sent[2] {
		t.Fatalf("gpu_layers -1 sent num_gpu (%v)", g.got[2])
	}
}

// Going idle also ends any rest the helper was serving: its timeouts
// were the crowded card's fault.
func TestGoingIdleWakesARestingHelper(t *testing.T) {
	g := &gpuCapture{}
	srv := g.server()
	defer srv.Close()
	b := NewBuilder(NewOllama(srv.URL, "m", 0), nil)
	b.mu.Lock()
	b.usable = true
	b.restAfter = time.Now().Add(time.Hour)
	b.mu.Unlock()

	if b.helperUsable() {
		t.Fatal("helper should be resting")
	}
	b.SetEngineBusy(false)
	if !b.helperUsable() {
		t.Fatal("a free card should end the rest")
	}
}
