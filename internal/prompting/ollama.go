package prompting

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Ollama is a minimal client for a local Ollama daemon, used only to polish
// prompts and write short lyrics. The application works fully without it.
type Ollama struct {
	url   string
	model string
	http  *http.Client
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

// Available checks the daemon is reachable and resolves the model name.
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
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tags); err != nil {
		return false
	}
	if o.model == "" {
		if len(tags.Models) == 0 {
			return false
		}
		o.model = tags.Models[0].Name
	}
	return true
}

// Model reports the resolved model name.
func (o *Ollama) Model() string { return o.model }

// ChatJSON sends one exchange with a JSON schema constraint on the
// reply (Ollama's format parameter), returning the raw JSON text.
func (o *Ollama) ChatJSON(ctx context.Context, system, user string, schema any) (string, error) {
	return o.chat(ctx, system, user, schema)
}

// Chat sends one system+user exchange and returns the reply text.
func (o *Ollama) Chat(ctx context.Context, system, user string) (string, error) {
	return o.chat(ctx, system, user, nil)
}

// chat implements both calls. keep_alive is zero so the helper model
// frees GPU memory for the music engine right after each call.
func (o *Ollama) chat(ctx context.Context, system, user string, format any) (string, error) {
	if o.model == "" && !o.Available(ctx) {
		return "", fmt.Errorf("ollama unavailable")
	}
	stream := false
	payload := map[string]any{
		"model": o.model,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
		"stream":     &stream,
		"keep_alive": 0,
		"options":    map[string]any{"temperature": 0.7},
	}
	if format != nil {
		payload["format"] = format
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
