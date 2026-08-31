package prompting

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// unsuitableModelRe matches installed models that cannot write prose
// (code, embedding, vision and safety models).
var unsuitableModelRe = regexp.MustCompile(`(?i)code|embed|rerank|guardian|vision|-vl\b`)

// Ollama is a minimal client for a local Ollama daemon, used only to polish
// prompts and write short lyrics. The application works fully without it.
type Ollama struct {
	url   string
	model string
	http  *http.Client
	// thinkOK records whether the resolved model advertises the
	// thinking capability (set by Available).
	thinkOK bool
}

// NewOllama returns a client for the daemon at url. model may be empty, in
// which case the first installed model is used.
func NewOllama(url, model string) *Ollama {
	return &Ollama{
		url:   strings.TrimRight(url, "/"),
		model: model,
		http:  &http.Client{Timeout: 90 * time.Second},
	}
}

// Available checks the daemon is reachable, resolves the model name and
// records whether the model supports thinking.
func (o *Ollama) Available(ctx context.Context) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.url+"/api/tags", nil)
	if err != nil {
		return false
	}
	resp, err := o.http.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var tags struct {
		Models []struct {
			Name         string   `json:"name"`
			Capabilities []string `json:"capabilities"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tags); err != nil {
		return false
	}
	if o.model == "" {
		if len(tags.Models) == 0 {
			return false
		}
		// Prefer a general model: coder/embedding/vision models write
		// terrible prose. Only when every installed model looks
		// special-purpose does the first one win.
		o.model = tags.Models[0].Name
		for _, m := range tags.Models {
			if !unsuitableModelRe.MatchString(m.Name) {
				o.model = m.Name
				break
			}
		}
	}
	for _, m := range tags.Models {
		if m.Name == o.model {
			for _, c := range m.Capabilities {
				if c == "thinking" {
					o.thinkOK = true
				}
			}
		}
	}
	if !o.thinkOK {
		// Older daemons omit capabilities from /api/tags; /api/show
		// has carried them longer.
		o.thinkOK = o.showSupportsThinking(ctx)
	}
	return true
}

// showSupportsThinking asks /api/show whether the resolved model
// advertises the thinking capability (best-effort).
func (o *Ollama) showSupportsThinking(ctx context.Context) bool {
	body, err := json.Marshal(map[string]string{"model": o.model})
	if err != nil {
		return false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.url+"/api/show", bytes.NewReader(body))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := o.http.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var show struct {
		Capabilities []string `json:"capabilities"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&show); err != nil {
		return false
	}
	for _, c := range show.Capabilities {
		if c == "thinking" {
			return true
		}
	}
	return false
}

// Model reports the resolved model name.
func (o *Ollama) Model() string { return o.model }

// ChatOpts tunes one chat call. Zero values keep the defaults the
// simple Chat call has always used.
type ChatOpts struct {
	// Format is a JSON schema constraint on the reply.
	Format any
	// Temperature overrides the default 0.7 when non-zero.
	Temperature float64
	// TopP, MinP and RepeatPenalty are passed through when non-zero.
	TopP          float64
	MinP          float64
	RepeatPenalty float64
	// NumPredict caps the reply length; NumCtx sets the context window.
	NumPredict int
	NumCtx     int
	// Think toggles the model's thinking phase; nil leaves the model's
	// default. Ignored for models without the capability.
	Think *bool
	// KeepAliveSeconds keeps the model loaded after the call, so a
	// pipeline of calls avoids a reload each time. Zero unloads
	// immediately (the historical behaviour, kind to the music
	// engine's VRAM).
	KeepAliveSeconds int
}

// ChatJSON sends one exchange with a JSON schema constraint on the
// reply (Ollama's format parameter), returning the raw JSON text.
func (o *Ollama) ChatJSON(ctx context.Context, system, user string, schema any) (string, error) {
	return o.ChatWith(ctx, system, user, ChatOpts{Format: schema})
}

// Chat sends one system+user exchange and returns the reply text.
func (o *Ollama) Chat(ctx context.Context, system, user string) (string, error) {
	return o.ChatWith(ctx, system, user, ChatOpts{})
}

// ChatWith sends one exchange with per-call options.
func (o *Ollama) ChatWith(ctx context.Context, system, user string, opts ChatOpts) (string, error) {
	if o.model == "" && !o.Available(ctx) {
		return "", fmt.Errorf("ollama unavailable")
	}
	stream := false
	options := map[string]any{"temperature": 0.7}
	if opts.Temperature != 0 {
		options["temperature"] = opts.Temperature
	}
	if opts.TopP != 0 {
		options["top_p"] = opts.TopP
	}
	if opts.MinP != 0 {
		options["min_p"] = opts.MinP
	}
	if opts.RepeatPenalty != 0 {
		options["repeat_penalty"] = opts.RepeatPenalty
	}
	if opts.NumPredict != 0 {
		options["num_predict"] = opts.NumPredict
	}
	if opts.NumCtx != 0 {
		options["num_ctx"] = opts.NumCtx
	}
	payload := map[string]any{
		"model": o.model,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
		"stream":     &stream,
		"keep_alive": opts.KeepAliveSeconds,
		"options":    options,
	}
	if opts.Think != nil && o.thinkOK {
		payload["think"] = *opts.Think
	}
	if opts.Format != nil {
		payload["format"] = opts.Format
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.url+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := o.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ollama: HTTP %d", resp.StatusCode)
	}
	var out struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	reply := strings.TrimSpace(out.Message.Content)
	if reply == "" {
		return "", fmt.Errorf("ollama: empty reply")
	}
	return reply, nil
}

// Unload asks the daemon to free the model's memory immediately
// (best-effort): an empty generate request with keep_alive 0. Used
// after a multi-call pipeline that kept the model warm between calls.
func (o *Ollama) Unload(ctx context.Context) {
	if o.model == "" {
		return
	}
	body, err := json.Marshal(map[string]any{"model": o.model, "keep_alive": 0})
	if err != nil {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.url+"/api/generate", bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if resp, err := o.http.Do(req); err == nil {
		resp.Body.Close()
	}
}
