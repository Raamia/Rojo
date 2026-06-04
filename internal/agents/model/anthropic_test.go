package model

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

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// The production client must satisfy the same interface the agents depend on,
// so nothing outside this package has to know which provider is in use.
var _ Client = (*AnthropicClient)(nil)

func TestAnthropicClient_DefaultsToOpus48(t *testing.T) {
	c := NewAnthropicClient(AnthropicOptions{})
	if c.model != anthropic.ModelClaudeOpus4_8 {
		t.Errorf("default model = %q, want %q", c.model, anthropic.ModelClaudeOpus4_8)
	}
	if got := NewAnthropicClient(AnthropicOptions{Model: "claude-haiku-4-5"}).model; got != "claude-haiku-4-5" {
		t.Errorf("configured model = %q, want it honoured", got)
	}
}

// newStubAPI stands in for the Messages endpoint so the wire handling can be
// exercised without a network call or an API key.
func newStubAPI(t *testing.T, handler http.HandlerFunc) *AnthropicClient {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	c := NewAnthropicClient(AnthropicOptions{APIKey: "test-key"})
	c.api = anthropic.NewClient(
		anthropicTestOptions(srv.URL)...,
	)
	return c
}

func TestAnthropicClient_ExtractsTextAndModel(t *testing.T) {
	c := newStubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"id": "msg_1", "type": "message", "role": "assistant",
			"model": "claude-opus-4-8", "stop_reason": "end_turn",
			"content": [
				{"type": "thinking", "thinking": "", "signature": ""},
				{"type": "text", "text": "{\"summary\":\"a plan\"}"}
			],
			"usage": {"input_tokens": 10, "output_tokens": 5}
		}`)
	})

	got, err := c.Generate(context.Background(), Request{Prompt: "plan this"})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	// Thinking blocks precede text and must not leak into the parsed answer.
	if got.Text != `{"summary":"a plan"}` {
		t.Errorf("text = %q, want only the text block", got.Text)
	}
	if got.Model != "claude-opus-4-8" {
		t.Errorf("model = %q, want the model that answered", got.Model)
	}
}

// A thinking-only or tool-only reply carries nothing for the caller to parse,
// so it is a failure rather than an empty success.
func TestAnthropicClient_NoTextContentIsAnError(t *testing.T) {
	c := newStubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"id": "msg_1", "type": "message", "role": "assistant",
			"model": "claude-opus-4-8", "stop_reason": "max_tokens",
			"content": [{"type": "thinking", "thinking": "", "signature": ""}],
			"usage": {"input_tokens": 10, "output_tokens": 0}
		}`)
	})

	_, err := c.Generate(context.Background(), Request{Prompt: "plan this"})
	if !errors.Is(err, ErrNoTextContent) {
		t.Fatalf("got %v, want ErrNoTextContent", err)
	}
	// The stop reason explains why it was empty — keep it in the message.
	if !strings.Contains(err.Error(), "max_tokens") {
		t.Errorf("error %q should name the stop reason", err)
	}
}

// The request must carry the system prompt, the user prompt, adaptive thinking
// and a bounded max_tokens — and must not carry the sampling parameters that
// Opus 4.8 rejects outright.
func TestAnthropicClient_RequestShape(t *testing.T) {
	var body map[string]any
	c := newStubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"m","type":"message","role":"assistant",
			"model":"claude-opus-4-8","stop_reason":"end_turn",
			"content":[{"type":"text","text":"ok"}],
			"usage":{"input_tokens":1,"output_tokens":1}}`)
	})

	if _, err := c.Generate(context.Background(), Request{
		System: "you are a planner", Prompt: "plan this", MaxToks: 4096,
	}); err != nil {
		t.Fatalf("generate: %v", err)
	}

	if body["model"] != "claude-opus-4-8" {
		t.Errorf("model = %v", body["model"])
	}
	if body["max_tokens"] != float64(4096) {
		t.Errorf("max_tokens = %v, want the requested 4096", body["max_tokens"])
	}
	thinking, _ := body["thinking"].(map[string]any)
	if thinking["type"] != "adaptive" {
		t.Errorf("thinking = %v, want adaptive", body["thinking"])
	}
	// budget_tokens and the sampling params are rejected by Opus 4.8; sending
	// any of them would 400 every request.
	for _, banned := range []string{"temperature", "top_p", "top_k"} {
		if _, present := body[banned]; present {
			t.Errorf("request carries %q, which Opus 4.8 rejects", banned)
		}
	}
	if _, present := thinking["budget_tokens"]; present {
		t.Error("thinking carries budget_tokens, which Opus 4.8 rejects")
	}
}

func TestAnthropicClient_DefaultMaxTokensApplied(t *testing.T) {
	var body map[string]any
	c := newStubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"m","type":"message","role":"assistant",
			"model":"claude-opus-4-8","stop_reason":"end_turn",
			"content":[{"type":"text","text":"ok"}],
			"usage":{"input_tokens":1,"output_tokens":1}}`)
	})

	if _, err := c.Generate(context.Background(), Request{Prompt: "x"}); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if body["max_tokens"] != float64(DefaultMaxTokens) {
		t.Errorf("max_tokens = %v, want the %d default", body["max_tokens"], DefaultMaxTokens)
	}
}

// A job's failure message carries whatever this returns, so the error has to
// say what went wrong and whether retrying could help.
func TestAnthropicClient_ErrorsAreDiagnosable(t *testing.T) {
	tests := []struct {
		name   string
		status int
		want   string
	}{
		{"unauthorized names the env var", 401, "ANTHROPIC_API_KEY"},
		{"not found points at the model id", 404, "model not found"},
		{"payload too large suggests the fix", 413, "too large"},
		{"rate limited notes retries were exhausted", 429, "rate limited after retries"},
		{"server error notes retries were exhausted", 500, "server error"},
		{"other 4xx still identifies the status", 400, "rejected (400)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newStubAPI(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, `{"type":"error","error":{"type":"x","message":"boom"}}`)
			})
			_, err := c.Generate(context.Background(), Request{Prompt: "x"})
			if err == nil {
				t.Fatalf("status %d produced no error", tt.status)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q should contain %q", err, tt.want)
			}
		})
	}
}

// A cancelled context must surface as a cancellation, not as an opaque API
// failure — the orchestrator distinguishes the two.
func TestAnthropicClient_ContextCancellation(t *testing.T) {
	c := newStubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := c.Generate(ctx, Request{Prompt: "x"})
	if err == nil {
		t.Fatal("expected an error when the context expires")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error %v should wrap the context cause", err)
	}
}

// anthropicTestOptions points the SDK at a stub server. Retries are disabled so
// error-mapping tests don't pay backoff for each status.
func anthropicTestOptions(baseURL string) []option.RequestOption {
	return []option.RequestOption{
		option.WithAPIKey("test-key"),
		option.WithBaseURL(baseURL),
		option.WithMaxRetries(0),
	}
}
