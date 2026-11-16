package proc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// ErrCommandNotStarted identifies a control command that never created a process.
var ErrCommandNotStarted = errors.New("control command never started")

// RunCommand runs a finite control command in its own process group, returning
// combined output. Cancellation and completion clean up remaining descendants.
func RunCommand(ctx context.Context, dir string, argv ...string) (string, error) {
	if len(argv) == 0 {
		return "", fmt.Errorf("command argv is empty")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) // #nosec G204 -- callers supply validated operator configuration or fixed argv
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = time.Second
	kill := func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.Cancel = kill
	out, err := cmd.CombinedOutput()
	if cmd.Process != nil {
		_ = kill()
	}
	if cmd.Process == nil && err != nil {
		if ctx.Err() != nil {
			err = errors.Join(err, ctx.Err())
		}
		return string(out), fmt.Errorf("%w: %w", ErrCommandNotStarted, err)
	}
	if ctx.Err() != nil {
		return string(out), ctx.Err()
	}
	if errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.Success() {
		err = nil
	}
	return string(out), err
}
