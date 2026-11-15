package proc

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"time"
)

// TerminateGrace bounds the wait between the SIGTERM and SIGKILL rungs of the
// kill ladder when the caller does not choose one.
const TerminateGrace = 5 * time.Second

// Kill sends sig to every process in the process group identified by pgid.
// The negative pid form is what makes this a group-wide signal.
func Kill(pgid int, sig syscall.Signal) error {
	return syscall.Kill(-pgid, sig)
}

// GroupAlive reports whether any process still exists in the process group.
// Zombies still count until reaped, so callers must reap the direct child
// before using this to assert a group is gone.
func GroupAlive(pgid int) bool {
	return syscall.Kill(-pgid, 0) == nil
}

// TerminateGroup runs the kill ladder against a whole process group: SIGTERM
// to the group, a grace period for orderly exit, then SIGKILL to the group.
// If the context is done before the grace period expires, the ladder skips
// straight to SIGKILL. A group that is already gone is a success.
func TerminateGroup(ctx context.Context, pgid int, grace time.Duration) error {
	if grace <= 0 {
		grace = TerminateGrace
	}
	if err := Kill(pgid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("terminate group %d: %w", pgid, err)
	}
	if !GroupAlive(pgid) {
		return nil
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
	}
	if !GroupAlive(pgid) {
		return nil
	}
	if err := Kill(pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("kill group %d: %w", pgid, err)
	}
	return nil
}
