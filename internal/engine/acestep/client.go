// Package acestep integrates the ACE-Step music generation engine: a REST
// client for its API server, a sidecar supervisor that runs the server as a
// child process, and an installer that sets the engine up from scratch.
package acestep

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"iar/internal/audio"
)

// Client talks to a running ACE-Step API server.
type Client struct {
	baseURL string
	http    *http.Client
	// PollInterval is how often task status is polled.
	PollInterval time.Duration
}

// NewClient returns a client for the API server at baseURL
// (e.g. "http://127.0.0.1:8451").
func NewClient(baseURL string) *Client {
	return &Client{
		baseURL:      baseURL,
		http:         &http.Client{Timeout: 60 * time.Second},
		PollInterval: 2 * time.Second,
	}
}

// apiEnvelope is the server's uniform response wrapper.
type apiEnvelope struct {
	Data  json.RawMessage `json:"data"`
	Code  int             `json:"code"`
	Error *string         `json:"error"`
}

// Health describes the server's readiness.
type Health struct {
	Status            string `json:"status"`
	ModelsInitialized bool   `json:"models_initialized"`
	LLMInitialized    bool   `json:"llm_initialized"`
	LoadedModel       string `json:"loaded_model"`
	LoadedLMModel     string `json:"loaded_lm_model"`
}

// Health queries the server's health endpoint.
func (c *Client) Health(ctx context.Context) (*Health, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/health", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	env, err := decodeEnvelope(resp)
	if err != nil {
		return nil, err
	}
	var h Health
	if err := json.Unmarshal(env.Data, &h); err != nil {
		return nil, fmt.Errorf("parsing health: %w", err)
	}
	return &h, nil
}

// GenerateRequest mirrors the fields of the server's release_task API that
// this application uses.
type GenerateRequest struct {
	Prompt      string `json:"prompt,omitempty"`
	Lyrics      string `json:"lyrics,omitempty"`
	SampleMode  bool   `json:"sample_mode,omitempty"`
	SampleQuery string `json:"sample_query,omitempty"`
	// PlanOnly runs the planner phases (metadata, lyrics, audio codes)
	// and returns them without rendering any audio (plan-only patch).
	PlanOnly      bool    `json:"plan_only,omitempty"`
	Thinking      bool    `json:"thinking"`
	AudioDuration float64 `json:"audio_duration,omitempty"`
	// AudioCodeString carries a plan's audio codes into a render job;
	// with Thinking off the planner LM is never consulted.
	AudioCodeString string `json:"audio_code_string,omitempty"`
	AudioFormat     string `json:"audio_format"`
	InferenceSteps  int    `json:"inference_steps,omitempty"`
	BatchSize       int    `json:"batch_size"`
	UseRandomSeed   bool   `json:"use_random_seed"`
	Seed            int64  `json:"seed"`

	// Structured constraints: the server injects these as hard metadata
	// during constrained decoding (user metadata always wins).
	BPM           int    `json:"bpm,omitempty"`
	KeyScale      string `json:"key_scale,omitempty"`
	TimeSignature string `json:"time_signature,omitempty"`
	VocalLanguage string `json:"vocal_language,omitempty"`
	// Planner-LM negative conditioning: the only working negative lever
	// on the CFG-distilled turbo model.
	LMNegativePrompt string  `json:"lm_negative_prompt,omitempty"`
	LMCfgScale       float64 `json:"lm_cfg_scale,omitempty"`
}

// GenerateResult is one finished track as reported by the server, before
// the audio file is fetched.
type GenerateResult struct {
	File   string `json:"file"`
	Status int    `json:"status"`
	Prompt string `json:"prompt"`
	Lyrics string `json:"lyrics"`
	Seed   string `json:"seed_value"`
	// AudioCodes is the planned audio-code string of a plan-only job.
	AudioCodes string `json:"audio_codes"`
	// Metas carries the planner's final metadata; numeric fields may
	// arrive as numbers or as the string "N/A".
	Metas planMetas `json:"metas"`
}

