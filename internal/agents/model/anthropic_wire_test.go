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

// These tests point the real SDK at a local server, so they exercise the actual
// request the production client would send without needing an API key. What
// they check is the wire format — the part that cannot be verified by reading
// the code, because it is the SDK's serialisation that decides it.

// captureServer records the request body and replies with a canned message.
type captureServer struct {
	*httptest.Server
	body       map[string]any
	raw        string
	status     int
	reply      string
	stopReason string
}

func newCaptureServer(t *testing.T) *captureServer {
	t.Helper()
	cs := &captureServer{status: http.StatusOK}
	cs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		cs.raw = string(b)
		_ = json.Unmarshal(b, &cs.body)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(cs.status)
		if cs.reply != "" && strings.HasPrefix(strings.TrimSpace(cs.reply), "{\"id\"") {
			_, _ = io.WriteString(w, cs.reply) // a full canned envelope
			return
		}
		text := cs.reply
		if text == "" {
			text = `{"summary":"ok"}`
		}
		stop := cs.stopReason
		if stop == "" {
			stop = "end_turn"
		}
		envelope, _ := json.Marshal(map[string]any{
			"id": "msg_1", "type": "message", "role": "assistant",
			"model":       "claude-opus-4-8",
			"content":     []map[string]any{{"type": "text", "text": text}},
			"stop_reason": stop,
			"usage":       map[string]int{"input_tokens": 10, "output_tokens": 5},
		})
		_, _ = w.Write(envelope)
	}))
	t.Cleanup(cs.Close)
	return cs
}

// clientAgainst builds the production client pointed at the local server. The
// SDK client is immutable once constructed, so the base URL goes in up front.
func clientAgainst(cs *captureServer, extra ...option.RequestOption) *AnthropicClient {
	opts := append([]option.RequestOption{
		option.WithAPIKey("test-key"),
		option.WithBaseURL(cs.URL),
		option.WithRequestTimeout(30 * time.Second),
	}, extra...)
	return &AnthropicClient{api: anthropic.NewClient(opts...), model: DefaultModel}
}

// Opus 4.8 rejects thinking.budget_tokens with a 400, and adaptive thinking is
// what replaced it. Reading the Go struct does not prove what goes on the wire.
func TestWire_SendsAdaptiveThinking(t *testing.T) {
	cs := newCaptureServer(t)
	if _, err := clientAgainst(cs).Generate(context.Background(), Request{Prompt: "hi"}); err != nil {
		t.Fatalf("generate: %v", err)
	}

	thinking, ok := cs.body["thinking"].(map[string]any)
	if !ok {
		t.Fatalf("no thinking block in the request: %s", cs.raw)
	}
	if thinking["type"] != "adaptive" {
		t.Errorf("thinking.type = %v, want adaptive", thinking["type"])
	}
	if _, present := thinking["budget_tokens"]; present {
		t.Errorf("budget_tokens is rejected by this model with a 400: %s", cs.raw)
	}
}

// Opus 4.8 also rejects temperature, top_p and top_k. Sending any of them would
// fail every request, which is the kind of thing worth failing a test over
// rather than discovering in production.
func TestWire_OmitsRejectedSamplingParams(t *testing.T) {
	cs := newCaptureServer(t)
	if _, err := clientAgainst(cs).Generate(context.Background(), Request{Prompt: "hi"}); err != nil {
		t.Fatalf("generate: %v", err)
	}
	for _, forbidden := range []string{"temperature", "top_p", "top_k"} {
		if _, present := cs.body[forbidden]; present {
			t.Errorf("request carries %q, which this model rejects: %s", forbidden, cs.raw)
		}
	}
}

