//go:build live

package prompting

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestScribeLive drives the real local Ollama daemon. Run manually:
//
//	go test -tags live -run TestScribeLive -v -timeout 20m ./internal/prompting/
func TestScribeLive(t *testing.T) {
	oll := NewOllama("http://127.0.0.1:11434", "")
	ctx := context.Background()
	if !oll.Available(ctx) {
		t.Skip("no ollama daemon")
	}
	t.Logf("model: %s", oll.Model())
	reqs := []LyricsRequest{
		{Style: "upbeat pop, bright, energetic, catchy", Theme: "having a good day", Seconds: 150},
		{Style: "energetic rock, driving drums", Theme: "going to the mall to do some shopping", Seconds: 150},
	}
	for _, req := range reqs {
		for _, gen := range Generators() {
			start := time.Now()
			cctx, cancel := context.WithTimeout(ctx, gen.Timeout())
			out, err := gen.Generate(cctx, oll, req)
			cancel()
			took := time.Since(start).Round(time.Second)
			if err != nil {
				t.Errorf("%s(%q): %v after %s", gen.Name(), req.Theme, err, took)
				continue
			}
			var sung []string
			for _, l := range strings.Split(out, "\n") {
				l = strings.TrimSpace(l)
				if l != "" && !strings.HasPrefix(l, "[") {
					sung = append(sung, l)
				}
			}
			probs := checkSong(sung, req.Seconds)
			fmt.Printf("\n======== %s | %q | %s | %d sung lines | song-notes: %v\n%s\n",
				gen.Name(), req.Theme, took, len(sung), probs, out)
		}
	}
}
