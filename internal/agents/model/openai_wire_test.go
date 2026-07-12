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

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
)

// These point the real OpenAI SDK at a local server, so they exercise the exact
// request the production client would send without needing a key. The wire
// format is the part that cannot be checked by reading the code, because the
// SDK's serialisation is what decides it — and OpenAI is unforgiving about a
// couple of fields in ways that would fail every request in production while
// every unit test stayed green.

type openAICapture struct {
	*httptest.Server
	body   map[string]any
	raw    string
	status int
	reply  string
}

func newOpenAICapture(t *testing.T) *openAICapture {
	t.Helper()
	cs := &openAICapture{status: http.StatusOK}
	cs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		cs.raw = string(b)
		_ = json.Unmarshal(b, &cs.body)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(cs.status)
		reply := cs.reply
		if reply == "" {
			reply = openAIReply(`{"summary":"ok"}`, "stop")
		}
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(cs.Close)
	return cs
}

// openAIReply builds a minimal chat-completion envelope.
func openAIReply(content, finish string) string {
	b, _ := json.Marshal(map[string]any{
		"id": "chatcmpl-1", "object": "chat.completion", "created": 1,
		"model": "gpt-5.2",
		"choices": []map[string]any{{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": content},
			"finish_reason": finish,
		}},
		"usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
	})
	return string(b)
}

func openAIClientAgainst(cs *openAICapture, extra ...option.RequestOption) *OpenAIClient {
	opts := append([]option.RequestOption{
		option.WithAPIKey("test-key"),
		option.WithBaseURL(cs.URL),
		option.WithRequestTimeout(30 * time.Second),
	}, extra...)
	return &OpenAIClient{api: openai.NewClient(opts...), model: DefaultOpenAIModel}
}

// Reasoning models reject the deprecated max_tokens with a 400. Only
// max_completion_tokens works across the whole model range, and which one the
// SDK actually serialises is not visible from the Go struct.
func TestOpenAIWire_UsesMaxCompletionTokens(t *testing.T) {
	cs := newOpenAICapture(t)
	if _, err := openAIClientAgainst(cs).Generate(context.Background(), Request{Prompt: "hi"}); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if _, present := cs.body["max_tokens"]; present {
		t.Errorf("request carries the deprecated max_tokens: %s", cs.raw)
	}
	if cs.body["max_completion_tokens"] != float64(DefaultMaxTokens) {
		t.Errorf("max_completion_tokens = %v, want %d", cs.body["max_completion_tokens"], DefaultMaxTokens)
	}
}

// Newer models 400 on temperature/top_p, so sending any of them would fail
// every request — exactly the class of mistake worth a test rather than a
// production incident.
func TestOpenAIWire_OmitsRejectedSamplingParams(t *testing.T) {
	cs := newOpenAICapture(t)
	if _, err := openAIClientAgainst(cs).Generate(context.Background(), Request{Prompt: "hi"}); err != nil {
		t.Fatalf("generate: %v", err)
	}
	for _, forbidden := range []string{"temperature", "top_p", "top_k", "frequency_penalty", "presence_penalty"} {
		if _, present := cs.body[forbidden]; present {
			t.Errorf("request carries %q, which newer models reject: %s", forbidden, cs.raw)
		}
	}
}

// Every agent parses the reply as JSON, so the API is asked to guarantee it.
func TestOpenAIWire_RequestsJSONMode(t *testing.T) {
	cs := newOpenAICapture(t)
	if _, err := openAIClientAgainst(cs).Generate(context.Background(), Request{Prompt: "hi"}); err != nil {
		t.Fatalf("generate: %v", err)
	}
	rf, ok := cs.body["response_format"].(map[string]any)
	if !ok || rf["type"] != "json_object" {
		t.Errorf("response_format = %v, want json_object: %s", cs.body["response_format"], cs.raw)
	}
}

