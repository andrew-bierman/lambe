package pi

import (
	"bufio"
	"bytes"
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

	"github.com/andrew-bierman/lambe/internal/proc"
)

// agentProc is one spawned Pi process in its own process group, with stdin
// and stdout wired to os.Pipe JSONL channels.
//
// This is a deliberate thin adapter rather than a proc.Start call:
// internal/proc owns the child's stdout but exposes no stdin seam, and the
// RPC protocol is bidirectional. Everything reusable from internal/proc —
// the kill ladder, the watchdog, the frame-size bound — is consumed
// read-only; the wait-ownership invariants mirror proc's own design.
type agentProc struct {
	pgid    int
	session string
	cmd     *exec.Cmd
	stdin   *os.File // pipe write end; closed to request orderly shutdown
	stdout  *os.File // pipe read end; closed only by waitOwner

	lines     chan []byte
	waitDelay time.Duration
	waitDone  chan struct{}
	waitErr   error
	state     *os.ProcessState
	pumpDone  chan struct{}
	closeOnce sync.Once
	stdinOnce sync.Once
}

// spawnOptions configures one spawned agent process. The fields mirror
// proc.Options plus the stdin direction.
type spawnOptions struct {
	Path    string
	Args    []string
	Env     []string
	Dir     string
	Stderr  io.Writer
	Logger  *slog.Logger
	Session string
	// Persist is invoked with the process-group id and session id
	// immediately after the process exists and before spawn returns; the
	// crash-recovery seam.
	Persist func(pgid int, session string) error
	// WaitDelay bounds post-exit pipe waits when descendants hold the
	// stdout pipe open. Zero means proc.DefaultWaitDelay.
	WaitDelay time.Duration
	// OnLine, when set, is invoked for every valid stdout line, before
	// the line is delivered; the watchdog liveness hook.
	OnLine func()
}

// spawnAgent launches the described process in a new process group with
// stdin and stdout connected to pipes, mirroring internal/proc's ownership
// rules: one goroutine owns Wait and the stdout read end; the parent drops
// its copy of the stdout write end so EOF depends only on the child.
func spawnAgent(opts spawnOptions) (*agentProc, error) {
	waitDelay := opts.WaitDelay
	if waitDelay <= 0 {
		waitDelay = proc.DefaultWaitDelay
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	stdinRead, stdinWrite, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("spawn %s: stdin pipe: %w", opts.Path, err)
	}
	stdoutRead, stdoutWrite, err := os.Pipe()
	if err != nil {
		_ = stdinRead.Close()
		_ = stdinWrite.Close()
		return nil, fmt.Errorf("spawn %s: stdout pipe: %w", opts.Path, err)
	}
	//nolint:gosec // G204: this adapter IS the spawn primitive for the Pi driver; Path and Args are caller-supplied by design.
	cmd := exec.Command(opts.Path, opts.Args...)
	cmd.Env = opts.Env
	cmd.Dir = opts.Dir
	cmd.Stdin = stdinRead
	cmd.Stdout = stdoutWrite
	cmd.Stderr = opts.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = waitDelay
	if err := cmd.Start(); err != nil {
		_ = stdinRead.Close()
		_ = stdinWrite.Close()
		_ = stdoutRead.Close()
		_ = stdoutWrite.Close()
		return nil, fmt.Errorf("spawn %s: %w", opts.Path, err)
	}
	pgid := cmd.Process.Pid // Setpgid: the child leads its own group.
	if opts.Persist != nil {
		if err := opts.Persist(pgid, opts.Session); err != nil {
			proc.AbortStart(cmd, stdinRead, stdinWrite, stdoutRead, stdoutWrite)
			return nil, fmt.Errorf("%w: %w", proc.ErrBindingUnavailable, err)
		}
	}
	// Drop the parent's copies: stdin EOF depends only on us closing
	// stdinWrite; stdout EOF depends only on the child side.
	_ = stdinRead.Close()
	_ = stdoutWrite.Close()

	p := &agentProc{
		pgid:      pgid,
		session:   opts.Session,
		cmd:       cmd,
		stdin:     stdinWrite,
		stdout:    stdoutRead,
		lines:     make(chan []byte, 1024),
		waitDelay: waitDelay,
		waitDone:  make(chan struct{}),
		pumpDone:  make(chan struct{}),
	}
	logger.Info("agent spawned", "pgid", pgid, "session", opts.Session, "pid", cmd.Process.Pid)
	go p.waitOwner(logger)
	go p.pump(logger, opts.OnLine)
	return p, nil
}

