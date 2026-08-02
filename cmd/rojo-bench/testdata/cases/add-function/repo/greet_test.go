package benchcase

import "testing"

func TestGreet(t *testing.T) {
	if got := Greet("world"); got != "hello world" {
		t.Fatalf("Greet(\"world\") = %q", got)
	}
}