func TestOpenAIWire_CarriesModelSystemAndPrompt(t *testing.T) {
	cs := newOpenAICapture(t)
	_, err := openAIClientAgainst(cs).Generate(context.Background(), Request{
		System: "you are the planner", Prompt: "add a greeting", MaxToks: 4096,
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if cs.body["model"] != string(DefaultOpenAIModel) {
		t.Errorf("model = %v, want %s", cs.body["model"], DefaultOpenAIModel)
	}
	if cs.body["max_completion_tokens"] != float64(4096) {
		t.Errorf("max_completion_tokens = %v, want 4096", cs.body["max_completion_tokens"])
	}

	msgs, _ := cs.body["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want system + user: %s", len(msgs), cs.raw)
	}
	first := msgs[0].(map[string]any)
	if first["role"] != "system" || !strings.Contains(cs.raw, "you are the planner") {
		t.Errorf("system message wrong: %s", cs.raw)
	}
	second := msgs[1].(map[string]any)
	if second["role"] != "user" || !strings.Contains(cs.raw, "add a greeting") {
		t.Errorf("user message wrong: %s", cs.raw)
	}
}

// With no system prompt there should be exactly one message, not an empty
// system message the API would reject or the model would find confusing.
func TestOpenAIWire_OmitsAnEmptySystemMessage(t *testing.T) {
	cs := newOpenAICapture(t)
	if _, err := openAIClientAgainst(cs).Generate(context.Background(), Request{Prompt: "hi"}); err != nil {
		t.Fatalf("generate: %v", err)
	}
	msgs, _ := cs.body["messages"].([]any)
	if len(msgs) != 1 {
		t.Errorf("got %d messages, want only the user message: %s", len(msgs), cs.raw)
	}
}

// Running out of output tokens mid-answer returns a valid-looking JSON prefix.
// Reporting success would fail much later as "unexpected end of JSON input",
// pointing at the model rather than at the real cause.
func TestOpenAIWire_TruncatedOutputIsAnError(t *testing.T) {
	cs := newOpenAICapture(t)
	cs.reply = openAIReply(`{"operations":[{"kind":"write","path":"big.go","content":"package main`, "length")

	_, err := openAIClientAgainst(cs).Generate(context.Background(), Request{Prompt: "rewrite big.go"})
	if !errors.Is(err, ErrTruncated) {
		t.Fatalf("got %v, want ErrTruncated", err)
	}
	if !strings.Contains(err.Error(), "too large for one response") {
		t.Errorf("error %q does not explain what to do about it", err)
	}
}

// A refusal is a real answer worth reporting, not an empty-content mystery.
func TestOpenAIWire_RefusalIsReported(t *testing.T) {
	cs := newOpenAICapture(t)
	b, _ := json.Marshal(map[string]any{
		"id": "chatcmpl-1", "object": "chat.completion", "created": 1, "model": "gpt-5.2",
		"choices": []map[string]any{{
			"index": 0,
			"message": map[string]any{
				"role": "assistant", "content": "", "refusal": "I cannot help with that",
			},
			"finish_reason": "stop",
		}},
	})
	cs.reply = string(b)

	_, err := openAIClientAgainst(cs).Generate(context.Background(), Request{Prompt: "hi"})
	if !errors.Is(err, ErrNoTextContent) {
		t.Fatalf("got %v, want ErrNoTextContent", err)
	}
	if !strings.Contains(err.Error(), "cannot help") {
		t.Errorf("error %q should carry the refusal reason", err)
	}
}

func TestOpenAIWire_EmptyContentIsAnError(t *testing.T) {
	cs := newOpenAICapture(t)
	cs.reply = openAIReply("", "stop")
	if _, err := openAIClientAgainst(cs).Generate(context.Background(), Request{Prompt: "hi"}); !errors.Is(err, ErrNoTextContent) {
		t.Errorf("got %v, want ErrNoTextContent", err)
	}
}

func TestOpenAIWire_NoChoicesIsAnError(t *testing.T) {
	cs := newOpenAICapture(t)
	cs.reply = `{"id":"c","object":"chat.completion","created":1,"model":"gpt-5.2","choices":[]}`
	if _, err := openAIClientAgainst(cs).Generate(context.Background(), Request{Prompt: "hi"}); !errors.Is(err, ErrNoTextContent) {
		t.Errorf("got %v, want ErrNoTextContent", err)
	}
}

func TestOpenAIWire_ReturnsTheContentAndModel(t *testing.T) {
	cs := newOpenAICapture(t)
	cs.reply = openAIReply(`{"summary":"the real answer"}`, "stop")

	got, err := openAIClientAgainst(cs).Generate(context.Background(), Request{Prompt: "hi"})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if !strings.Contains(got.Text, "the real answer") {
		t.Errorf("text = %q", got.Text)
	}
	if got.Model != "gpt-5.2" {
		t.Errorf("model = %q, want the model the API reported", got.Model)
	}
}

// The error mapping decides what a failed job's message says, so it has to
// survive a real SDK error rather than a hand-built one.
func TestOpenAIWire_MapsAPIErrors(t *testing.T) {
	tests := map[int]string{
		401: "check OPENAI_API_KEY",
		403: "project and model access",
		404: "check the configured model id",
		413: "reduce the prompt",
		429: "rate limited after retries",
		500: "after retries",
	}
	for status, want := range tests {
		t.Run(http.StatusText(status), func(t *testing.T) {
			cs := newOpenAICapture(t)
			cs.status = status
			cs.reply = `{"error":{"message":"nope","type":"invalid_request_error","code":"bad"}}`

			c := openAIClientAgainst(cs, option.WithMaxRetries(0))
			_, err := c.Generate(context.Background(), Request{Prompt: "hi"})
			if err == nil {
				t.Fatalf("expected an error for status %d", status)
			}
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error for %d = %q, want it to mention %q", status, err, want)
			}
		})
	}
}

