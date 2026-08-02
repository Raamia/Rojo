package benchcase

import "testing"

func TestBenchParsePortError(t *testing.T) {
	n, err := ParsePort("8080")
	if err != nil {
		t.Fatalf("ParsePort(\"8080\") returned error %v, want nil", err)
	}
	if n != 8080 {
		t.Fatalf("ParsePort(\"8080\") = %d, want 8080", n)
	}
	if _, err := ParsePort("not-a-port"); err == nil {
		t.Fatal("ParsePort(\"not-a-port\") returned nil error, want a non-nil error")
	}
}
