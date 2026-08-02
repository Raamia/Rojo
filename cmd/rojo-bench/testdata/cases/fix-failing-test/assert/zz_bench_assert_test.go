package benchcase

import "testing"

func TestBenchSum(t *testing.T) {
	cases := []struct {
		in   []int
		want int
	}{
		{nil, 0},
		{[]int{}, 0},
		{[]int{5}, 5},
		{[]int{1, 2, 3}, 6},
		{[]int{-1, 1}, 0},
	}
	for _, c := range cases {
		if got := Sum(c.in); got != c.want {
			t.Errorf("Sum(%v) = %d, want %d", c.in, got, c.want)
		}
	}
}
