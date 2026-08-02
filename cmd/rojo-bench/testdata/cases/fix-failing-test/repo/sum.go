package benchcase

// Sum returns the total of nums.
func Sum(nums []int) int {
	total := 0
	for i := 1; i < len(nums); i++ {
		total += nums[i]
	}
	return total
}
