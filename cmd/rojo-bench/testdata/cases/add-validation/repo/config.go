package benchcase

import "errors"

// Config holds service settings.
type Config struct {
	Name    string
	Timeout int
}

// Validate reports whether the configuration is usable.
func (c Config) Validate() error {
	if c.Name == "" {
		return errors.New("name must not be empty")
	}
	return nil
}