// A 429 is both "slow down" and "you are out of credit", and those need
// different responses from whoever reads the failed job.
func TestOpenAIWire_QuotaErrorIsDistinctFromRateLimit(t *testing.T) {
	cs := newOpenAICapture(t)
	cs.status = http.StatusTooManyRequests
	cs.reply = `{"error":{"message":"You exceeded your current quota","type":"insufficient_quota","code":"insufficient_quota"}}`

	c := openAIClientAgainst(cs, option.WithMaxRetries(0))
	_, err := c.Generate(context.Background(), Request{Prompt: "hi"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "quota") {
		t.Errorf("error %q should distinguish an exhausted quota from a rate limit", err)
	}
}

// A cancelled job must not leave a model call running: the worker slot is only
// freed when Generate returns.
func TestOpenAIWire_RespectsContextCancellation(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer slow.Close()

	c := &OpenAIClient{
		api: openai.NewClient(
			option.WithAPIKey("test-key"),
			option.WithBaseURL(slow.URL),
			option.WithMaxRetries(0),
		),
		model: DefaultOpenAIModel,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := c.Generate(ctx, Request{Prompt: "hi"})
		done <- err
	}()

	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Error("expected an error after cancellation")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Generate ignored context cancellation")
	}
}

// A configured model id must reach the wire — otherwise ROJO_MODEL silently
// does nothing and every job runs on the default.
func TestOpenAIWire_HonoursTheConfiguredModel(t *testing.T) {
	cs := newOpenAICapture(t)
	c := &OpenAIClient{
		api:   openai.NewClient(option.WithAPIKey("k"), option.WithBaseURL(cs.URL)),
		model: shared.ChatModel("gpt-4o-mini"),
	}
	if _, err := c.Generate(context.Background(), Request{Prompt: "hi"}); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if cs.body["model"] != "gpt-4o-mini" {
		t.Errorf("model = %v, want the configured gpt-4o-mini", cs.body["model"])
	}
}

// NewOpenAIClient's option wiring is what production uses, so the constructor
// itself is exercised rather than only the struct literal the other tests build.
func TestNewOpenAIClient_AppliesOptions(t *testing.T) {
	cs := newOpenAICapture(t)
	c := NewOpenAIClient(OpenAIOptions{
		APIKey: "test-key", Model: "gpt-4.1", BaseURL: cs.URL, Timeout: 20 * time.Second,
	})
	if _, err := c.Generate(context.Background(), Request{Prompt: "hi"}); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if cs.body["model"] != "gpt-4.1" {
		t.Errorf("model = %v, want gpt-4.1", cs.body["model"])
	}
}

func TestNewOpenAIClient_DefaultsTheModel(t *testing.T) {
	if got := NewOpenAIClient(OpenAIOptions{APIKey: "k"}).model; got != DefaultOpenAIModel {
		t.Errorf("model = %q, want %q", got, DefaultOpenAIModel)
	}
}
