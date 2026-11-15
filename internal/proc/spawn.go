package proc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// DefaultWaitDelay bounds post-exit pipe waits when Options.WaitDelay is unset.
const DefaultWaitDelay = 3 * time.Second

// Options configures one spawned agent process.
type Options struct {
	// Path is the executable to run.
	Path string
	// Args are the command-line arguments, excluding Path itself.
	Args []string
	// Env is the child environment; nil inherits the parent's.
	Env []string
	// Dir is the working directory; empty means the parent's.
	Dir string
	// Stderr receives the child's diagnostics. It is a separate channel from
	// the JSONL protocol on stdout and must never be parsed as frames.
	Stderr io.Writer
	// Logger receives spawn and wait lifecycle events; nil discards them.
	Logger *slog.Logger
	// Session identifies the supervised session this child belongs to.
	Session string
	// Persist is invoked with the child's process-group id and session id
	// immediately after the process exists and before Start returns. This is
	// the crash-recovery seam: persistence lives elsewhere, but it must
	// happen while the child is guaranteed alive so a supervisor crash right
	// after Start can still find and kill the group.
	Persist func(pgid int, session string)
	// WaitDelay bounds how long waits continue after the child exits while
	// descendants still hold pipe ends. Zero means DefaultWaitDelay.
	WaitDelay time.Duration
}

// Child is a spawned agent process in its own process group, together with
// the goroutines that own its wait and its stdout frame pump.
type Child struct {
	pgid    int
	session string
	cmd     *exec.Cmd
	stdout  *os.File // pipe read end; closed only by waitOwner
	frames  chan Frame

	waitDelay time.Duration
	waitDone  chan struct{}
	waitErr   error
	state     *os.ProcessState
	pumpDone  chan struct{}
	closeOnce sync.Once
}

// Start launches the child described by opts in a new process group
// (Setpgid), with stdout wired to an owned os.Pipe carrying JSONL frames and
// stderr routed to opts.Stderr as diagnostics.
func Start(opts Options) (*Child, error) {
	waitDelay := opts.WaitDelay
	if waitDelay <= 0 {
		waitDelay = DefaultWaitDelay
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	stdoutRead, stdoutWrite, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("spawn %s: stdout pipe: %w", opts.Path, err)
	}
	// The write end must be a plain *os.File so the child gets the descriptor
	// directly: exec then spawns no copy goroutine for stdout and Wait cannot
	// race the frame pump the way Cmd.StdoutPipe does.
	//nolint:gosec // G204: this package IS the spawn primitive; Path and Args are caller-supplied by design.
	cmd := exec.Command(opts.Path, opts.Args...)
	cmd.Env = opts.Env
	cmd.Dir = opts.Dir
	cmd.Stdout = stdoutWrite
	cmd.Stderr = opts.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = waitDelay
	if err := cmd.Start(); err != nil {
		_ = stdoutRead.Close()
		_ = stdoutWrite.Close()
		return nil, fmt.Errorf("spawn %s: %w", opts.Path, err)
	}
	// Setpgid makes the child its own group leader, so pgid == pid.
	pgid := cmd.Process.Pid
	if opts.Persist != nil {
		opts.Persist(pgid, opts.Session)
	}
	// Drop the parent's write end: EOF must depend only on the child (and
	// whatever it passes the descriptor on to).
	_ = stdoutWrite.Close()

	child := &Child{
		pgid:      pgid,
		session:   opts.Session,
		cmd:       cmd,
		stdout:    stdoutRead,
		frames:    make(chan Frame, 1024),
		waitDelay: waitDelay,
		waitDone:  make(chan struct{}),
		pumpDone:  make(chan struct{}),
	}
	logger.Info("child started", "pgid", pgid, "session", opts.Session, "pid", cmd.Process.Pid)
	go child.waitOwner(logger)
	go child.pump(logger)
	return child, nil
}

// Pgid returns the child's process-group id.
func (c *Child) Pgid() int { return c.pgid }

// Session returns the supervised session id the child was spawned for.
func (c *Child) Session() string { return c.session }

// Frames returns the channel of JSONL frames read from the child's stdout.
// The channel closes when the stream ends (EOF or bounded teardown).
func (c *Child) Frames() <-chan Frame { return c.frames }

// Wait blocks until the child is reaped and returns its wait error. It may be
// called from any number of goroutines; exactly one internal goroutine owns
// the underlying Cmd.Wait.
func (c *Child) Wait() error {
	<-c.waitDone
	return c.waitErr
}

// State returns the child's process state after Wait; nil before that.
func (c *Child) State() *os.ProcessState { return c.state }

// Terminate runs the SIGTERM → grace → SIGKILL ladder against the child's
// process group, then waits for the child to be reaped. A non-zero exit from
// the deliberate kill is expected and returned as nil; only ladder or wait
// failures are errors.
func (c *Child) Terminate(ctx context.Context, grace time.Duration) error {
	if grace <= 0 {
		grace = TerminateGrace
	}
	if err := TerminateGroup(ctx, c.pgid, grace); err != nil {
		return err
	}
	var exitErr *exec.ExitError
	if err := c.Wait(); err != nil && !errors.As(err, &exitErr) {
		return err
	}
	return nil
}

// waitOwner is the single goroutine allowed to call Cmd.Wait and the only
// closer of the stdout read end. It keeps the read end open until the frame
// pump has drained (so a final frame is never lost), bounded by waitDelay for
// descendants that inherit the pipe and never let go.
func (c *Child) waitOwner(logger *slog.Logger) {
	waitErr := c.cmd.Wait()
	// ErrWaitDelay with an otherwise-clean exit means a descendant held an
	// exec-managed pipe past the bound; that is a successful run.
	if errors.Is(waitErr, exec.ErrWaitDelay) {
		waitErr = nil
	}
	c.waitErr = waitErr
	c.state = c.cmd.ProcessState
	close(c.waitDone)

	select {
	case <-c.pumpDone:
	case <-time.After(c.waitDelay):
		c.closeStdout()
		<-c.pumpDone
	}
	if c.state != nil {
		logger.Info("child reaped",
			"pgid", c.pgid, "session", c.session,
			"exit_err", waitErr, "exit_code", c.state.ExitCode())
	} else {
		logger.Info("child reaped",
			"pgid", c.pgid, "session", c.session,
			"exit_err", waitErr)
	}
}

// closeStdout closes the stdout read end exactly once.
func (c *Child) closeStdout() {
	c.closeOnce.Do(func() { _ = c.stdout.Close() })
}

// pump scans the stdout pipe into the frames channel. It exits at EOF, when
// the read end is closed by waitOwner after the bound, or if a line exceeds
// MaxFrameBytes.
func (c *Child) pump(logger *slog.Logger) {
	defer close(c.pumpDone)
	defer close(c.frames)
	err := ScanFrames(c.stdout, func(frame Frame) error {
		c.frames <- frame
		return nil
	})
	if err != nil && !errors.Is(err, os.ErrClosed) {
		logger.Warn("stdout frame scan ended", "pgid", c.pgid, "session", c.session, "err", err)
	}
}
