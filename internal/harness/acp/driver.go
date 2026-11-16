// Package acp implements lambe's second harness.Driver over the Agent
// Client Protocol (ACP): JSON-RPC 2.0 over the agent's stdio, using the
// coder/acp-go-sdk client side. ACP is lambe's gateway to the forty-plus
// agents that speak it.
//
// Protocol-to-harness mappings, each deliberate:
//
//   - Completion: ACP signals that an agent has no remaining automatic work
//     only via the session/prompt response's stop reason — the protocol's
//     stop event. The driver maps it to [harness.AgentSettledEvent] (and a
//     settled Outcome), with stopReason "cancelled" mapping to the aborted
//     flag. session/update notifications — message chunks, tool-call
//     updates, plans — are progress and never settle a run, so a streaming
//     agent cannot fool the supervisor into an early completion. Likewise
//     one completed tool call or message chunk is a milestone, not
//     completion.
//   - Interruption: [Driver.Abort] sends the session/cancel notification;
//     an agent that honors it answers the prompt with stopReason
//     "cancelled", which surfaces as a settled, aborted run.
//   - Permissions: lambe is unattended, so session/request_permission is
//     always auto-denied (the reject option when offered, else cancelled)
//     and surfaced as a [harness.DiagnosticEvent]; the agent sees the denial
//     and continues or fails on its own.
//   - Resume: advertised session/resume avoids replaying long histories;
//     session/load is the compatible fallback. Unsupported backends return
//     [ErrResumeNotSupported] rather than silently starting a new thread.
//   - Silence: ACP agents can go quiet without dying, so Run arms the
//     internal/proc watchdog on inbound-traffic silence. A silent agent is
//     killed via the group kill ladder and reported as a retryable
//     [harness.OutcomeWatchdogKilled].
package acp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/andrew-bierman/lambe/internal/harness"
	"github.com/andrew-bierman/lambe/internal/proc"
)

// ErrResumeNotSupported reports a backend that does not advertise session/load.
var ErrResumeNotSupported = errors.New("acp: backend does not support loading saved sessions")

// ErrSessionNotFound identifies a backend that explicitly has no saved conversation for the requested ID.
var ErrSessionNotFound = errors.New("acp: saved backend session does not exist")

// Driver tuning defaults.
const (
	// DefaultCallTimeout bounds one correlated ACP request.
	DefaultCallTimeout = 30 * time.Second
	// DefaultStopFlush bounds the wait for a stop response that raced the
	// process exit before Run concludes the process exited unsettled.
	DefaultStopFlush = 250 * time.Millisecond
)

// Options configures one ACP driver.
type Options struct {
	// SessionID loads a saved backend conversation when nonempty.
	SessionID string
	// Argv launches the agent in ACP mode, e.g. ["opencode", "acp"];
	// typically obtained from Lookup. Mutually exclusive with Agent.
	Argv []string
	// Agent is a harness-level agent name resolved through Lookup;
	// a convenience over Argv for the known agents.
	Agent string
	// Dir is the workspace directory the agent runs in and the ACP
	// session cwd; empty means the parent's directory.
	Dir string
	// Env is the child environment; nil inherits the parent's.
	Env []string
	// Stderr receives the agent's diagnostics; never parsed as protocol.
	Stderr io.Writer
	// Logger receives driver lifecycle events; nil discards them.
	Logger *slog.Logger
	// Persist is the proc crash-recovery seam: it runs with the pgid and
	// session label after the process exists and before Start returns.
	Persist func(pgid int, session string) error
	// WaitDelay bounds post-exit pipe waits; zero means the proc default.
	WaitDelay time.Duration
	// SilenceAfter arms the silence watchdog for every run: an agent that
	// sends nothing — no updates, no responses — for this long is killed
	// via the ladder. Zero or less disables it unless MaxRun is set.
	SilenceAfter time.Duration
	// MaxRun bounds one supervised turn; zero disables the deadline.
	MaxRun time.Duration
	// TerminateGrace is the wait between kill-ladder rungs; zero means
	// the proc default.
	TerminateGrace time.Duration
	// CallTimeout bounds one correlated ACP request; zero means
	// DefaultCallTimeout.
	CallTimeout time.Duration
	// Stats, when set, records per-turn usage into the attempt record.
	Stats harness.TurnStatsRecorder
}

