package benchcase

import (
	"strings"
	"testing"
)

func TestBenchValidateTimeout(t *testing.T) {
	err := Config{Name: "svc", Timeout: -1}.Validate()
	if err == nil {
		t.Fatal("negative Timeout was accepted, want an error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "timeout") {
		t.Fatalf("error %q does not mention timeout", err)
	}
	if err := (Config{Name: "svc", Timeout: 30}).Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}
