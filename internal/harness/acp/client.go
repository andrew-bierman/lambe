package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/andrew-bierman/lambe/internal/proc"
)

// clientOptions configures one ACP client and the agent process it owns.
type clientOptions struct {
	// Argv launches the agent in ACP mode, e.g. ["opencode", "acp"].
	Argv []string
	// Dir is the agent's working directory and the ACP session cwd.
	Dir string
	// Env is the child environment; nil inherits the parent's.
	Env []string
	// Stderr receives the child's diagnostics; never protocol data.
	Stderr io.Writer
	// Logger receives lifecycle events; nil discards them.
	Logger *slog.Logger
	// Session labels the supervised session for the Persist seam.
	Session string
	// Persist runs with the pgid and session id between process creation
	// and spawn return; the crash-recovery seam.
	Persist func(pgid int, session string) error
	// WaitDelay bounds post-exit pipe waits; zero means the proc default.
	WaitDelay time.Duration
	// OnActivity, when set, fires for every byte read from the agent; the
	// watchdog liveness hook.
	OnActivity func()
}

// inbound is the driver-side sink for agent-initiated traffic: session
// updates and auto-denied permission requests.
type inbound interface {
	// handleUpdate delivers one session/update notification.
	handleUpdate(acpsdk.SessionNotification)
	// handlePermissionDenied reports one auto-denied permission request.
	handlePermissionDenied(toolTitle string)
}

// client is the ACP client: it owns the spawned agent process, the JSON-RPC
// connection over its stdio (via the acp-go-sdk client side), and the
// client-side callbacks every inbound agent request lands in.
type client struct {
	proc          *agentProc
	conn          *acpsdk.ClientSideConnection
	cbs           *clientCallbacks
	loadSession   bool
	resumeSession bool
}

// agentProc is one spawned agent process in its own process group with stdin
// and stdout wired to os.Pipe ends. As with the Pi driver, this is a local
// adapter because internal/proc owns stdout but exposes no stdin seam, and
// the ACP protocol is bidirectional. The kill ladder and watchdog come from
// internal/proc unchanged.
type agentProc struct {
	pgid    int
	session string
	cmd     *exec.Cmd
	stdin   *os.File // write end; closed to request orderly shutdown
	stdout  *os.File // read end; closed only after the process is reaped

	waitDone  chan struct{}
	waitErr   error
	closeOnce sync.Once
	stdinOnce sync.Once
	readEOF   chan struct{}
	eofOnce   sync.Once
}

// spawnAgent launches the described process in a new process group (Setpgid)
// with pipes on both stdio directions. The parent drops its copies of the
// child-side pipe ends so EOF depends only on the child for stdout and only
// on the parent for stdin.
func spawnAgent(opts clientOptions) (*agentProc, error) {
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	stdinRead, stdinWrite, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("spawn %v: stdin pipe: %w", opts.Argv, err)
	}
	stdoutRead, stdoutWrite, err := os.Pipe()
	if err != nil {
		_ = stdinRead.Close()
		_ = stdinWrite.Close()
		return nil, fmt.Errorf("spawn %v: stdout pipe: %w", opts.Argv, err)
	}
	//nolint:gosec // G204: this adapter IS the spawn primitive for the ACP driver; Argv is caller-supplied by design.
	cmd := exec.Command(opts.Argv[0], opts.Argv[1:]...)
	cmd.Env = opts.Env
	cmd.Dir = opts.Dir
	cmd.Stdin = stdinRead
	cmd.Stdout = stdoutWrite
	cmd.Stderr = opts.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = proc.DefaultWaitDelay
	if opts.WaitDelay > 0 {
		cmd.WaitDelay = opts.WaitDelay
	}
	if err := cmd.Start(); err != nil {
		_ = stdinRead.Close()
		_ = stdinWrite.Close()
		_ = stdoutRead.Close()
		_ = stdoutWrite.Close()
		return nil, fmt.Errorf("spawn %v: %w", opts.Argv, err)
	}
	pgid := cmd.Process.Pid // Setpgid: the child leads its own group.
	if opts.Persist != nil {
		if err := opts.Persist(pgid, opts.Session); err != nil {
			proc.AbortStart(cmd, stdinRead, stdinWrite, stdoutRead, stdoutWrite)
			return nil, fmt.Errorf("%w: %w", proc.ErrBindingUnavailable, err)
		}
	}
	_ = stdinRead.Close()
	_ = stdoutWrite.Close()

	p := &agentProc{
		pgid:     pgid,
		session:  opts.Session,
		cmd:      cmd,
		stdin:    stdinWrite,
		stdout:   stdoutRead,
		waitDone: make(chan struct{}),
		readEOF:  make(chan struct{}),
	}
	logger.Info("acp agent spawned", "pgid", pgid, "session", opts.Session, "pid", cmd.Process.Pid)
	go p.waitOwner()
	return p, nil
}

