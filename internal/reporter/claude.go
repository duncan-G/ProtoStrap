package reporter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// DefaultModel writes the report unless the scheduler's config names another.
const DefaultModel = "claude-opus-5"

// ErrRefused reports a request the model declined, on its own or through Anthropic's safety
// classifiers, after any fallback model declined too.
var ErrRefused = errors.New("the model declined to write the report")

// Claude calls the Messages API with net/http: ProtoStrap uses only the standard library.
type Claude struct {
	Key string
	// Model defaults to DefaultModel.
	Model string
	// BaseURL defaults to https://api.anthropic.com.
	BaseURL string
	// MaxTokens caps thinking and the report together. It defaults to 16000.
	MaxTokens int
	// Retries is how many more tries a request gets after the first, on rate limits,
	// timeouts, overloads, server errors and network errors. It defaults to 3.
	Retries int
	// HTTP defaults to a client with a 10-minute timeout.
	HTTP  *http.Client
	Sleep func(ctx context.Context, d time.Duration) error
}

// Reply is what the model wrote.
type Reply struct {
	Text string
	// Model is the model that wrote the reply, which is the fallback model if FellBack is set.
	Model string
	// FellBack means the requested model declined and Anthropic's fallback wrote the reply.
	FellBack bool
	// Truncated is true when the reply hit MaxTokens.
	Truncated    bool
	InputTokens  int
	OutputTokens int
}

// APIError is an error response from the Messages API.
type APIError struct {
	Status int
	// Type is the error's type, such as invalid_request_error, rate_limit_error or
	// overloaded_error.
	Type       string
	Message    string
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	return fmt.Sprintf("messages API: HTTP %d %s: %s", e.Status, e.Type, e.Message)
}

// retryable reports whether the error is a rate limit, a timeout, an overload or a server
// error. Anything else in 4xx fails the same way every time.
func (e *APIError) retryable() bool {
	return e.Status == http.StatusTooManyRequests || e.Status == http.StatusRequestTimeout || e.Status >= 500
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type request struct {
	Model     string         `json:"model"`
	MaxTokens int            `json:"max_tokens"`
	System    string         `json:"system"`
	Messages  []message      `json:"messages"`
	Thinking  map[string]any `json:"thinking"`
	// A request the model's safety classifiers decline reruns on the model Anthropic
	// recommends for that kind of refusal, server side.
	Fallbacks string `json:"fallbacks"`
}

const fallbackBeta = "server-side-fallback-2026-07-01"

type response struct {
	Model   string `json:"model"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StopReason  string `json:"stop_reason"`
	StopDetails *struct {
		Category    string `json:"category"`
		Explanation string `json:"explanation"`
	} `json:"stop_details"`
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
		Iterations   []struct {
			Type string `json:"type"`
		} `json:"iterations"`
	} `json:"usage"`
}

// Write sends one message, with system as the system prompt, and returns the text of the
// reply.
func (c *Claude) Write(ctx context.Context, system, user string) (Reply, error) {
	if c.Key == "" {
		return Reply{}, errors.New("no API key")
	}
	model := c.Model
	if model == "" {
		model = DefaultModel
	}
	maxTokens := c.MaxTokens
	if maxTokens == 0 {
		maxTokens = 16000
	}
	body, err := json.Marshal(request{
		Model: model, MaxTokens: maxTokens, System: system,
		Messages:  []message{{Role: "user", Content: user}},
		Thinking:  map[string]any{"type": "adaptive"},
		Fallbacks: "default",
	})
	if err != nil {
		return Reply{}, err
	}
	retries := c.Retries
	if retries == 0 {
		retries = 3
	}
	backoff := 2 * time.Second
	for attempt := 0; ; attempt++ {
		resp, err := c.send(ctx, body)
		if err == nil {
			return reply(resp)
		}
		var apiErr *APIError
		transient := !errors.As(err, &apiErr) || apiErr.retryable()
		if ctx.Err() != nil || !transient || attempt >= retries {
			return Reply{}, err
		}
		wait := backoff
		if apiErr != nil && apiErr.RetryAfter > 0 {
			wait = min(apiErr.RetryAfter, time.Minute)
		}
		backoff *= 2
		if err := c.sleep(ctx, wait); err != nil {
			return Reply{}, err
		}
	}
}

func (c *Claude) send(ctx context.Context, body []byte) (*response, error) {
	base := c.BaseURL
	if base == "" {
		base = "https://api.anthropic.com"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(base, "/")+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", c.Key)
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Set("Anthropic-Beta", fallbackBeta)
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Minute}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("messages API: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("messages API: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		e := &APIError{Status: resp.StatusCode, Message: strings.TrimSpace(string(data))}
		var body struct {
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(data, &body) == nil && body.Error.Type != "" {
			e.Type, e.Message = body.Error.Type, body.Error.Message
		}
		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
			e.RetryAfter = time.Duration(s) * time.Second
		}
		return nil, e
	}
	var r response
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("messages API: the response: %w", err)
	}
	return &r, nil
}

// reply reads a response. A refusal is checked first: its content is empty or partial.
func reply(r *response) (Reply, error) {
	if r.StopReason == "refusal" {
		if r.StopDetails != nil && r.StopDetails.Category != "" {
			return Reply{}, fmt.Errorf("%w (category %s)", ErrRefused, r.StopDetails.Category)
		}
		return Reply{}, ErrRefused
	}
	var text strings.Builder
	for _, b := range r.Content {
		if b.Type == "text" {
			text.WriteString(b.Text)
		}
	}
	out := Reply{Text: text.String(), Model: r.Model, Truncated: r.StopReason == "max_tokens",
		InputTokens: r.Usage.InputTokens, OutputTokens: r.Usage.OutputTokens}
	for _, it := range r.Usage.Iterations {
		if it.Type == "fallback_message" {
			out.FellBack = true
		}
	}
	return out, nil
}

func (c *Claude) sleep(ctx context.Context, d time.Duration) error {
	if c.Sleep != nil {
		return c.Sleep(ctx, d)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}
