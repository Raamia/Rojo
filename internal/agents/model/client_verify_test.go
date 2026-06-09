package model

import (
	"context"
	"errors"
	"testing"
)

// FakeClient satisfies the Client interface.
var _ Client = (*FakeClient)(nil)

func TestFakeClientGenerate_ReturnsReplyAsText(t *testing.T) {
	f := &FakeClient{Reply: "hello world"}
	resp, err := f.Generate(context.Background(), Request{Prompt: "hi"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Text != "hello world" {
		t.Fatalf("Text: got %q, want %q", resp.Text, "hello world")
	}
	// FakeClient has no configurable Model field; Generate hard-codes "fake".
	if resp.Model != "fake" {
		t.Fatalf("Model: got %q, want %q", resp.Model, "fake")
	}
}

func TestFakeClientGenerate_EmptyReply(t *testing.T) {
	f := &FakeClient{}
	resp, err := f.Generate(context.Background(), Request{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Text != "" {
		t.Fatalf("Text: got %q, want empty", resp.Text)
	}
	if resp.Model != "fake" {
		t.Fatalf("Model: got %q, want %q", resp.Model, "fake")
	}
}

func TestFakeClientGenerate_ReturnsConfiguredError(t *testing.T) {
	sentinel := errors.New("model unavailable")
	f := &FakeClient{Reply: "ignored", Err: sentinel}
	resp, err := f.Generate(context.Background(), Request{})
	if !errors.Is(err, sentinel) {
		t.Fatalf("error: got %v, want %v", err, sentinel)
	}
	// On error the Response is zero-valued (Reply is NOT surfaced).
	if resp != (Response{}) {
		t.Fatalf("Response on error: got %+v, want zero value", resp)
	}
}

// Characterizes context handling: Generate ignores ctx (param is `_`), so a
// cancelled context does NOT produce an error. This documents actual behavior.
func TestFakeClientGenerate_IgnoresCancelledContext(t *testing.T) {
	f := &FakeClient{Reply: "still works"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	resp, err := f.Generate(ctx, Request{})
	if err != nil {
		t.Fatalf("cancelled ctx: got error %v, want nil (ctx is ignored)", err)
	}
	if resp.Text != "still works" {
		t.Fatalf("Text: got %q, want %q", resp.Text, "still works")
	}
}

// Err takes precedence over Reply regardless of Reply's value.
func TestFakeClientGenerate_ErrPrecedesReply(t *testing.T) {
	f := &FakeClient{Reply: "should not appear", Err: errors.New("boom")}
	resp, err := f.Generate(context.Background(), Request{})
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if resp.Text != "" {
		t.Fatalf("Text on error: got %q, want empty", resp.Text)
	}
}
