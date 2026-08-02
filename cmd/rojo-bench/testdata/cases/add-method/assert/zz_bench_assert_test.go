package benchcase

import "testing"

func TestBenchCounterReset(t *testing.T) {
	var c Counter
	c.Inc()
	c.Inc()
	c.Reset()
	if c.Value() != 0 {
		t.Fatalf("after Reset, Value() = %d, want 0", c.Value())
	}
	c.Inc()
	if c.Value() != 1 {
		t.Fatalf("counting after Reset is broken: Value() = %d, want 1", c.Value())
	}
}
