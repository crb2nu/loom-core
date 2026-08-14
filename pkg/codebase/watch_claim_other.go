//go:build !unix

package codebase

import "os"

// flockExclusive is a no-op on platforms without flock; watch run-claims fall
// back to single-process semantics (in-memory dedup still applies).
func flockExclusive(f *os.File) bool {
	return true
}
