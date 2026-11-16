package proc

import (
	"errors"
	"io"
	"os/exec"
	"syscall"
)

// ErrBindingUnavailable means a new child was stopped before prompting because its durable identity could not be recorded.
var ErrBindingUnavailable = errors.New("agent process binding unavailable")

// AbortStart kills and reaps a newly spawned child whose durable binding failed.
// Call it before starting a wait owner; it also closes every parent-owned pipe.
func AbortStart(cmd *exec.Cmd, pipes ...io.Closer) {
	if cmd.Process != nil {
		_ = Kill(cmd.Process.Pid, syscall.SIGKILL)
	}
	for _, pipe := range pipes {
		_ = pipe.Close()
	}
	_ = cmd.Wait()
}
