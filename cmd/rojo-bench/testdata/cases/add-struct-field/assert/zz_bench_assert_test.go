package benchcase

import "testing"

func TestBenchUserEmail(t *testing.T) {
	u := NewUser("Ada", "ada@example.com")
	if u.Email != "ada@example.com" {
		t.Fatalf("Email = %q, want ada@example.com", u.Email)
	}
	if u.Name != "Ada" {
		t.Fatalf("Name = %q, want Ada", u.Name)
	}
	if got := u.Describe(); got != "Ada <ada@example.com>" {
		t.Fatalf("Describe() = %q, want %q", got, "Ada <ada@example.com>")
	}
}
