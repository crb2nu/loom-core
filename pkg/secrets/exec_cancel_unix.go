//go:build darwin || linux

package secrets

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

const commandCancelGracePeriod = 100 * time.Millisecond

// configureCommandCancellation isolates credential helpers in a process group
// so cancellation reaches descendants that inherited the command's output
// pipes. A short TERM grace lets cooperative parents reap children before a
// final group-wide KILL bounds shutdown for uncooperative processes.
func configureCommandCancellation(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		processGroup := -cmd.Process.Pid
		if err := syscall.Kill(processGroup, syscall.SIGTERM); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}
			return err
		}
		timer := time.NewTimer(commandCancelGracePeriod)
		defer timer.Stop()
		<-timer.C
		if err := syscall.Kill(processGroup, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
		return nil
	}
}
