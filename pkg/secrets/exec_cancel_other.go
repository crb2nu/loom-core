//go:build !darwin && !linux

package secrets

import "os/exec"

// configureCommandCancellation retains exec.CommandContext's portable
// single-process cancellation where process groups are unavailable.
func configureCommandCancellation(_ *exec.Cmd) {}
