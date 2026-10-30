package reporter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// messagesAPI fakes the Messages API: each request gets the next scripted response.
type messagesAPI struct {
	t         *testing.T
	responses []apiResponse
	requests  []map[string]any
	headers   []http.Header
}

type apiResponse struct {
	status int
	body   string
	header map[string]string
}

func (m *messagesAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" {
		m.t.Errorf("%s %s", r.Method, r.URL.Path)
	}
	b, _ := io.ReadAll(r.Body)
	var req map[string]any
	if err := json.Unmarshal(b, &req); err != nil {
		m.t.Errorf("request body: %v", err)
	}
	m.requests = append(m.requests, req)
	m.headers = append(m.headers, r.Header.Clone())
	resp := m.responses[min(len(m.requests)-1, len(m.responses)-1)]
	for k, v := range resp.header {
		w.Header().Set(k, v)
	}
	w.WriteHeader(resp.status)
	w.Write([]byte(resp.body))
}

func testClaude(t *testing.T, responses ...apiResponse) (*Claude, *messagesAPI, *[]time.Duration) {
	api := &messagesAPI{t: t, responses: responses}
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	var slept []time.Duration
	c := &Claude{Key: "sk-test", BaseURL: srv.URL,
		Sleep: func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }}
	return c, api, &slept
}

const okBody = `{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5",
	"content":[{"type":"thinking","thinking":"","signature":"sig"},{"type":"text","text":"## What happened\n"},{"type":"text","text":"Quiet day."}],
	"stop_reason":"end_turn","usage":{"input_tokens":1200,"output_tokens":80}}`

func TestClaudeSendsOneRequestAndReadsItsText(t *testing.T) {
	c, api, _ := testClaude(t, apiResponse{status: 200, body: okBody})
	reply, err := c.Write(context.Background(), "the system prompt", "the facts")
	if err != nil {
		t.Fatal(err)
	}
	if reply.Text != "## What happened\nQuiet day." {
		t.Errorf("text %q: every text block, in order, and no thinking", reply.Text)
	}
	if reply.Model != "claude-opus-5" || reply.Truncated || reply.FellBack || reply.InputTokens != 1200 || reply.OutputTokens != 80 {
		t.Errorf("reply %+v", reply)
	}
	h := api.headers[0]
	for k, want := range map[string]string{
		"X-Api-Key": "sk-test", "Anthropic-Version": "2023-06-01", "Content-Type": "application/json",
		"Anthropic-Beta": "server-side-fallback-2026-07-01",
	} {
		if h.Get(k) != want {
			t.Errorf("header %s = %q, want %q", k, h.Get(k), want)
		}
	}
	req := api.requests[0]
	if req["model"] != "claude-opus-5" || req["max_tokens"].(float64) != 16000 || req["system"] != "the system prompt" {
		t.Errorf("request %v", req)
	}
	if th, _ := req["thinking"].(map[string]any); th["type"] != "adaptive" {
		t.Errorf("thinking %v: adaptive", req["thinking"])
	}
	if req["fallbacks"] != "default" {
		t.Errorf("fallbacks %v: a declined request reruns on Anthropic's recommended model", req["fallbacks"])
	}
	msgs := req["messages"].([]any)
	if len(msgs) != 1 || msgs[0].(map[string]any)["role"] != "user" || msgs[0].(map[string]any)["content"] != "the facts" {
		t.Errorf("messages %v", msgs)
	}
	if _, ok := req["temperature"]; ok {
		t.Errorf("temperature is set: Opus 5 rejects non-default sampling")
	}
}

func TestClaudeUsesTheConfiguredModel(t *testing.T) {
	c, api, _ := testClaude(t, apiResponse{status: 200, body: okBody})
	c.Model = "claude-sonnet-5"
	if _, err := c.Write(context.Background(), "s", "u"); err != nil {
		t.Fatal(err)
	}
	if api.requests[0]["model"] != "claude-sonnet-5" {
		t.Errorf("model %v", api.requests[0]["model"])
	}
}

func TestClaudeRefusal(t *testing.T) {
	c, _, _ := testClaude(t, apiResponse{status: 200, body: `{"type":"message","model":"claude-opus-4-8","content":[],
		"stop_reason":"refusal","stop_details":{"category":"cyber"},"usage":{"input_tokens":0,"output_tokens":0}}`})
	_, err := c.Write(context.Background(), "s", "u")
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "cyber") {
		t.Errorf("err %v: a refusal is not a report", err)
	}
}

