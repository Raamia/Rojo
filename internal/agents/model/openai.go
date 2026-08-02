package model

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
)

// DefaultOpenAIModel is used when none is configured.
//
// GPT-5.2 is a current general-purpose model with the reasoning depth this
// pipeline wants: every agent is asked for a structured plan, patch, or review,
// which is exactly the work worth spending thinking on. Override with
// ROJO_MODEL for a cheaper or newer one.
const DefaultOpenAIModel = shared.ChatModelGPT5_2

// OpenAIClient is a production Client backed by OpenAI's official SDK.
//
// It exists behind the same Client interface as AnthropicClient, which is the
// whole point of that interface: the planner, implementor and reviewer never
// learn which provider answered them, and switching is a config change rather
// than a code change.
type OpenAIClient struct {
	api   openai.Client
	model shared.ChatModel
}

// OpenAIOptions configures the client. The zero value is usable: the SDK reads
// OPENAI_API_KEY from the environment, and the model and timeout fall back to
// package defaults.
type OpenAIOptions struct {
	APIKey  string
	Model   string
	Timeout time.Duration
	// MaxRetries bounds the SDK's automatic retry of 408/409/429/5xx and
	// connection errors. Zero uses the SDK default (2).
	MaxRetries int
	// BaseURL overrides the API endpoint. Empty means the real API. It exists
	// for pointing the client at a proxy, a gateway, or an OpenAI-compatible
	// server — and it is what lets the wire tests drive this code path through
	// the real SDK against a stub, rather than never exercising it at all.
	BaseURL string
}

func NewOpenAIClient(opts OpenAIOptions) *OpenAIClient {
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

	m := DefaultOpenAIModel
	if opts.Model != "" {
		m = shared.ChatModel(opts.Model)
	}
	return &OpenAIClient{api: openai.NewClient(clientOpts...), model: m}
}

// Generate sends one request and returns the model's text.
//
// JSON mode is requested rather than left to the prompt. Every agent here parses
// the reply as JSON, and asking the API to guarantee a JSON object removes the
// commonest failure this codebase has already had to work around — a model
// wrapping its answer in a markdown fence. The agents still unfence defensively,
// because the guarantee is per-provider and the parsers are shared.
func (c *OpenAIClient) Generate(ctx context.Context, req Request) (Response, error) {
	maxToks := req.MaxToks
	if maxToks <= 0 {
		maxToks = DefaultMaxTokens
	}

	messages := make([]openai.ChatCompletionMessageParamUnion, 0, 2)
	if req.System != "" {
		messages = append(messages, openai.SystemMessage(req.System))
	}
	messages = append(messages, openai.UserMessage(req.Prompt))

	params := openai.ChatCompletionNewParams{
		Model:    c.model,
		Messages: messages,
		// max_completion_tokens, not the deprecated max_tokens: reasoning
		// models reject the old field outright, and this one is accepted
		// everywhere, so it is the single spelling that works across the range.
		MaxCompletionTokens: openai.Int(int64(maxToks)),
		ResponseFormat: openai.ChatCompletionNewParamsResponseFormatUnion{
			OfJSONObject: &shared.ResponseFormatJSONObjectParam{},
		},
		// Temperature, top_p and the rest are deliberately unset. Newer
		// reasoning models reject them with a 400 — the same trap the Anthropic
		// client sidesteps — and the defaults are right for structured output.
	}

	completion, err := c.api.Chat.Completions.New(ctx, params)
	if err != nil {
		return Response{}, describeOpenAIError(err)
	}
	if len(completion.Choices) == 0 {
		return Response{}, fmt.Errorf("%w (no choices returned)", ErrNoTextContent)
	}

	choice := completion.Choices[0]
	// "length" is OpenAI's max-tokens stop reason. Same trap as the Anthropic
	// side: the truncated text is a valid-looking JSON prefix that fails much
	// later as "unexpected end of JSON input", pointing at the wrong thing. The
	// implementor is asked for whole files, so this is routine on real repos.
	if choice.FinishReason == "length" {
		return Response{}, fmt.Errorf("%w: %d tokens were not enough to finish the answer; "+
			"the change is probably too large for one response", ErrTruncated, maxToks)
	}
	// A refusal is the model declining, which is a real answer to report rather
	// than an empty-content mystery for the caller to puzzle over.
	if r := strings.TrimSpace(choice.Message.Refusal); r != "" {
		return Response{}, fmt.Errorf("%w: model refused: %s", ErrNoTextContent, r)
	}
	if strings.TrimSpace(choice.Message.Content) == "" {
		return Response{}, fmt.Errorf("%w (finish reason %q)", ErrNoTextContent, choice.FinishReason)
	}

	return Response{
		Text:  choice.Message.Content,
		Model: completion.Model,
		// prompt/completion are OpenAI's names for the same two numbers
		// Anthropic calls input/output. CompletionTokens includes reasoning
		// tokens, which is what makes it the billable figure rather than a
		// count of what came back as text.
		Usage: Usage{
			InputTokens:  completion.Usage.PromptTokens,
			OutputTokens: completion.Usage.CompletionTokens,
		},
	}, nil
}

// describeOpenAIError turns an SDK error into one that says what went wrong and
// whether retrying is worthwhile — the orchestrator logs it and the job's
// failure message carries it, so a bare status code is not enough.
//
// The SDK already retries 408/409/429/5xx with backoff, so an error surfacing
// here means those retries were exhausted or the status is not retryable.
func describeOpenAIError(err error) error {
	var apiErr *openai.Error
	if !errors.As(err, &apiErr) {
		// No HTTP response at all: DNS, dial, or a cancelled context.
		return fmt.Errorf("call openai api: %w", err)
	}

	switch apiErr.StatusCode {
	case 401:
		return fmt.Errorf("openai auth failed (check OPENAI_API_KEY): %w", err)
	case 403:
		return fmt.Errorf("openai denied this request (check the key's project and model access): %w", err)
	case 404:
		return fmt.Errorf("openai model not found (check the configured model id): %w", err)
	case 413:
		return fmt.Errorf("openai request too large (reduce the prompt or context): %w", err)
	case 429:
		// 429 covers both rate limits and an exhausted quota, and those need
		// very different responses from whoever reads the job's failure.
		if strings.Contains(strings.ToLower(apiErr.Code), "quota") {
			return fmt.Errorf("openai quota exceeded (check billing): %w", err)
		}
		return fmt.Errorf("openai rate limited after retries: %w", err)
	default:
		if apiErr.StatusCode >= 500 {
			return fmt.Errorf("openai server error %d after retries: %w", apiErr.StatusCode, err)
		}
		return fmt.Errorf("openai request rejected (%d): %w", apiErr.StatusCode, err)
	}
}

// Compile-time proof that the client satisfies the interface the agents use.
var _ Client = (*OpenAIClient)(nil)