// waitOwner is the single goroutine allowed to call Cmd.Wait. After the reap
// it waits one WaitDelay for the SDK read loop to observe EOF, then closes
// the stdout read end so the read loop can never block forever on a
// descendant holding the pipe.
func (p *agentProc) waitOwner() {
	waitErr := p.cmd.Wait()
	if errors.Is(waitErr, exec.ErrWaitDelay) {
		waitErr = nil
	}
	p.waitErr = waitErr
	close(p.waitDone)
	// Give the SDK read loop a moment to see EOF, but stop waiting as soon as
	// it does: the old unconditional WaitDelay added three seconds to every
	// teardown even when the pipe was already drained.
	timer := time.NewTimer(proc.DefaultWaitDelay)
	defer timer.Stop()
	select {
	case <-p.readEOF:
	case <-timer.C:
	}
	p.closeOnce.Do(func() { _ = p.stdout.Close() })
}

// Pgid returns the process-group id.
func (p *agentProc) Pgid() int { return p.pgid }

// WaitDone returns a channel closed when the process is reaped.
func (p *agentProc) WaitDone() <-chan struct{} { return p.waitDone }

// WaitErr returns the wait error once WaitDone has closed.
func (p *agentProc) WaitErr() error {
	<-p.waitDone
	return p.waitErr
}

// CloseStdin closes the stdin write end to request an orderly shutdown.
func (p *agentProc) CloseStdin() {
	p.stdinOnce.Do(func() { _ = p.stdin.Close() })
}

// Terminate runs the proc kill ladder against the process group and waits
// for the reap. A deliberate-kill exit status is not an error.
func (p *agentProc) Terminate(ctx context.Context, grace time.Duration) error {
	if err := proc.TerminateGroup(ctx, p.pgid, grace); err != nil {
		return err
	}
	var exitErr *exec.ExitError
	if err := p.WaitErr(); err != nil && !errors.As(err, &exitErr) {
		return err
	}
	return nil
}

// eofReader signals when the child's stdout reaches EOF, so waitOwner can stop
// waiting as soon as the SDK has seen the end of the stream.
type eofReader struct {
	r      io.Reader
	onEOF  func()
	closed bool
}

func (e *eofReader) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if err == io.EOF && !e.closed {
		e.closed = true
		e.onEOF()
	}
	return n, err
}

// activityReader wraps the child's stdout read end and fires onActivity for
// every byte read, so any inbound protocol traffic — notifications, requests,
// responses — resets the driver's silence watchdog.
type activityReader struct {
	r          io.Reader
	onActivity func()
}

// Read implements io.Reader.
func (a *activityReader) Read(p []byte) (int, error) {
	n, err := a.r.Read(p)
	if n > 0 && a.onActivity != nil {
		a.onActivity()
	}
	return n, err
}

// gatedReader blocks reads until ready is closed.
type gatedReader struct {
	r     io.Reader
	ready <-chan struct{}
}

// Read implements io.Reader.
func (g *gatedReader) Read(p []byte) (int, error) {
	<-g.ready
	return g.r.Read(p)
}

// clientCallbacks implements the acp-go-sdk Client interface: every request
// the agent sends to its client lands here.
//
// Posture: lambe is unattended, so permission requests are auto-denied —
// the deny/reject option when the agent offered one, otherwise an explicit
// cancellation — and surfaced to the driver as an event. File and terminal
// capabilities are not advertised at initialize, so those methods answer
// method-not-found; they are implemented only because the interface demands
// them.
type clientCallbacks struct {
	sink inbound
}

// ReadTextFile is unsupported: lambe advertises no fs.readTextFile capability.
func (c *clientCallbacks) ReadTextFile(context.Context, acpsdk.ReadTextFileRequest) (acpsdk.ReadTextFileResponse, error) {
	return acpsdk.ReadTextFileResponse{}, acpsdk.NewMethodNotFound("fs/readTextFile")
}

// WriteTextFile is unsupported: lambe advertises no fs.writeTextFile capability.
func (c *clientCallbacks) WriteTextFile(context.Context, acpsdk.WriteTextFileRequest) (acpsdk.WriteTextFileResponse, error) {
	return acpsdk.WriteTextFileResponse{}, acpsdk.NewMethodNotFound("fs/writeTextFile")
}

// CreateTerminal is unsupported: lambe advertises no terminal capability.
func (c *clientCallbacks) CreateTerminal(context.Context, acpsdk.CreateTerminalRequest) (acpsdk.CreateTerminalResponse, error) {
	return acpsdk.CreateTerminalResponse{}, acpsdk.NewMethodNotFound("terminal/create")
}