// Driver drives one ACP agent process. It implements harness.Driver. Runs
// are serialized: one Run at a time. Events must be consumed concurrently
// with Run.
type Driver struct {
	opts Options
	log  *slog.Logger

	mu        sync.Mutex // guards client, sessionID, streaming, result, stats, pendingMessage
	client    *client
	sessionID string
	streaming bool
	result    chan harness.Outcome
	stats     harness.SessionStats
	// pendingMessage buffers streamed assistant-message chunks until the
	// next boundary (tool call or stop) completes the message.
	pendingMessage string
	replaying      bool

	runMu sync.Mutex // serializes Run

	wdMu     sync.Mutex // guards watchdog
	watchdog *proc.Watchdog

	events    chan harness.Event
	closeOnce sync.Once
	closed    chan struct{}
}

// Interface conformance check.
var _ harness.Driver = (*Driver)(nil)

// New returns a driver for the given options. The process is not spawned
// until Start or Run.
func New(opts Options) (*Driver, error) {
	argv := opts.Argv
	if len(argv) == 0 && opts.Agent != "" {
		resolved, err := Lookup(opts.Agent)
		if err != nil {
			return nil, err
		}
		argv = resolved
	}
	if len(argv) == 0 {
		return nil, fmt.Errorf("acp: no agent argv and no agent name")
	}
	opts.Argv = argv
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	if opts.CallTimeout <= 0 {
		opts.CallTimeout = DefaultCallTimeout
	}
	return &Driver{
		opts:   opts,
		log:    logger,
		events: make(chan harness.Event, 1024),
		closed: make(chan struct{}),
	}, nil
}

// Capabilities reports the driver's features. ACP exposes no reliable
// provider-limit signal, so LimitSignal is false and limit classification
// stays with the supervisor.
func (d *Driver) Capabilities() harness.Capabilities {
	return harness.Capabilities{LimitSignal: false}
}

// Start spawns the agent in ACP mode, performs the initialize handshake,
// and creates or loads a session rooted at the configured directory. The event
// loop is attached before Start returns, so no inbound update can be missed.
func (d *Driver) Start(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-d.closed:
		return fmt.Errorf("acp: driver is closed")
	default:
	}

	d.mu.Lock()
	if d.client != nil {
		d.mu.Unlock()
		return nil
	}
	d.mu.Unlock()

	dir := d.opts.Dir
	if dir == "" {
		abs, err := filepath.Abs(".")
		if err != nil {
			return fmt.Errorf("acp: resolve cwd: %w", err)
		}
		dir = abs
	}
	client, err := startClient(clientOptions{
		Argv:       d.opts.Argv,
		Dir:        d.opts.Dir,
		Env:        d.opts.Env,
		Stderr:     d.opts.Stderr,
		Logger:     d.log,
		Session:    d.sessionID,
		Persist:    d.opts.Persist,
		WaitDelay:  d.opts.WaitDelay,
		OnActivity: d.activitySeen,
	}, d)
	if err != nil {
		return err
	}
	d.mu.Lock()
	d.client = client
	d.mu.Unlock()

	ictx, cancel := context.WithTimeout(ctx, d.opts.CallTimeout)
	defer cancel()
	if err := client.initialize(ictx); err != nil {
		_ = client.Close(context.WithoutCancel(ctx), d.opts.TerminateGrace)
		d.mu.Lock()
		if d.client == client {
			d.client = nil
		}
		d.mu.Unlock()
		return err
	}
	sessionID := d.opts.SessionID
	d.mu.Lock()
	d.replaying = sessionID != ""
	d.mu.Unlock()
	if sessionID == "" {
		sessionID, err = client.newSession(ictx, dir)
	} else {
		err = client.load(ictx, sessionID, dir)
	}
	if err != nil {
		_ = client.Close(context.WithoutCancel(ctx), d.opts.TerminateGrace)
		d.mu.Lock()
		d.replaying = false
		if d.client == client {
			d.client = nil
		}
		d.mu.Unlock()
		return err
	}
	d.mu.Lock()
	d.sessionID = sessionID
	d.replaying = false
	// session/load replays history before returning; it is not output from the next turn.
	d.pendingMessage = ""
	d.stats = harness.SessionStats{SessionID: sessionID}
	d.mu.Unlock()
	d.log.Info("acp session ready", "session_id", sessionID, "cwd", dir)
	return nil
}

