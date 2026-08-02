package benchcase

import "testing"

func TestBenchFarewell(t *testing.T) {
	if got := Farewell("world"); got != "goodbye world" {
		t.Fatalf("Farewell(\"world\") = %q, want %q", got, "goodbye world")
	}
	if got := Farewell(""); got != "goodbye " {
		t.Fatalf("Farewell(\"\") = %q, want %q", got, "goodbye ")
	}
}