func TestWire_CarriesModelSystemAndPrompt(t *testing.T) {
	cs := newCaptureServer(t)
	_, err := clientAgainst(cs).Generate(context.Background(), Request{
		System: "you are the planner", Prompt: "add a greeting", MaxToks: 4096,
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	if cs.body["model"] != string(DefaultModel) {
		t.Errorf("model = %v, want %s", cs.body["model"], DefaultModel)
	}
	if cs.body["max_tokens"] != float64(4096) {
		t.Errorf("max_tokens = %v, want 4096", cs.body["max_tokens"])
	}
	if !strings.Contains(cs.raw, "you are the planner") {
		t.Errorf("system prompt missing: %s", cs.raw)
	}
	if !strings.Contains(cs.raw, "add a greeting") {
		t.Errorf("user prompt missing: %s", cs.raw)
	}
}

func TestWire_DefaultMaxTokensWhenUnset(t *testing.T) {
	cs := newCaptureServer(t)
	if _, err := clientAgainst(cs).Generate(context.Background(), Request{Prompt: "hi"}); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if cs.body["max_tokens"] != float64(DefaultMaxTokens) {
		t.Errorf("max_tokens = %v, want %d", cs.body["max_tokens"], DefaultMaxTokens)
	}
}

// A real reply carries thinking blocks before the text. Only the text is the
// answer; concatenating the thinking into it would feed reasoning prose to a
// JSON parser.
func TestWire_ExtractsTextPastThinkingBlocks(t *testing.T) {
	cs := newCaptureServer(t)
	cs.reply = `{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-4-8",
		"content":[
			{"type":"thinking","thinking":"Let me consider the options here...","signature":"sig"},
			{"type":"text","text":"{\"summary\":\"the real answer\"}"}
		],
		"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`

	got, err := clientAgainst(cs).Generate(context.Background(), Request{Prompt: "hi"})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if strings.Contains(got.Text, "consider the options") {
		t.Errorf("reasoning leaked into the answer: %q", got.Text)
	}
	if !strings.Contains(got.Text, "the real answer") {
		t.Errorf("text = %q", got.Text)
	}
}

// A thinking-only reply has no answer in it. Returning empty success would hand
// the caller "" to parse as JSON, failing further from the cause.
func TestWire_ThinkingOnlyReplyIsAnError(t *testing.T) {
	cs := newCaptureServer(t)
	cs.reply = `{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-4-8",
		"content":[{"type":"thinking","thinking":"still thinking","signature":"sig"}],
		"stop_reason":"max_tokens","usage":{"input_tokens":10,"output_tokens":5}}`

	_, err := clientAgainst(cs).Generate(context.Background(), Request{Prompt: "hi"})
	if err == nil {
		t.Fatal("expected an error for a reply with no text")
	}
	if !strings.Contains(err.Error(), "max_tokens") {
		t.Errorf("error %q should name the stop reason — that is the diagnosis", err)
	}
}

// The API error path decides what a failed job's message says, so the mapping
// has to survive a real SDK error rather than a hand-built one.
func TestWire_MapsAPIErrors(t *testing.T) {
	tests := map[int]string{
		401: "check ANTHROPIC_API_KEY",
		404: "check the configured model id",
		413: "reduce the prompt",
		500: "after retries",
	}
	for status, want := range tests {
		t.Run(http.StatusText(status), func(t *testing.T) {
			cs := newCaptureServer(t)
			cs.status = status
			cs.reply = `{"type":"error","error":{"type":"invalid_request_error","message":"nope"}}`

			c := clientAgainst(cs, option.WithMaxRetries(0))

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

// A cancelled job must not leave a model call running: the worker slot is only
// freed when Generate returns.
func TestWire_RespectsContextCancellation(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer slow.Close()

	c := &AnthropicClient{
		api: anthropic.NewClient(
			option.WithAPIKey("test-key"),
			option.WithBaseURL(slow.URL),
			option.WithMaxRetries(0),
		),
		model: DefaultModel,
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

// Running out of output tokens mid-answer returned success with truncated JSON,
// so the job failed much later as "unexpected end of JSON input" — which reads
// like the model returned nonsense rather than like it ran out of room. The
// implementor is asked for whole files, so this is a routine failure on real
// repositories, not an exotic one.
func TestWire_TruncatedOutputIsAnError(t *testing.T) {
	cs := newCaptureServer(t)
	cs.reply = `{"operations":[{"kind":"write","path":"big.go","content":"package main\nfunc B(`
	cs.stopReason = "max_tokens"

	_, err := clientAgainst(cs).Generate(context.Background(), Request{Prompt: "rewrite big.go"})
	if !errors.Is(err, ErrTruncated) {
		t.Fatalf("got %v, want ErrTruncated", err)
	}
	// The message has to point at the cause, not just the symptom.
	if !strings.Contains(err.Error(), "too large for one response") {
		t.Errorf("error %q does not explain what to do about it", err)
	}
}

// A complete answer is not truncated, whatever its length.
func TestWire_NormalStopReasonIsNotAnError(t *testing.T) {
	cs := newCaptureServer(t)
	cs.stopReason = "end_turn"
	if _, err := clientAgainst(cs).Generate(context.Background(), Request{Prompt: "hi"}); err != nil {
		t.Errorf("end_turn should not be an error: %v", err)
	}
}
