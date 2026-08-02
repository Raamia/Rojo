package benchcase

// User is a person in the system.
type User struct {
	Name string
}

// NewUser builds a User.
func NewUser(name string) *User {
	return &User{Name: name}
}

// Describe renders the user as a single line.
func (u *User) Describe() string {
	return u.Name
}