// Events returns the typed event stream; it closes when Close completes.
func (d *Driver) Events() <-chan harness.Event { return d.events }

// Prompt submits a prompt as a new ACP turn. The stop response is consumed
// by the driver's turn machinery and surfaced on the event stream; callers
// that need the turn's outcome use Run.
func (d *Driver) Prompt(ctx context.Context, text string) error {
	client, sessionID, err := d.ensureStarted(ctx)
	if err != nil {
		return err
	}
	return d.submitTurn(ctx, client, sessionID, text)
}

// Steer sends additional user input mid-turn. ACP has no dedicated steering
// method, so this submits a second session/prompt on the same session; agents
// typically queue it against the in-flight turn. It never races a streaming
// agent into protocol errors because prompts are per-session, not per-run.
func (d *Driver) Steer(ctx context.Context, text string) error {
	client, sessionID, err := d.ensureStarted(ctx)
	if err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, d.opts.CallTimeout)
	defer cancel()
	_, err = client.prompt(cctx, sessionID, text)
	return err
}

// Abort asks the agent to abort its current operation via the session/cancel
// notification. Agents that honor it answer the in-flight prompt with
// stopReason "cancelled".
func (d *Driver) Abort(ctx context.Context) error {
	d.mu.Lock()
	client := d.client
	sessionID := d.sessionID
	d.mu.Unlock()
	if client == nil {
		return fmt.Errorf("acp: abort before start")
	}
	cctx, cancel := context.WithTimeout(ctx, d.opts.CallTimeout)
	defer cancel()
	return client.cancel(cctx, sessionID)
}

// Resume replaces an idle agent process and loads the named backend conversation.
func (d *Driver) Resume(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return fmt.Errorf("acp: resume requires a session id")
	}
	if !d.runMu.TryLock() {
		return fmt.Errorf("acp: cannot resume during an active turn")
	}
	defer d.runMu.Unlock()
	d.mu.Lock()
	old := d.client
	d.client = nil
	d.opts.SessionID = sessionID
	d.mu.Unlock()
	if old != nil {
		if err := old.Close(ctx, d.opts.TerminateGrace); err != nil {
			return err
		}
	}
	return d.Start(ctx)
}

// SessionStats returns the session's current usage snapshot, accumulated
// from usage updates and per-turn usage on stop responses. ACP usage is
// unstable and optional; zeros mean the agent did not report any.
func (d *Driver) SessionStats(context.Context) (harness.SessionStats, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.stats, nil
}

// Run supervises one full turn: it submits the prompt and blocks until the
// agent answers with its stop reason (the only completion signal), the
// watchdog kills a silent agent, or the process exits. Update streams,
// tool-call milestones, and message chunks never end a run early.
func (d *Driver) Run(ctx context.Context, promptText string) (harness.Outcome, error) {
	d.runMu.Lock()
	defer d.runMu.Unlock()

	client, sessionID, err := d.ensureStarted(ctx)
	if err != nil {
		out := harness.Outcome{Kind: harness.OutcomeFailed, Err: err}
		return out, err
	}

	d.mu.Lock()
	d.result = make(chan harness.Outcome, 1)
	d.mu.Unlock()
	d.armWatchdog()
	defer d.stopWatchdog()

	d.emit(harness.AgentStartedEvent{})
	d.setStreaming(true)
	err = d.submitTurn(ctx, client, sessionID, promptText)
	if err != nil {
		// The watchdog may have killed the process while the prompt was
		// in flight; its outcome wins over the transport error. (Kept as
		// a comment, unencoded: submitTurn submits on a background
		// goroutine and never fails in flight, so this race cannot be
		// reproduced through the real protocol; the branch guards the
		// transport-error path only.)
		if out, found := d.takeResult(0); found {
			d.finishRun(false)
			return out, out.RunError()
		}
		d.finishRun(false)
		out := harness.Outcome{Kind: harness.OutcomeFailed, Err: err}
		return out, err
	}

	select {
	case out := <-d.result:
		d.finishRun(false)
		return out, out.RunError()
	case <-ctx.Done():
		go d.kill(client, "run context cancelled") //nolint:gosec // G118: deliberate; the kill ladder must outlive the cancelled run context.
		out := harness.Outcome{
			Kind:            harness.OutcomeFailed,
			Retryable:       true,
			AttemptConsumed: true,
			Err:             ctx.Err(),
		}
		return out, ctx.Err()
	case <-client.Done():
		if out, found := d.takeResult(DefaultStopFlush); found {
			d.finishRun(false)
			return out, out.RunError()
		}
		waitErr := client.WaitErr()
		clean := waitErr == nil
		var outErr error
		if !clean {
			outErr = fmt.Errorf("agent exited: %w", waitErr)
		}
		out := harness.Outcome{
			Kind:            harness.OutcomeProcessExited,
			ExitClean:       clean,
			Retryable:       true,
			AttemptConsumed: true,
			Err:             outErr,
		}
		return out, out.RunError()
	}
}

