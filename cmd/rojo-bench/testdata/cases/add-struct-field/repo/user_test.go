package benchcase

import "testing"

func TestDescribe(t *testing.T) {
	u := NewUser("Ada")
	if u.Describe() != "Ada" {
		t.Fatalf("Describe() = %q", u.Describe())
	}
}
