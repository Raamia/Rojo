package benchcase

// Counter counts events.
type Counter struct {
	n int
}

// Inc adds one to the count.
func (c *Counter) Inc() { c.n++ }

// Value reports the current count.
func (c *Counter) Value() int { return c.n }