// Close shuts the agent down — stdin close, a grace period, then the group
// kill ladder — and closes the event stream. It is idempotent.
func (d *Driver) Close(ctx context.Context) error {
	var err error
	d.closeOnce.Do(func() {
		close(d.closed)
		err = d.shutdown(ctx)
		close(d.events)
	})
	return err
}

// --- inbound traffic (acp.Client callbacks land here) ---

// handleUpdate maps one session/update notification onto typed events.
// Updates are progress only; none of them settle a run.
func (d *Driver) handleUpdate(n acpsdk.SessionNotification) {
	d.mu.Lock()
	replaying := d.replaying
	d.mu.Unlock()
	if replaying {
		return
	}
	u := n.Update
	switch {
	case u.AgentMessageChunk != nil:
		text := ""
		if u.AgentMessageChunk.Content.Text != nil {
			text = u.AgentMessageChunk.Content.Text.Text
		}
		d.mu.Lock()
		d.pendingMessage += text
		d.mu.Unlock()
	case u.ToolCall != nil:
		d.flushMessage()
		d.observeToolCall(u.ToolCall.ToolCallId, u.ToolCall.Title, u.ToolCall.Status)
	case u.ToolCallUpdate != nil:
		d.flushMessage()
		title := ""
		if u.ToolCallUpdate.Title != nil {
			title = *u.ToolCallUpdate.Title
		}
		status := acpsdk.ToolCallStatusPending
		if u.ToolCallUpdate.Status != nil {
			status = *u.ToolCallUpdate.Status
		}
		d.observeToolCall(u.ToolCallUpdate.ToolCallId, title, status)
	case u.UsageUpdate != nil:
		d.observeUsageUpdate(u.UsageUpdate)
	default:
		d.emit(harness.UnknownEvent{Type: "session_update"})
	}
}

// observeToolCall maps tool-call status onto the started/ended pair. A
// pending or in-progress status starts the execution; completed and failed
// end it, with failed carrying IsError.
func (d *Driver) observeToolCall(id acpsdk.ToolCallId, title string, status acpsdk.ToolCallStatus) {
	name := title
	if name == "" {
		name = string(id)
	}
	switch status {
	case acpsdk.ToolCallStatusPending, acpsdk.ToolCallStatusInProgress:
		d.emit(harness.ToolExecutionStartedEvent{ToolName: name})
	case acpsdk.ToolCallStatusCompleted:
		d.emit(harness.ToolExecutionEndedEvent{ToolName: name, IsError: false})
	case acpsdk.ToolCallStatusFailed:
		d.emit(harness.ToolExecutionEndedEvent{ToolName: name, IsError: true})
	default:
		d.emit(harness.UnknownEvent{Type: "tool_call_status:" + string(status)})
	}
}

// handlePermissionDenied surfaces one auto-denied permission request. Lambe
// never grants interactive approvals; the diagnostic explains why the tool
// call did not proceed.
func (d *Driver) handlePermissionDenied(toolTitle string) {
	d.log.Warn("acp permission request auto-denied", "tool", toolTitle)
	d.emit(harness.DiagnosticEvent{Message: "permission auto-denied: " + toolTitle})
}

// --- internals ---

