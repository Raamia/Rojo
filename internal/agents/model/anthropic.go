package model

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// DefaultModel is the model used when none is configured.
const DefaultModel = anthropic.ModelClaudeOpus4_8

// DefaultMaxTokens bounds a single completion. 16k is the recommended ceiling
// for non-streaming requests: larger outputs risk hitting the HTTP timeout
// before the response is complete, and would need streaming instead.
const DefaultMaxTokens = 16000

// ErrNoTextContent means the model returned a response carrying no text — a
// thinking-only or tool-only reply. Callers parse the text, so an empty one is
// a failure rather than an empty success.
var ErrNoTextContent = errors.New("model returned no text content")

// ErrTruncated means the model hit its output limit mid-answer.
//
// This matters more here than it looks. The implementor is asked for the
// complete new contents of every file it changes, so one large file can exhaust
// the budget partway through — and the truncated text is still valid-looking
// prose that fails much later as "unexpected end of JSON input", which reads
// like the model returned nonsense rather than like it ran out of room.
var ErrTruncated = errors.New("model output was cut off at the token limit")

// AnthropicClient is the production Client, backed by the official SDK.
//
// It exists behind the Client interface so the rest of the codebase never
// imports the SDK: agents depend on Generate, and tests use FakeClient. That
// boundary is why swapping providers later touches exactly this file.
type AnthropicClient struct {
	api   anthropic.Client
	model anthropic.Model
}

// AnthropicOptions configures the client. The zero value is usable: the SDK
// reads ANTHROPIC_API_KEY from the environment, and the model and timeout fall
// back to package defaults.
type AnthropicOptions struct {
	APIKey  string
	Model   string
	Timeout time.Duration
	// MaxRetries bounds the SDK's automatic retry of 408/409/429/5xx and
	// connection errors. Zero uses the SDK default (2).
	MaxRetries int
	// BaseURL overrides the API endpoint. Empty means the real API. It exists
	// for pointing the client at a proxy or gateway, and it is what lets the
	// end-to-end tests drive the whole pipeline through the real SDK against a
	// stub — the alternative being to never exercise this code path at all.
	BaseURL string
}

func NewAnthropicClient(opts AnthropicOptions) *AnthropicClient {
	clientOpts := []option.RequestOption{}
	if opts.APIKey != "" {
		clientOpts = append(clientOpts, option.WithAPIKey(opts.APIKey))
	}
	if opts.Timeout > 0 {
		clientOpts = append(clientOpts, option.WithRequestTimeout(opts.Timeout))
	}
	if opts.MaxRetries > 0 {
		clientOpts = append(clientOpts, option.WithMaxRetries(opts.MaxRetries))
	}
	if opts.BaseURL != "" {
		clientOpts = append(clientOpts, option.WithBaseURL(opts.BaseURL))
	}

	m := DefaultModel
	if opts.Model != "" {
		m = anthropic.Model(opts.Model)
	}
	return &AnthropicClient{api: anthropic.NewClient(clientOpts...), model: m}
}

// Generate sends one request and returns the model's text.
//
// Adaptive thinking is enabled: the model decides how much to reason per
// request rather than being given a fixed token budget. Every agent in this
// codebase asks for a structured plan, patch, or review — work where letting
// the model think is worth the tokens.
func (c *AnthropicClient) Generate(ctx context.Context, req Request) (Response, error) {
	maxToks := req.MaxToks
	if maxToks <= 0 {
		maxToks = DefaultMaxTokens
	}

	params := anthropic.MessageNewParams{
		Model:     c.model,
		MaxTokens: int64(maxToks),
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(req.Prompt)),
		},
		Thinking: anthropic.ThinkingConfigParamUnion{
			OfAdaptive: &anthropic.ThinkingConfigAdaptiveParam{},
		},
	}
	if req.System != "" {
		params.System = []anthropic.TextBlockParam{{Text: req.System}}
	}

	msg, err := c.api.Messages.New(ctx, params)
	if err != nil {
		return Response{}, describeAPIError(err)
	}

	// Thinking blocks precede text blocks in the response; only the text is
	// the answer the caller asked for.
	var text strings.Builder
	for _, block := range msg.Content {
		if t, ok := block.AsAny().(anthropic.TextBlock); ok {
			text.WriteString(t.Text)
		}
	}
	if strings.TrimSpace(text.String()) == "" {
		return Response{}, fmt.Errorf("%w (stop reason %q)", ErrNoTextContent, msg.StopReason)
	}
	// Checked after the empty case so a thinking-only reply still reports the
	// more specific problem.
	if msg.StopReason == anthropic.StopReasonMaxTokens {
		return Response{}, fmt.Errorf("%w: %d tokens were not enough to finish the answer; "+
			"the change is probably too large for one response", ErrTruncated, maxToks)
	}

	return Response{Text: text.String(), Model: string(msg.Model)}, nil
}

// describeAPIError turns an SDK error into one that says what went wrong and
// whether retrying is worthwhile — the orchestrator logs it and the job's
// failure message carries it, so "400" alone is not enough.
//
// The SDK already retries 408/409/429/5xx with backoff, so an error surfacing
// here means those retries were exhausted or the status is not retryable.
func describeAPIError(err error) error {
	var apiErr *anthropic.Error
	if !errors.As(err, &apiErr) {
		// No HTTP response at all: DNS, dial, or a cancelled context.
		return fmt.Errorf("call anthropic api: %w", err)
	}

	switch apiErr.StatusCode {
	case 401:
		return fmt.Errorf("anthropic auth failed (check ANTHROPIC_API_KEY): %w", err)
	case 404:
		return fmt.Errorf("anthropic model not found (check the configured model id): %w", err)
	case 413:
		return fmt.Errorf("anthropic request too large (reduce the prompt or context): %w", err)
	case 429:
		return fmt.Errorf("anthropic rate limited after retries: %w", err)
	default:
		if apiErr.StatusCode >= 500 {
			return fmt.Errorf("anthropic server error %d after retries: %w", apiErr.StatusCode, err)
		}
		return fmt.Errorf("anthropic request rejected (%d): %w", apiErr.StatusCode, err)
	}
}
