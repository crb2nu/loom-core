//go:build !unix

package gates

import "errors"

// statfsUsage has no portable implementation off unix. Returning an error
// makes the filesystem probe report the capacity check as unavailable, which
// the admission gate treats as unknown evidence and blocks on — the operator
// only ships on linux, so a non-unix build failing closed is correct.
func statfsUsage(string) (float64, float64, error) {
	return 0, 0, errors.New("filesystem capacity statistics are not supported on this platform")
}