// planMetas is the loosely-typed metadata block of a task result.
type planMetas struct {
	BPM           any    `json:"bpm"`
	Duration      any    `json:"duration"`
	Keyscale      string `json:"keyscale"`
	Timesignature string `json:"timesignature"`
}

// metaFloat coerces a metadata value that may be a number or "N/A".
func metaFloat(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	case string:
		f, err := strconv.ParseFloat(x, 64)
		if err != nil {
			return 0
		}
		return f
	}
	return 0
}

// TaskError reports a generation job the engine marked failed, carrying
// the engine's own failure reason when it provided one.
type TaskError struct {
	// TaskID identifies the failed job.
	TaskID string
	// Reason is the engine's error message ("" when it gave none).
	Reason string
}

// Error implements error.
func (e *TaskError) Error() string {
	if e.Reason != "" {
		return "generation failed: " + e.Reason
	}
	return "generation task " + e.TaskID + " failed"
}

// failureReason digs the engine's own message out of a failed task's
// result payload (a JSON array encoded as a string).
func failureReason(payload string) string {
	if payload == "" {
		return ""
	}
	var rows []struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(payload), &rows); err != nil {
		return ""
	}
	for _, r := range rows {
		if msg := strings.TrimSpace(r.Error); msg != "" {
			return msg
		}
	}
	return ""
}

// taskStatus values used by the ACE-Step job store.
const (
	statusPending   = 0
	statusSucceeded = 1
	statusFailed    = 2
)

// Generate submits a generation task, waits for completion and returns the
// decoded track in the internal PCM format.
func (c *Client) Generate(ctx context.Context, req GenerateRequest) (*Result, error) {
	start := time.Now()
	taskID, err := c.releaseTask(ctx, req)
	if err != nil {
		return nil, err
	}
	res, err := c.waitForTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	wavBytes, err := c.fetchAudio(ctx, res.File)
	if err != nil {
		return nil, err
	}
	samples, err := decodeToInternal(ctx, wavBytes)
	if err != nil {
		return nil, err
	}
	return &Result{
		Samples: samples,
		Prompt:  res.Prompt,
		Lyrics:  res.Lyrics,
		Seed:    res.Seed,
		Elapsed: time.Since(start),
	}, nil
}

// Result is a decoded generation result.
type Result struct {
	Samples []int16
	Prompt  string
	Lyrics  string
	Seed    string
	Elapsed time.Duration
}

func (c *Client) releaseTask(ctx context.Context, req GenerateRequest) (string, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return "", err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/release_task", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("release_task: %w", err)
	}
	defer resp.Body.Close()
	env, err := decodeEnvelope(resp)
	if err != nil {
		return "", fmt.Errorf("release_task: %w", err)
	}
	var data struct {
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil || data.TaskID == "" {
		return "", fmt.Errorf("release_task: no task_id in response")
	}
	return data.TaskID, nil
}

func (c *Client) waitForTask(ctx context.Context, taskID string) (*GenerateResult, error) {
	ticker := time.NewTicker(c.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
		status, result, err := c.queryResult(ctx, taskID)
		if err != nil {
			return nil, err
		}
		switch status {
		case statusPending:
			continue
		case statusSucceeded:
			if len(result) == 0 {
				return nil, fmt.Errorf("task %s succeeded with empty result", taskID)
			}
			return &result[0], nil
		default:
			return nil, &TaskError{TaskID: taskID}
		}
	}
}

