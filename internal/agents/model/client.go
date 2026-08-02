package model

import "context"

type Request struct {
	System   string
	Prompt   string
	MaxToks  int
	Metadata map[string]string
}

// Usage is what one call consumed. Both providers report it, and it is the
// only honest basis for a cost figure — inferring tokens from string length is
// a guess that is wrong by a different margin for every model.
//
// The two fields are deliberately provider-neutral names: Anthropic calls them
// input/output and OpenAI calls them prompt/completion, and the agents should
// not have to know which backend answered in order to read a number.
type Usage struct {
	InputTokens  int64
	OutputTokens int64
}

func (u Usage) Total() int64 { return u.InputTokens + u.OutputTokens }

type Response struct {
	Text  string
	Model string
	// Usage is zero when the provider did not report it. Callers should treat
	// a zero total as "unknown", not as "free".
	Usage Usage
}

type Client interface {
	Generate(ctx context.Context, req Request) (Response, error)
}

type FakeClient struct {
	Reply string
	Err   error
	// Usage lets a test drive the accounting path without a real provider.
	Usage Usage
}

func (f *FakeClient) Generate(_ context.Context, _ Request) (Response, error) {
	if f.Err != nil {
		return Response{}, f.Err
	}
	return Response{Text: f.Reply, Model: "fake", Usage: f.Usage}, nil
}
