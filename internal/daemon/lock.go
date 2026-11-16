// Package daemon assembles the lambe supervisor lifecycle: the
// single-instance state lock, startup recovery of orphaned process groups,
// the tick loop with hub sync, the claim heartbeat, signal handling, and the
// morning-report status snapshot.
package daemon

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// ErrLockHeld reports that another lambe instance already holds the
// single-instance state lock. The caller must exit immediately; the holder
// is unaffected.
var ErrLockHeld = errors.New("another lambe instance holds the state lock")

// StateLock is the single-instance lock: an exclusive, non-blocking flock
// on a file under the lambe state dir, held for the daemon's whole life.
// The kernel releases it on any exit path, including SIGKILL, so the lock —
// never the PID file — is the only arbiter of instance uniqueness.
type StateLock struct {
	file    *os.File
	path    string
	pidPath string
}

// AcquireStateLock takes the exclusive lock on path, creating it (and its
// parent directory) as needed, and writes the daemon PID next to it as a
// human hint. A held lock returns an error wrapping ErrLockHeld.
func AcquireStateLock(path string) (*StateLock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("state dir: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) // #nosec G304 -- path is daemon state configuration
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: %s; use lambe tui or lambe logs -follow to view the running daemon; quitting a viewer does not stop it; stop the daemon in its terminal before restarting", ErrLockHeld, path)
		}
		return nil, fmt.Errorf("flock %s: %w", path, err)
	}

	lock := &StateLock{file: file, path: path, pidPath: path + ".pid"}
	// The PID file is a diagnostic hint for humans; it is never consulted
	// as an arbiter of instance uniqueness, so a failure to write it is
	// logged and the lock stands.
	if err := os.WriteFile(lock.pidPath, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		slog.Default().Warn("pid hint write failed, continuing without it",
			"path", lock.pidPath, "error", err)
	}
	return lock, nil
}

// PidHint returns the PID recorded beside the lock, or zero when absent or
// unparsable. It is for display only.
func (l *StateLock) PidHint() int {
	data, err := os.ReadFile(l.pidPath) // #nosec G304 -- derived from the lock path this instance chose
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0
	}
	return pid
}

// Release drops the lock and removes the PID hint. It is idempotent and
// safe to call from defer paths.
func (l *StateLock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	err := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	closeErr := l.file.Close()
	l.file = nil
	_ = os.Remove(l.pidPath)
	if err != nil {
		return fmt.Errorf("unlock %s: %w", l.path, err)
	}
	return closeErr
}