func (c *Client) queryResult(ctx context.Context, taskID string) (int, []GenerateResult, error) {
	body, _ := json.Marshal(map[string]any{"task_id_list": []string{taskID}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/query_result", bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("query_result: %w", err)
	}
	defer resp.Body.Close()
	env, err := decodeEnvelope(resp)
	if err != nil {
		return 0, nil, fmt.Errorf("query_result: %w", err)
	}
	var entries []struct {
		TaskID string `json:"task_id"`
		Status int    `json:"status"`
		Result string `json:"result"`
		Error  string `json:"error"`
		// ProgressText carries the engine's last log line, which is
		// where the failure text lands when nothing else has it.
		ProgressText string `json:"progress_text"`
	}
	if err := json.Unmarshal(env.Data, &entries); err != nil {
		return 0, nil, fmt.Errorf("query_result: parsing: %w", err)
	}
	if len(entries) == 0 {
		return 0, nil, fmt.Errorf("query_result: task %s unknown", taskID)
	}
	entry := entries[0]
	if entry.Status == statusFailed {
		reason := entry.Error
		if reason == "" {
			// The server reports a failure's cause inside the encoded
			// result payload, not beside it. Without this the reason is
			// always empty, and every failure looks alike - so the
			// known-fatal device fault is never recognized and the
			// engine is restarted three failures late.
			reason = failureReason(entry.Result)
		}
		if reason == "" {
			reason = strings.TrimSpace(entry.ProgressText)
		}
		return entry.Status, nil, &TaskError{TaskID: taskID, Reason: reason}
	}
	if entry.Status != statusSucceeded || entry.Result == "" {
		return entry.Status, nil, nil
	}
	// The result field is a JSON string containing an array of results.
	var results []GenerateResult
	if err := json.Unmarshal([]byte(entry.Result), &results); err != nil {
		return entry.Status, nil, fmt.Errorf("query_result: parsing result payload: %w", err)
	}
	return entry.Status, results, nil
}

func (c *Client) fetchAudio(ctx context.Context, fileURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+fileURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching audio: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching audio: HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

func decodeEnvelope(resp *http.Response) (*apiEnvelope, error) {
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, bytes.TrimSpace(body))
	}
	var env apiEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, fmt.Errorf("parsing response: %w", err)
	}
	if env.Code != http.StatusOK {
		msg := "unknown error"
		if env.Error != nil {
			msg = *env.Error
		}
		return nil, fmt.Errorf("API error %d: %s", env.Code, msg)
	}
	return &env, nil
}

// decodeToInternal converts WAV bytes to the internal PCM format, shelling
// out to ffmpeg only when the sample rate needs conversion.
func decodeToInternal(ctx context.Context, wavBytes []byte) ([]int16, error) {
	w, err := audio.DecodeWAV(wavBytes)
	if err != nil {
		return nil, err
	}
	if w.Rate == audio.SampleRate {
		return w.ToInternal()
	}
	return resampleWithFFmpeg(ctx, wavBytes)
}

// resampleWithFFmpeg converts arbitrary audio bytes to the internal format.
func resampleWithFFmpeg(ctx context.Context, in []byte) ([]int16, error) {
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-hide_banner", "-loglevel", "error",
		"-i", "-",
		"-f", "s16le", "-ar", fmt.Sprint(audio.SampleRate), "-ac", fmt.Sprint(audio.Channels),
		"-",
	)
	cmd.Stdin = bytes.NewReader(in)
	var out bytes.Buffer
	cmd.Stdout = &out
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg resample: %w: %s", err, bytes.TrimSpace(errBuf.Bytes()))
	}
	return audio.BytesToSamples(out.Bytes()), nil
}

// Plan runs a plan-only job: the planner phases produce metadata,
// lyrics and audio codes, and no audio is rendered or fetched.
func (c *Client) Plan(ctx context.Context, req GenerateRequest) (*PlanResult, error) {
	start := time.Now()
	req.PlanOnly = true
	taskID, err := c.releaseTask(ctx, req)
	if err != nil {
		return nil, err
	}
	res, err := c.waitForTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if res.AudioCodes == "" {
		return nil, fmt.Errorf("plan task %s returned no audio codes", taskID)
	}
	return &PlanResult{
		Caption:       res.Prompt,
		Lyrics:        res.Lyrics,
		AudioCodes:    res.AudioCodes,
		Seconds:       metaFloat(res.Metas.Duration),
		BPM:           int(metaFloat(res.Metas.BPM)),
		KeyScale:      res.Metas.Keyscale,
		TimeSignature: res.Metas.Timesignature,
		Elapsed:       time.Since(start),
	}, nil
}

// PlanResult is a completed planning job.
type PlanResult struct {
	Caption       string
	Lyrics        string
	AudioCodes    string
	Seconds       float64
	BPM           int
	KeyScale      string
	TimeSignature string
	Elapsed       time.Duration
}
