package benchcase

import "testing"

func TestValidateName(t *testing.T) {
	if err := (Config{Name: ""}).Validate(); err == nil {
		t.Fatal("empty name was accepted")
	}
	if err := (Config{Name: "svc"}).Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}