// KillTerminal is unsupported: lambe advertises no terminal capability.
func (c *clientCallbacks) KillTerminal(context.Context, acpsdk.KillTerminalRequest) (acpsdk.KillTerminalResponse, error) {
	return acpsdk.KillTerminalResponse{}, acpsdk.NewMethodNotFound("terminal/kill")
}

// TerminalOutput is unsupported: lambe advertises no terminal capability.
func (c *clientCallbacks) TerminalOutput(context.Context, acpsdk.TerminalOutputRequest) (acpsdk.TerminalOutputResponse, error) {
	return acpsdk.TerminalOutputResponse{}, acpsdk.NewMethodNotFound("terminal/output")
}

// ReleaseTerminal is unsupported: lambe advertises no terminal capability.
func (c *clientCallbacks) ReleaseTerminal(context.Context, acpsdk.ReleaseTerminalRequest) (acpsdk.ReleaseTerminalResponse, error) {
	return acpsdk.ReleaseTerminalResponse{}, acpsdk.NewMethodNotFound("terminal/release")
}

// WaitForTerminalExit is unsupported: lambe advertises no terminal capability.
func (c *clientCallbacks) WaitForTerminalExit(context.Context, acpsdk.WaitForTerminalExitRequest) (acpsdk.WaitForTerminalExitResponse, error) {
	return acpsdk.WaitForTerminalExitResponse{}, acpsdk.NewMethodNotFound("terminal/waitForExit")
}

// RequestPermission auto-denies: lambe never grants interactive approvals.
// It selects the agent's reject-always option when offered, then reject-once,
// and otherwise answers cancelled. The denial is surfaced to the driver so
// the supervisor can see why the agent's tool call will not proceed.
func (c *clientCallbacks) RequestPermission(_ context.Context, params acpsdk.RequestPermissionRequest) (acpsdk.RequestPermissionResponse, error) {
	outcome := acpsdk.RequestPermissionOutcome{Cancelled: &acpsdk.RequestPermissionOutcomeCancelled{}}
	// Prefer reject-always wherever it appears: an agent that offers both
	// reject kinds should not fall back to reject-once just because it listed
	// that option first.
	for _, kind := range []acpsdk.PermissionOptionKind{
		acpsdk.PermissionOptionKindRejectAlways,
		acpsdk.PermissionOptionKindRejectOnce,
	} {
		for _, opt := range params.Options {
			if opt.Kind == kind {
				outcome = acpsdk.RequestPermissionOutcome{
					Selected: &acpsdk.RequestPermissionOutcomeSelected{OptionId: opt.OptionId},
				}
				break
			}
		}
		if outcome.Selected != nil {
			break
		}
	}
	toolTitle := ""
	if params.ToolCall.Title != nil {
		toolTitle = *params.ToolCall.Title
	}
	c.sink.handlePermissionDenied(toolTitle)
	return acpsdk.RequestPermissionResponse{Outcome: outcome}, nil
}

// SessionUpdate forwards one session/update notification to the driver.
func (c *clientCallbacks) SessionUpdate(_ context.Context, params acpsdk.SessionNotification) error {
	c.sink.handleUpdate(params)
	return nil
}

// startClient spawns the agent process, wires the SDK client-side connection
// over its stdio, and returns the client. Inbound traffic flows to sink
// immediately; listeners attached right after startClient returns cannot miss
// a fast completion before the first prompt.
func startClient(opts clientOptions, sink inbound) (*client, error) {
	if len(opts.Argv) == 0 {
		return nil, fmt.Errorf("acp: empty argv")
	}
	p, err := spawnAgent(opts)
	if err != nil {
		return nil, err
	}
	cbs := &clientCallbacks{sink: sink}
	// The SDK starts its read loop inside the constructor, and SetLogger is
	// an unsynchronized write; hold the first read until the logger is set so
	// a fast-talking (or garbage-emitting) agent cannot race it.
	ready := make(chan struct{})
	reader := &eofReader{r: p.stdout, onEOF: func() { p.eofOnce.Do(func() { close(p.readEOF) }) }}
	var out io.Reader = &gatedReader{r: reader, ready: ready}
	if opts.OnActivity != nil {
		out = &activityReader{r: out, onActivity: opts.OnActivity}
	}
	conn := acpsdk.NewClientSideConnection(cbs, p.stdin, out)
	if opts.Logger != nil {
		conn.SetLogger(opts.Logger)
	}
	close(ready)
	return &client{proc: p, conn: conn, cbs: cbs}, nil
}