func TestClaudeRefusalWithoutDetails(t *testing.T) {
	c, _, _ := testClaude(t, apiResponse{status: 200, body: `{"type":"message","content":[{"type":"text","text":"partial"}],"stop_reason":"refusal","stop_details":null}`})
	if _, err := c.Write(context.Background(), "s", "u"); !errors.Is(err, ErrRefused) {
		t.Errorf("err %v: branch on stop_reason, not stop_details", err)
	}
}

func TestClaudeNotesAFallback(t *testing.T) {
	c, _, _ := testClaude(t, apiResponse{status: 200, body: `{"type":"message","model":"claude-opus-4-8",
		"content":[{"type":"fallback","from":{"model":"claude-opus-5"},"to":{"model":"claude-opus-4-8"}},{"type":"text","text":"Report."}],
		"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5,"iterations":[{"type":"message"},{"type":"fallback_message"}]}}`})
	reply, err := c.Write(context.Background(), "s", "u")
	if err != nil {
		t.Fatal(err)
	}
	if !reply.FellBack || reply.Model != "claude-opus-4-8" || reply.Text != "Report." {
		t.Errorf("reply %+v: the fallback block isn't text, and the report says which model wrote it", reply)
	}
}

func TestClaudeTruncated(t *testing.T) {
	c, _, _ := testClaude(t, apiResponse{status: 200, body: `{"type":"message","model":"claude-opus-5","content":[{"type":"text","text":"## What hap"}],"stop_reason":"max_tokens"}`})
	reply, err := c.Write(context.Background(), "s", "u")
	if err != nil || !reply.Truncated || reply.Text != "## What hap" {
		t.Errorf("reply %+v %v: keep what was written, and say it was cut short", reply, err)
	}
}

func TestClaudeRetriesOverloadAndRateLimits(t *testing.T) {
	c, api, slept := testClaude(t,
		apiResponse{status: 529, body: `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`},
		apiResponse{status: 429, body: `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`, header: map[string]string{"retry-after": "7"}},
		apiResponse{status: 500, body: `{"type":"error","error":{"type":"api_error","message":"oops"}}`},
		apiResponse{status: 200, body: okBody},
	)
	if _, err := c.Write(context.Background(), "s", "u"); err != nil {
		t.Fatal(err)
	}
	if len(api.requests) != 4 {
		t.Errorf("%d requests", len(api.requests))
	}
	if len(*slept) != 3 || (*slept)[1] != 7*time.Second {
		t.Errorf("slept %v: honor retry-after", *slept)
	}
	if (*slept)[2] <= (*slept)[0] {
		t.Errorf("slept %v: back off", *slept)
	}
}

func TestClaudeGivesUpAfterItsRetries(t *testing.T) {
	c, api, _ := testClaude(t, apiResponse{status: 529, body: `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`})
	_, err := c.Write(context.Background(), "s", "u")
	if err == nil || !strings.Contains(err.Error(), "overloaded_error") {
		t.Errorf("err %v", err)
	}
	if len(api.requests) != 4 {
		t.Errorf("%d requests: 1 and 3 retries", len(api.requests))
	}
}

func TestClaudeDoesNotRetryABadRequest(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 413} {
		c, api, _ := testClaude(t, apiResponse{status: status, body: `{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens: too large"}}`})
		_, err := c.Write(context.Background(), "s", "u")
		if err == nil || !strings.Contains(err.Error(), "max_tokens: too large") || !strings.Contains(err.Error(), "invalid_request_error") {
			t.Errorf("%d: err %v", status, err)
		}
		if len(api.requests) != 1 {
			t.Errorf("%d: %d requests: retrying the same body fails the same way", status, len(api.requests))
		}
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.Status != status {
			t.Errorf("%d: err %T", status, err)
		}
	}
}

func TestClaudeNeedsAKey(t *testing.T) {
	c, api, _ := testClaude(t, apiResponse{status: 200, body: okBody})
	c.Key = ""
	if _, err := c.Write(context.Background(), "s", "u"); err == nil {
		t.Errorf("no error without a key")
	}
	if len(api.requests) != 0 {
		t.Errorf("sent a request without a key")
	}
}
