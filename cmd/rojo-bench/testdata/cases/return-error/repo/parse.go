package benchcase

import "strconv"

// ParsePort converts s to a port number.
func ParsePort(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}