// initialize performs the ACP initialize handshake, advertising lambe's
// client capabilities: no fs, no terminal, no interactive auth.
func (c *client) initialize(ctx context.Context) error {
	response, err := c.conn.Initialize(ctx, acpsdk.InitializeRequest{
		ClientCapabilities: acpsdk.ClientCapabilities{},
		ClientInfo:         &acpsdk.Implementation{Name: "lambe", Version: "0"},
		ProtocolVersion:    1,
	})
	if err != nil {
		return fmt.Errorf("acp initialize: %w", err)
	}
	c.loadSession = response.AgentCapabilities.LoadSession
	c.resumeSession = response.AgentCapabilities.SessionCapabilities.Resume != nil
	return nil
}

// newSession creates a fresh ACP session rooted at cwd and returns its id.
func (c *client) newSession(ctx context.Context, cwd string) (string, error) {
	resp, err := c.conn.NewSession(ctx, acpsdk.NewSessionRequest{
		Cwd:        cwd,
		McpServers: []acpsdk.McpServer{},
	})
	if err != nil {
		return "", fmt.Errorf("acp session/new: %w", err)
	}
	if resp.SessionId == "" {
		return "", fmt.Errorf("acp session/new returned an empty session id")
	}
	return string(resp.SessionId), nil
}

// prompt submits one turn. It blocks until the agent answers with the turn's
// stop reason, which in ACP is the completion signal.
func (c *client) prompt(ctx context.Context, sessionID string, text string) (acpsdk.PromptResponse, error) {
	return c.conn.Prompt(ctx, acpsdk.PromptRequest{
		SessionId: acpsdk.SessionId(sessionID),
		Prompt:    []acpsdk.ContentBlock{acpsdk.TextBlock(text)},
	})
}

// cancel sends the session/cancel notification: the ACP interrupt.
func (c *client) cancel(ctx context.Context, sessionID string) error {
	return c.conn.Cancel(ctx, acpsdk.CancelNotification{SessionId: acpsdk.SessionId(sessionID)})
}

// Done returns a channel closed when the agent disconnects (process exit or
// EOF on stdout).
func (c *client) Done() <-chan struct{} { return c.conn.Done() }

// WaitErr blocks until the process is reaped and returns its wait error.
func (c *client) WaitErr() error { return c.proc.WaitErr() }

// CloseStdin closes the child's stdin to request an orderly shutdown.
func (c *client) CloseStdin() { c.proc.CloseStdin() }

// Terminate runs the group kill ladder directly.
func (c *client) Terminate(ctx context.Context, grace time.Duration) error {
	return c.proc.Terminate(ctx, grace)
}

// Close shuts the agent down: stdin close for an orderly exit, a grace
// period, then the proc kill ladder. A deliberate-kill exit status is not an
// error.
func (c *client) Close(ctx context.Context, grace time.Duration) error {
	if grace <= 0 {
		grace = proc.TerminateGrace
	}
	c.proc.CloseStdin()
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-c.proc.WaitDone():
	case <-timer.C:
	case <-ctx.Done():
	}
	return c.proc.Terminate(ctx, grace)
}

func (c *client) load(ctx context.Context, sessionID, cwd string) error {
	if !c.loadSession && !c.resumeSession {
		return ErrResumeNotSupported
	}
	var err error
	if c.resumeSession {
		_, err = c.conn.ResumeSession(ctx, acpsdk.ResumeSessionRequest{SessionId: acpsdk.SessionId(sessionID), Cwd: cwd, McpServers: []acpsdk.McpServer{}})
		var unsupported *acpsdk.RequestError
		if err != nil && errors.As(err, &unsupported) && unsupported.Code == -32601 && !c.loadSession {
			return ErrResumeNotSupported
		}
		if err == nil || !c.loadSession || !errors.As(err, &unsupported) || unsupported.Code != -32601 {
			return sessionRestoreError(err, sessionID)
		}
	}
	_, err = c.conn.LoadSession(ctx, acpsdk.LoadSessionRequest{SessionId: acpsdk.SessionId(sessionID), Cwd: cwd, McpServers: []acpsdk.McpServer{}})
	return sessionRestoreError(err, sessionID)
}

func sessionRestoreError(err error, sessionID string) error {
	if err != nil {
		var rpcErr *acpsdk.RequestError
		if errors.As(err, &rpcErr) {
			var data struct {
				SessionID string `json:"sessionId"`
				Details   string `json:"details"`
			}
			if encoded, merr := json.Marshal(rpcErr.Data); merr == nil {
				_ = json.Unmarshal(encoded, &data)
			}
			// shortcut: codex-acp 2.6 reports empty rollouts as InternalError; remove that case when it uses resource-not-found.
			if (rpcErr.Code == -32002 && data.SessionID == sessionID) || (rpcErr.Code == -32603 && data.Details == "no rollout found for thread id "+sessionID) {
				return fmt.Errorf("%w: %w", ErrSessionNotFound, err)
			}
		}
		return fmt.Errorf("acp restore session %s: %w", sessionID, err)
	}
	return nil
}
