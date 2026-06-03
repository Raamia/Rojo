package model

import "context"

type Request struct {
	System   string
	Prompt   string
	MaxToks  int
	Metadata map[string]string
}

type Response struct {
	Text  string
	Model string
}

type Client interface {
	Generate(ctx context.Context, req Request) (Response, error)
}

type FakeClient struct {
	Reply string
	Err   error
}

func (f *FakeClient) Generate(_ context.Context, _ Request) (Response, error) {
	if f.Err != nil {
		return Response{}, f.Err
	}
	return Response{Text: f.Reply, Model: "fake"}, nil
}
