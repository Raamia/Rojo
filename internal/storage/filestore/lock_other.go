//go:build !unix

package filestore

// acquireLock is a no-op where flock does not exist. Single-writer enforcement
// is a unix feature for now; the platforms Rojo actually targets (macOS and
// Linux, per the Dockerfile) both have it.
func acquireLock(string) (func(), error) { return func() {}, nil }
