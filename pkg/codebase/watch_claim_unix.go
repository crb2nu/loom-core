//go:build unix

package codebase

import (
	"os"

	"golang.org/x/sys/unix"
)

// flockExclusive takes a non-blocking exclusive flock on f. flock locks are
// held by the open file description and released by the kernel when the
// owning process exits, which is exactly the lifetime a watch run-claim needs.
func flockExclusive(f *os.File) bool {
	return unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB) == nil
}
