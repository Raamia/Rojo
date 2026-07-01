//go:build unix

package filestore

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// acquireLock takes an exclusive advisory lock on <dir>/.lock and returns the
// release func.
//
// Two servers pointed at one data directory would each rebuild their own
// in-memory index and overwrite job.json behind each other's backs — silent
// corruption with no error anywhere, because every individual write succeeds.
// The classic cause is mundane: a second `make run` in another terminal.
// Refusing at startup turns that into one clear message instead.
//
// flock rather than a PID file because the kernel releases it when the process
// dies, however it dies. A PID file left behind by a crash blocks the next
// start until a human deletes it; a dead process's flock just evaporates.
// Advisory locks bind other cooperating processes only, which is exactly the
// scope needed: the enemy is a second rojo, not a hostile root.
func acquireLock(dir string) (func(), error) {
	path := filepath.Join(dir, ".lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("data dir %s is already in use by another rojo process "+
			"(stop it, or point ROJO_DATA_DIR elsewhere): %w", dir, err)
	}
	return func() {
		// Closing the descriptor releases the flock.
		_ = f.Close()
	}, nil
}
