package advisor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model          string          `json:"model"`
	Messages       []chatMessage   `json:"messages"`
	Temperature    float64         `json:"temperature"`
	MaxTokens      int             `json:"max_tokens"`
	Stream         bool            `json:"stream"`
	ResponseFormat json.RawMessage `json:"response_format,omitempty"`
}

type usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage usage `json:"usage"`
}

type chatResult struct {
	Content string
	Usage   usage
}

// schemaSpec describes the expected JSON object for response_format.
type schemaSpec struct {
	Name   string
	Schema map[string]any
}

const (
	fmtSchema = "json_schema"
	fmtObject = "json_object"
	fmtNone   = "none"
)

// client is a minimal OpenAI-compatible chat client (stdlib only).
type client struct {
	cfg     Config
	hc      *http.Client
	log     *slog.Logger
	retries *atomic.Uint64
	mode    atomic.Value // string: current response_format mode (may be downgraded)
	sleep   func(ctx context.Context, d time.Duration) error
}

func newClient(cfg Config, hc *http.Client, log *slog.Logger, retries *atomic.Uint64) *client {
	if hc == nil {
		hc = &http.Client{}
	}
	c := &client{cfg: cfg, hc: hc, log: log, retries: retries, sleep: sleepCtx}
	c.mode.Store(cfg.ResponseFormat)
	return c
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// scrub removes the API key (and URL credentials) from any text.
func (c *client) scrub(s string) string {
	if c.cfg.APIKey != "" {
		s = strings.ReplaceAll(s, c.cfg.APIKey, "***")
	}
	for _, v := range c.cfg.Headers {
		if len(v) >= 6 {
			s = strings.ReplaceAll(s, v, "***")
		}
	}
	return s
}

type scrubbedError struct {
	msg string
	err error
}

func (e *scrubbedError) Error() string { return e.msg }
func (e *scrubbedError) Unwrap() error { return e.err }

func (c *client) scrubErr(err error) error {
	if err == nil {
		return nil
	}
	m := c.scrub(err.Error())
	if m == err.Error() {
		return err
	}
	return &scrubbedError{msg: m, err: err}
}

func (c *client) responseFormat(mode string, s *schemaSpec) json.RawMessage {
	switch mode {
	case fmtSchema:
		b, _ := json.Marshal(map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name": s.Name, "strict": true, "schema": s.Schema,
			},
		})
		return b
	case fmtObject:
		return json.RawMessage(`{"type":"json_object"}`)
	}
	return nil
}

func nextMode(m string) string {
	if m == fmtSchema {
		return fmtObject
	}
	return fmtNone
}

var formatErrHints = []string{"response_format", "json_schema", "json_object", "schema", "structured",
	"unsupported", "not supported", "unrecognized", "unknown parameter", "extra inputs", "invalid parameter"}

func looksLikeFormatErr(body string) bool {
	b := strings.ToLower(body)
	for _, h := range formatErrHints {
		if strings.Contains(b, h) {
			return true
		}
	}
	return false
}

// chat sends one system+user exchange. When the provider rejects
// response_format (400/422) the mode is downgraded json_schema -> json_object
// -> none and remembered for later calls.
func (c *client) chat(ctx context.Context, system, user string, s *schemaSpec) (*chatResult, error) {
	for {
		mode := c.mode.Load().(string)
		res, err := c.withRetry(ctx, system, user, s, mode)
		var he *HTTPError
		if errors.As(err, &he) && mode != fmtNone && (he.Status == 400 || he.Status == 422) && looksLikeFormatErr(he.Body) {
			next := nextMode(mode)
			c.log.Warn("advisor: provider rejected response_format, downgrading", "from", mode, "to", next)
			c.mode.Store(next)
			continue
		}
		return res, c.scrubErr(err)
	}
}

func (c *client) withRetry(ctx context.Context, system, user string, s *schemaSpec, mode string) (*chatResult, error) {
	body, err := json.Marshal(chatRequest{
		Model:          c.cfg.Model,
		Messages:       []chatMessage{{Role: "system", Content: system}, {Role: "user", Content: user}},
		Temperature:    0,
		MaxTokens:      c.cfg.MaxTokens,
		ResponseFormat: c.responseFormat(mode, s),
	})
	if err != nil {
		return nil, err
	}
	var lastErr error
	for attempt := 0; attempt <= c.cfg.MaxRetries; attempt++ {
		if attempt > 0 {
			c.retries.Add(1)
			wait := c.cfg.RetryBackoff << (attempt - 1)
			wait += time.Duration(rand.Int64N(int64(wait)/4 + 1))
			var he *HTTPError
			if errors.As(lastErr, &he) && he.retryAfter > 0 {
				wait = he.retryAfter
			}
			if wait > 30*time.Second {
				wait = 30 * time.Second
			}
			if err := c.sleep(ctx, wait); err != nil {
				return nil, err
			}
		}
		res, err := c.once(ctx, body)
		if err == nil {
			return res, nil
		}
		lastErr = err
		var he *HTTPError
		if !(errors.As(err, &he) && (he.Status == 429 || he.Status >= 500)) {
			return nil, err // only 429/5xx are retried
		}
	}
	return nil, lastErr
}

func (c *client) once(ctx context.Context, body []byte) (*chatResult, error) {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	for k, v := range c.cfg.Headers {
		req.Header.Set(k, v)
	}
	if c.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	}
	start := time.Now()
	resp, err := c.hc.Do(req)
	if err != nil {
		c.log.Warn("advisor: request failed", "model", c.cfg.Model, "err", c.scrub(err.Error()))
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, c.cfg.MaxResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > c.cfg.MaxResponseBytes {
		return nil, ErrResponseTooLarge
	}
	c.log.Debug("advisor: response", "model", c.cfg.Model, "status", resp.StatusCode, "ms", time.Since(start).Milliseconds())
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		snippet := c.scrub(string(raw))
		if len(snippet) > 512 {
			snippet = snippet[:512] + "..."
		}
		he := &HTTPError{Status: resp.StatusCode, Body: snippet}
		if ra, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && ra > 0 {
			he.retryAfter = time.Duration(ra) * time.Second
		}
		return nil, he
	}
	var cr chatResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		return nil, &ValidationError{Kind: ErrMalformedJSON, Detail: "provider envelope: " + err.Error()}
	}
	if len(cr.Choices) == 0 || strings.TrimSpace(cr.Choices[0].Message.Content) == "" {
		return nil, ErrNoContent
	}
	return &chatResult{Content: cr.Choices[0].Message.Content, Usage: cr.Usage}, nil
}
