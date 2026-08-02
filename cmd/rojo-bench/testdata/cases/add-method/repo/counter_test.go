package benchcase

import "testing"

func TestCounterInc(t *testing.T) {
	var c Counter
	c.Inc()
	c.Inc()
	if c.Value() != 2 {
		t.Fatalf("Value() = %d, want 2", c.Value())
	}
}