// Pgid returns the process-group id.
func (p *agentProc) Pgid() int { return p.pgid }

// Session returns the supervised session id the process was spawned for.
func (p *agentProc) Session() string { return p.session }

// Lines returns the channel of raw LF-stripped stdout records. The channel
// closes when the stream ends (EOF or bounded teardown).
func (p *agentProc) Lines() <-chan []byte { return p.lines }

// WriteLine writes one record and its terminating LF to the child's stdin.
func (p *agentProc) WriteLine(b []byte) error {
	if _, err := p.stdin.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("write command: %w", err)
	}
	return nil
}

// CloseStdin closes the stdin write end to request an orderly shutdown.
func (p *agentProc) CloseStdin() {
	p.stdinOnce.Do(func() { _ = p.stdin.Close() })
}

// Wait blocks until the process is reaped and returns its wait error.
func (p *agentProc) Wait() error {
	<-p.waitDone
	return p.waitErr
}

// WaitDone returns a channel closed when the process is reaped.
func (p *agentProc) WaitDone() <-chan struct{} { return p.waitDone }

// WaitErr returns the wait error once WaitDone has closed.
func (p *agentProc) WaitErr() error {
	<-p.waitDone
	return p.waitErr
}

// State returns the process state after Wait; nil before that.
func (p *agentProc) State() *os.ProcessState { return p.state }

// Terminate runs the SIGTERM → grace → SIGKILL ladder against the process
// group via internal/proc, then waits for the reap. A deliberate-kill exit
// status is not an error.
func (p *agentProc) Terminate(ctx context.Context, grace time.Duration) error {
	if err := proc.TerminateGroup(ctx, p.pgid, grace); err != nil {
		return err
	}
	var exitErr *exec.ExitError
	if err := p.Wait(); err != nil && !errors.As(err, &exitErr) {
		return err
	}
	return nil
}

// waitOwner is the single goroutine allowed to call Cmd.Wait and the only
// closer of the stdout read end; it keeps the read end open until the pump
// drains, bounded by waitDelay for descendants holding the pipe.
func (p *agentProc) waitOwner(logger *slog.Logger) {
	waitErr := p.cmd.Wait()
	// ErrWaitDelay with an otherwise-clean exit means a descendant held a
	// pipe past the bound; that is a successful run.
	if errors.Is(waitErr, exec.ErrWaitDelay) {
		waitErr = nil
	}
	p.waitErr = waitErr
	p.state = p.cmd.ProcessState
	close(p.waitDone)

	select {
	case <-p.pumpDone:
	case <-time.After(p.waitDelay):
		p.closeStdout()
		<-p.pumpDone
	}
	logger.Info("agent reaped", "pgid", p.pgid, "session", p.session, "exit_err", waitErr)
}

// closeStdout closes the stdout read end exactly once.
func (p *agentProc) closeStdout() {
	p.closeOnce.Do(func() { _ = p.stdout.Close() })
}

// pump scans stdout into the lines channel, splitting strictly on LF —
// never on CR or Unicode separators — with the same size bound as
// internal/proc's frame scanner.
func (p *agentProc) pump(logger *slog.Logger, onLine func()) {
	defer close(p.pumpDone)
	defer close(p.lines)
	scanner := bufio.NewScanner(p.stdout)
	scanner.Split(splitRecords)
	scanner.Buffer(make([]byte, 0, 64*1024), proc.MaxFrameBytes)
	for scanner.Scan() {
		line := bytes.Clone(scanner.Bytes())
		if onLine != nil {
			onLine()
		}
		p.lines <- line
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, os.ErrClosed) {
		logger.Warn("stdout scan ended", "pgid", p.pgid, "session", p.session, "err", err)
	}
}

// splitRecords splits strictly on the LF byte, stripping only the optional
// preceding CR. Unicode line separators never terminate a record.
func splitRecords(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		token = data[:i]
		if len(token) > 0 && token[len(token)-1] == '\r' {
			token = token[:len(token)-1]
		}
		return i + 1, token, nil
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}
