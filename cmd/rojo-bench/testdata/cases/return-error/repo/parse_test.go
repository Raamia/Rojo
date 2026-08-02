package benchcase

import "testing"

func TestParsePort(t *testing.T) {
	if got := ParsePort("8080"); got != 8080 {
		t.Fatalf("ParsePort(\"8080\") = %d, want 8080", got)
	}
}