// ensureStarted returns the live client and session id, starting the
// process if needed.
func (d *Driver) ensureStarted(ctx context.Context) (*client, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	d.mu.Lock()
	client := d.client
	sessionID := d.sessionID
	d.mu.Unlock()
	if client != nil {
		return client, sessionID, nil
	}
	if err := d.Start(ctx); err != nil {
		return nil, "", err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.client, d.sessionID, nil
}

// submitTurn sends the session/prompt request on a background context (the
// turn's lifetime is the agent's, not the caller's) and routes the stop
// response into the run machinery.
func (d *Driver) submitTurn(_ context.Context, client *client, sessionID string, text string) error {
	go func() { //nolint:gosec // G118: deliberate; the turn's lifetime is the agent's, not the caller's context's.
		ctx, cancel := context.WithTimeout(context.Background(), d.turnDeadline())
		defer cancel()
		resp, err := client.prompt(ctx, sessionID, text)
		d.onStop(resp, err)
	}()
	return nil
}

// turnDeadline bounds one submitted turn when no MaxRun is configured, so a
// prompt can never wait forever independent of the watchdog.
func (d *Driver) turnDeadline() time.Duration {
	if d.opts.MaxRun > 0 {
		return d.opts.MaxRun
	}
	if d.opts.SilenceAfter > 0 {
		return 10 * d.opts.SilenceAfter
	}
	return 24 * time.Hour
}

// onStop is the completion path: the prompt response carries the protocol's
// stop reason, the only signal that no automatic work remains.
func (d *Driver) onStop(resp acpsdk.PromptResponse, err error) {
	if err != nil {
		if _, found := d.takeResult(0); !found {
			d.setStreaming(false)
			d.emit(harness.AgentEndedEvent{})
			d.deliverResult(harness.Outcome{
				Kind:            harness.OutcomeFailed,
				Err:             fmt.Errorf("acp session/prompt: %w", err),
				Retryable:       true,
				AttemptConsumed: true,
			})
		}
		return
	}
	aborted := resp.StopReason == acpsdk.StopReasonCancelled
	d.flushMessage()
	d.observeUsage(resp.Usage)
	d.setStreaming(false)
	d.emit(harness.AgentEndedEvent{})
	d.emit(harness.AgentSettledEvent{Aborted: aborted})
	out := harness.Outcome{
		Kind:            harness.OutcomeSettled,
		Aborted:         aborted,
		AttemptConsumed: true,
	}
	d.mu.Lock()
	stats := d.stats
	d.mu.Unlock()
	if stats.InputTokens != 0 || stats.OutputTokens != 0 || stats.TotalTokens != 0 {
		snapshot := stats
		out.Stats = &snapshot
		if d.opts.Stats != nil {
			d.opts.Stats.RecordTurnStats(snapshot.SessionID, snapshot)
		}
	}
	d.deliverResult(out)
}

// observeUsage folds an optional per-turn usage report into the cumulative
// snapshot.
func (d *Driver) observeUsage(u *acpsdk.Usage) {
	if u == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stats.InputTokens = int64(u.InputTokens)
	d.stats.OutputTokens = int64(u.OutputTokens)
	d.stats.TotalTokens = int64(u.TotalTokens)
	if u.CachedReadTokens != nil {
		d.stats.CacheReadTokens = int64(*u.CachedReadTokens)
	}
	if u.CachedWriteTokens != nil {
		d.stats.CacheWriteTokens = int64(*u.CachedWriteTokens)
	}
}

// observeUsageUpdate folds an unstable usage_update notification into the
// snapshot, including the context-window estimate.
func (d *Driver) observeUsageUpdate(u *acpsdk.SessionUsageUpdate) {
	d.mu.Lock()
	defer d.mu.Unlock()
	used := int64(u.Used)
	if u.Size > 0 {
		d.stats.ContextTokens = &used
		d.stats.ContextWindow = int64(u.Size)
		pct := float64(u.Used) / float64(u.Size) * 100
		d.stats.ContextPercent = &pct
	}
	if u.Cost != nil && u.Cost.Currency == "USD" {
		d.stats.CostUSD = u.Cost.Amount
	}
}

// flushMessage emits the buffered assistant message, if any. ACP has no
// per-message completion notification, so a streamed message is considered
// complete at the next boundary: a tool call starting, or the turn stopping.
func (d *Driver) flushMessage() {
	d.mu.Lock()
	text := d.pendingMessage
	d.pendingMessage = ""
	d.mu.Unlock()
	if text != "" {
		d.emit(harness.MessageCompletedEvent{Role: "assistant", Text: text})
	}
}

// emit delivers one typed event, aborting when the driver closes.
func (d *Driver) emit(ev harness.Event) {
	harness.Emit(d.events, ev, d.closed)
}

// setStreaming updates the tracked streaming state.
func (d *Driver) setStreaming(v bool) {
	d.mu.Lock()
	d.streaming = v
	d.mu.Unlock()
}

// finishRun resets per-turn streaming bookkeeping.
func (d *Driver) finishRun(streaming bool) {
	d.setStreaming(streaming)
}

// takeResult waits up to window for the active Run's outcome and reports
// whether one arrived.
func (d *Driver) takeResult(window time.Duration) (harness.Outcome, bool) {
	d.mu.Lock()
	ch := d.result
	d.mu.Unlock()
	if ch == nil {
		return harness.Outcome{}, false
	}
	if window <= 0 {
		select {
		case out := <-ch:
			return out, true
		default:
			return harness.Outcome{}, false
		}
	}
	timer := time.NewTimer(window)
	defer timer.Stop()
	select {
	case out := <-ch:
		return out, true
	case <-timer.C:
		return harness.Outcome{}, false
	}
}

// deliverResult hands one outcome to the active Run, if any.
func (d *Driver) deliverResult(out harness.Outcome) {
	d.mu.Lock()
	ch := d.result
	d.mu.Unlock()
	harness.DeliverResult(ch, out)
}

// armWatchdog arms the silence/deadline watchdog for one run. Silence means
// no inbound ACP traffic at all; every byte read from the agent resets the
// silence timer through the activity hook.
func (d *Driver) armWatchdog() {
	d.stopWatchdog()
	silence := d.opts.SilenceAfter
	maxRun := d.opts.MaxRun
	if silence <= 0 {
		silence = maxRun
	}
	if silence <= 0 {
		return
	}
	if maxRun > 0 && maxRun < silence {
		silence = maxRun
	}
	d.wdMu.Lock()
	defer d.wdMu.Unlock()
	d.watchdog = proc.NewWatchdog(maxRun, silence, d.onExpiry)
}

// stopWatchdog disarms the watchdog, if armed.
func (d *Driver) stopWatchdog() {
	d.wdMu.Lock()
	defer d.wdMu.Unlock()
	if d.watchdog != nil {
		d.watchdog.Stop()
		d.watchdog = nil
	}
}

// activitySeen is the liveness hook: every byte read from the agent resets
// the silence timer.
func (d *Driver) activitySeen() {
	d.wdMu.Lock()
	defer d.wdMu.Unlock()
	if d.watchdog != nil {
		d.watchdog.FrameSeen()
	}
}

// onExpiry runs the kill ladder for a silent or over-deadline agent and
// delivers the retryable watchdog-kill outcome before the ladder so a Run
// racing process-exit still reports the kill.
func (d *Driver) onExpiry(reason proc.ExpiryReason) {
	select {
	case <-d.closed:
		return
	default:
	}
	d.stopWatchdog()
	d.mu.Lock()
	client := d.client
	d.mu.Unlock()
	if client == nil {
		return
	}
	d.kill(client, reason.String())
}

// kill delivers the retryable watchdog-kill outcome, then runs the group
// kill ladder. Idempotent per driver once closed.
func (d *Driver) kill(client *client, reason string) {
	d.stopWatchdog()
	d.log.Warn("killing acp agent process group", "reason", reason)
	d.deliverResult(harness.Outcome{
		Kind:            harness.OutcomeWatchdogKilled,
		Retryable:       true,
		AttemptConsumed: true,
		Err:             fmt.Errorf("watchdog kill: %s", reason), //nolint:err113 // the typed outcome carries it; error text is diagnostic only.
	})
	ctx, cancel := context.WithTimeout(context.Background(), proc.TerminateGrace)
	defer cancel()
	if err := client.Terminate(ctx, d.opts.TerminateGrace); err != nil {
		d.log.Warn("kill ladder failed", "err", err)
	}
}

// shutdown tears the process down; it does not close the event stream.
func (d *Driver) shutdown(ctx context.Context) error {
	d.stopWatchdog()
	d.mu.Lock()
	client := d.client
	d.client = nil
	d.result = nil
	d.mu.Unlock()
	if client == nil {
		return nil
	}
	return client.Close(ctx, d.opts.TerminateGrace)
}
