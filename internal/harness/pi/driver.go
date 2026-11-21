package pi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/andrew-bierman/lambe/internal/harness"
	"github.com/andrew-bierman/lambe/internal/proc"
)

// Driver tuning defaults. The silence ladder mirrors the supervision
// contract: steer-probe, then kill ladder.
const (
	// DefaultCallTimeout bounds one correlated command.
	DefaultCallTimeout = 30 * time.Second
	// DefaultSteerProbeMessage is the liveness probe sent on the first
	// silence expiry.
	DefaultSteerProbeMessage = "Report your current status in one line."
	// DefaultProbeWindow bounds the wait for any response to the steer
	// probe before the kill ladder runs.
	DefaultProbeWindow = 30 * time.Second
	// DefaultLimitHorizon is how far ahead a LimitSuspected outcome
	// suggests waking.
	DefaultLimitHorizon = 15 * time.Minute
	// DefaultStatsTimeout bounds the per-turn stats fetch.
	DefaultStatsTimeout = 5 * time.Second
)

// Options configures one Pi driver.
type Options struct {
	// Path is the pi executable; empty means "pi".
	Path string
	// Args are extra command-line arguments after the mode and session
	// arguments.
	Args []string
	// Dir is the workspace directory Pi runs in; it controls Pi's config
	// discovery. Empty means the parent's directory.
	Dir string
	// Env is the child environment; nil inherits the parent's.
	Env []string
	// SessionID, when set, opens-or-creates the session with that id;
	// resuming is spawning again with the same id.
	SessionID string
	// Stderr receives Pi's diagnostics; never parsed as protocol.
	Stderr io.Writer
	// Logger receives driver lifecycle events; nil discards them.
	Logger *slog.Logger
	// Persist is the proc crash-recovery seam: it runs with the pgid and
	// session id after the process exists and before Start returns.
	Persist func(pgid int, session string) error
	// WaitDelay bounds post-exit pipe waits; zero means the proc default.
	WaitDelay time.Duration
	// SilenceAfter arms the silence watchdog for every run; zero disables
	// it unless MaxRun is set.
	SilenceAfter time.Duration
	// ProbeWindow bounds the escalation wait after the steer probe;
	// zero means DefaultProbeWindow.
	ProbeWindow time.Duration
	// MaxRun bounds one supervised turn; zero disables the deadline.
	MaxRun time.Duration
	// TerminateGrace is the wait between kill-ladder rungs; zero means
	// the proc default.
	TerminateGrace time.Duration
	// SteerProbeMessage is the liveness probe text; empty means
	// DefaultSteerProbeMessage.
	SteerProbeMessage string
	// LimitHorizon is the suggested wake-at lead for LimitSuspected
	// outcomes; zero means DefaultLimitHorizon.
	LimitHorizon time.Duration
	// CallTimeout bounds one correlated command; zero means
	// DefaultCallTimeout.
	CallTimeout time.Duration
	// Stats, when set, records per-turn usage into the attempt record.
	Stats harness.TurnStatsRecorder
}

// Driver drives one Pi agent process over its RPC protocol. It implements
// harness.Driver. Runs are serialized: one Run at a time. Events must be
// consumed concurrently with Run.
type Driver struct {
	opts Options
	log  *slog.Logger

	classifier *LimitClassifier
	sessionID  string // adopted from get_state when Options.SessionID is empty

	mu        sync.Mutex // guards client, streaming, result
	client    *Client
	streaming bool
	result    chan harness.Outcome

	runMu sync.Mutex // serializes Run

	wdMu       sync.Mutex // guards watchdog
	watchdog   *proc.Watchdog
	escalation atomic.Int32        // 0 armed, 1 probe sent, 2 killing
	runStart   time.Time           // guarded by wdMu; start of the armed run
	tools      map[string]struct{} // guarded by wdMu; tool executions in flight this run
	killed     atomic.Bool

	events    chan harness.Event
	loopDone  chan struct{}
	closeOnce sync.Once
	closed    chan struct{}
}

// Interface conformance check.
var _ harness.Driver = (*Driver)(nil)

// New returns a driver for the given options. The process is not spawned
// until Start, Resume, or Run.
func New(opts Options) *Driver {
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	if opts.SteerProbeMessage == "" {
		opts.SteerProbeMessage = DefaultSteerProbeMessage
	}
	if opts.LimitHorizon <= 0 {
		opts.LimitHorizon = DefaultLimitHorizon
	}
	if opts.CallTimeout <= 0 {
		opts.CallTimeout = DefaultCallTimeout
	}
	if opts.ProbeWindow <= 0 {
		opts.ProbeWindow = DefaultProbeWindow
	}
	return &Driver{
		opts:       opts,
		log:        logger,
		classifier: NewLimitClassifier(opts.LimitHorizon),
		events:     make(chan harness.Event, 1024),
		closed:     make(chan struct{}),
	}
}

// Capabilities reports the driver's features: limit detection is
// heuristic but present.
func (d *Driver) Capabilities() harness.Capabilities {
	return harness.Capabilities{LimitSignal: true}
}

// Start spawns Pi in RPC mode in the target directory: the session id is
// persisted through the proc seam before spawn returns, the event loop is
// attached before any prompt can be sent, and the streaming state is
// synced from get_state so prompt-vs-steer never races a resumed run.
func (d *Driver) Start(ctx context.Context) error {
	d.mu.Lock()
	if d.client != nil {
		d.mu.Unlock()
		return nil
	}
	d.mu.Unlock()

	path := d.opts.Path
	if path == "" {
		path = "pi"
	}
	args := []string{"--mode", "rpc"}
	if d.opts.SessionID != "" {
		args = append(args, "--session-id", d.opts.SessionID)
	}
	args = append(args, d.opts.Args...)

	client, err := StartClient(ClientOptions{
		Path:      path,
		Args:      args,
		Dir:       d.opts.Dir,
		Env:       d.opts.Env,
		Stderr:    d.opts.Stderr,
		Logger:    d.log,
		Session:   d.opts.SessionID,
		Persist:   d.opts.Persist,
		WaitDelay: d.opts.WaitDelay,
		OnRecord:  d.recordSeen,
	})
	if err != nil {
		return err
	}
	d.mu.Lock()
	d.client = client
	d.loopDone = make(chan struct{})
	d.mu.Unlock()

	go d.loop(client)

	if err := d.refreshState(ctx, client); err != nil {
		_ = client.Close(context.WithoutCancel(ctx), d.opts.TerminateGrace)
		d.mu.Lock()
		if d.client == client {
			d.client = nil
		}
		d.mu.Unlock()
		<-d.loopDone
		return err
	}
	return nil
}

// Events returns the typed event stream; it closes when Close completes.
func (d *Driver) Events() <-chan harness.Event { return d.events }

// Prompt submits a prompt, steering instead when the agent is mid-run so
// prompting never races a streaming agent.
func (d *Driver) Prompt(ctx context.Context, text string) error {
	client, err := d.ensureStarted(ctx)
	if err != nil {
		return err
	}
	_, err = d.submitPrompt(ctx, client, text)
	return err
}

// Steer queues a steering message for an in-flight run.
func (d *Driver) Steer(ctx context.Context, text string) error {
	client, err := d.ensureStarted(ctx)
	if err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, d.opts.CallTimeout)
	defer cancel()
	_, err = client.Call(cctx, Command{Type: CmdSteer, Message: text})
	return err
}

// Abort asks the agent to abort its current operation.
func (d *Driver) Abort(ctx context.Context) error {
	client, err := d.ensureStarted(ctx)
	if err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, d.opts.CallTimeout)
	defer cancel()
	_, err = client.Call(cctx, Command{Type: CmdAbort})
	return err
}

// Resume restarts the driver's process, continuing the session with the
// given backend session id: spawning with the same id opens-or-creates
// that session for the working directory.
func (d *Driver) Resume(ctx context.Context, sessionID string) error {
	select {
	case <-d.closed:
		return fmt.Errorf("resume on closed driver")
	default:
	}
	if err := d.shutdown(context.WithoutCancel(ctx)); err != nil {
		return err
	}
	d.opts.SessionID = sessionID
	return d.Start(ctx)
}

// SessionStats returns the session's current usage snapshot.
func (d *Driver) SessionStats(ctx context.Context) (harness.SessionStats, error) {
	client, err := d.ensureStarted(ctx)
	if err != nil {
		return harness.SessionStats{}, err
	}
	cctx, cancel := context.WithTimeout(ctx, d.opts.CallTimeout)
	defer cancel()
	rec, err := client.Call(cctx, Command{Type: CmdGetSessionStat})
	if err != nil {
		return harness.SessionStats{}, err
	}
	return d.decodeStats(rec)
}

// Run supervises one full turn: it submits the prompt (steering if the
// agent is mid-run) and blocks until the turn settles, a provider limit is
// suspected, the watchdog kills the process group, or the process exits.
// Completion is reported only on agent_settled, never on agent_end.
func (d *Driver) Run(ctx context.Context, promptText string) (harness.Outcome, error) {
	d.runMu.Lock()
	defer d.runMu.Unlock()

	client, err := d.ensureStarted(ctx)
	if err != nil {
		return harness.Outcome{Kind: harness.OutcomeFailed, Err: err}, err
	}

	d.mu.Lock()
	d.result = make(chan harness.Outcome, 1)
	d.mu.Unlock()
	d.classifier.Reset()
	d.escalation.Store(0)
	d.killed.Store(false)
	d.armWatchdog()
	defer d.stopWatchdog()

	ackRec, err := d.submitPrompt(ctx, client, promptText)
	if err != nil {
		select {
		case out := <-d.result:
			return out, out.Err
		default:
		}
		out := harness.Outcome{Kind: harness.OutcomeFailed, Err: err}
		return out, err
	}
	var ack PromptAck
	if len(ackRec.Data) > 0 {
		if jerr := json.Unmarshal(ackRec.Data, &ack); jerr != nil {
			d.log.Warn("decode prompt ack", "err", jerr)
		}
	}
	if ack.Disposition == DispositionHandled {
		// A handler consumed the prompt; no run started and no settle
		// event will follow.
		return harness.Outcome{Kind: harness.OutcomeHandled}, nil
	}

	select {
	case out := <-d.result:
		return out, out.RunError()
	case <-ctx.Done():
		go d.kill(client, "run context cancelled") //nolint:gosec // G118: deliberate; ctx is the thing being cancelled.
		out := harness.Outcome{
			Kind:            harness.OutcomeFailed,
			Retryable:       true,
			AttemptConsumed: true,
			Err:             ctx.Err(),
		}
		return out, ctx.Err()
	case <-client.Done():
		if out, found := d.takeResult(250 * time.Millisecond); found {
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

// Close shuts the agent down — orderly stdin close, a grace period, then
// the group kill ladder — and closes the event stream. It is idempotent.
func (d *Driver) Close(ctx context.Context) error {
	var err error
	d.closeOnce.Do(func() {
		close(d.closed)
		err = d.shutdown(ctx)
		close(d.events)
	})
	return err
}

// --- internals ---

// ensureStarted returns the live client, starting the process if needed.
func (d *Driver) ensureStarted(ctx context.Context) (*Client, error) {
	d.mu.Lock()
	client := d.client
	d.mu.Unlock()
	if client != nil {
		return client, nil
	}
	if err := d.Start(ctx); err != nil {
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.client, nil
}

// submitPrompt sends prompt or steer depending on the tracked streaming
// state, so a prompt during streaming never hits the protocol error.
func (d *Driver) submitPrompt(ctx context.Context, client *Client, text string) (Record, error) {
	d.mu.Lock()
	streaming := d.streaming
	d.mu.Unlock()
	cctx, cancel := context.WithTimeout(ctx, d.opts.CallTimeout)
	defer cancel()
	if streaming {
		d.log.Info("agent is streaming; steering instead of prompting")
		return client.Call(cctx, Command{Type: CmdSteer, Message: text})
	}
	return client.Call(cctx, Command{Type: CmdPrompt, Message: text})
}

// refreshState syncs the tracked streaming state and session id from
// get_state.
func (d *Driver) refreshState(ctx context.Context, client *Client) error {
	cctx, cancel := context.WithTimeout(ctx, d.opts.CallTimeout)
	defer cancel()
	rec, err := client.Call(cctx, Command{Type: CmdGetState})
	if err != nil {
		return fmt.Errorf("get_state: %w", err)
	}
	var state StateResponse
	if uerr := json.Unmarshal(rec.Data, &state); uerr != nil {
		return fmt.Errorf("decode get_state: %w", uerr)
	}
	d.mu.Lock()
	d.streaming = state.IsStreaming
	if d.sessionID == "" {
		d.sessionID = state.SessionID
	}
	d.mu.Unlock()
	d.log.Info("agent state synced",
		"is_streaming", state.IsStreaming, "session_id", state.SessionID, "session_file", state.SessionFile)
	return nil
}

// loop consumes client records until the stream ends, converting each to a
// typed event and applying driver state updates.
func (d *Driver) loop(client *Client) {
	d.mu.Lock()
	done := d.loopDone
	d.mu.Unlock()
	defer close(done)
	for rec := range client.Events() {
		if d.handleRecord(client, rec) {
			return
		}
	}
}

// handleRecord processes one record; it returns true only when the driver
// is closed and the loop must stop.
func (d *Driver) handleRecord(client *Client, rec Record) bool {
	select {
	case <-d.closed:
		return true
	default:
	}
	switch rec.Type {
	case EventAgentStart:
		d.setStreaming(true)
		d.emit(harness.AgentStartedEvent{})
	case EventAgentEnd:
		d.setStreaming(false)
		d.emit(harness.AgentEndedEvent{WillRetry: rec.WillRetry})
	case EventAgentSettled:
		d.setStreaming(false)
		d.emit(harness.AgentSettledEvent{Aborted: rec.Aborted})
		d.settle(client, rec.Aborted)
	case EventMessageEnd:
		if rec.Message == nil {
			d.emit(harness.UnknownEvent{Type: rec.Type})
			return false
		}
		ev := harness.MessageCompletedEvent{
			Role:       rec.Message.Role,
			Text:       rec.Message.Text(),
			StopReason: rec.Message.StopReason,
		}
		d.emit(ev)
		if ev.StopReason == "error" {
			d.observeError(client, "message error: "+ev.Text)
		} else {
			d.classifier.Progress()
		}
	case EventToolExecStart:
		d.toolInFlight(rec.ToolCallID, true)
		d.emit(harness.ToolExecutionStartedEvent{ToolName: rec.ToolName})
	case EventToolExecEnd:
		d.toolInFlight(rec.ToolCallID, false)
		d.emit(harness.ToolExecutionEndedEvent{ToolName: rec.ToolName, IsError: rec.IsError})
		if rec.IsError {
			d.observeError(client, "tool error: "+rec.ToolName)
		} else {
			d.classifier.Progress()
		}
	case EventAutoRetrySt:
		d.emit(harness.AutoRetryScheduledEvent{
			Attempt:      rec.Attempt,
			MaxAttempts:  rec.MaxAttempts,
			Delay:        time.Duration(rec.DelayMs) * time.Millisecond,
			ErrorMessage: rec.ErrorMessage,
		})
		d.observeError(client, rec.ErrorMessage)
	case EventAutoRetryEnd:
		retrySuccess := rec.IsSuccess() != nil && *rec.IsSuccess()
		d.emit(harness.AutoRetryEndedEvent{Success: retrySuccess, FinalError: rec.FinalError})
		if rec.FinalError != "" {
			d.observeError(client, rec.FinalError)
		} else {
			d.classifier.Progress()
		}
	case EventExtensionErr:
		d.emit(harness.DiagnosticEvent{Message: rec.Error})
		d.observeError(client, rec.Error)
	default:
		d.emit(harness.UnknownEvent{Type: rec.Type})
	}
	return false
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

// settle completes a run: fetch per-turn stats, record them, deliver the
// settled outcome.
func (d *Driver) settle(client *Client, aborted bool) {
	out := harness.Outcome{
		Kind:            harness.OutcomeSettled,
		Aborted:         aborted,
		AttemptConsumed: true,
	}
	cctx, cancel := context.WithTimeout(context.Background(), DefaultStatsTimeout)
	defer cancel()
	if rec, err := client.Call(cctx, Command{Type: CmdGetSessionStat}); err == nil {
		if stats, serr := d.decodeStats(rec); serr == nil {
			out.Stats = &stats
			if d.opts.Stats != nil {
				d.opts.Stats.RecordTurnStats(d.sessionFor(stats), stats)
			}
		} else {
			d.log.Warn("decode session stats", "err", serr)
		}
	} else {
		d.log.Warn("fetch session stats", "err", err)
	}
	d.deliverResult(out)
}

// decodeStats decodes a get_session_stats response into the harness type.
func (d *Driver) decodeStats(rec Record) (harness.SessionStats, error) {
	var s StatsResponse
	if err := json.Unmarshal(rec.Data, &s); err != nil {
		return harness.SessionStats{}, fmt.Errorf("decode get_session_stats: %w", err)
	}
	stats := harness.SessionStats{
		SessionID:        s.SessionID,
		InputTokens:      s.Tokens.Input,
		OutputTokens:     s.Tokens.Output,
		CacheReadTokens:  s.Tokens.CacheRead,
		CacheWriteTokens: s.Tokens.CacheWrite,
		TotalTokens:      s.Tokens.Total,
		CostUSD:          s.Cost,
	}
	if s.ContextUsage != nil {
		stats.ContextWindow = s.ContextUsage.ContextWindow
		if s.ContextUsage.Tokens != nil {
			stats.ContextTokens = s.ContextUsage.Tokens
		}
		if s.ContextUsage.Percent != nil {
			stats.ContextPercent = s.ContextUsage.Percent
		}
	}
	return stats, nil
}

// sessionFor picks the session id stats should be recorded under.
func (d *Driver) sessionFor(stats harness.SessionStats) string {
	if stats.SessionID != "" {
		return stats.SessionID
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.sessionID
}

// observeError feeds the limit classifier and, on suspicion, delivers the
// retryable LimitSuspected outcome (no attempt consumed) and kills the
// process group.
func (d *Driver) observeError(client *Client, text string) {
	if text == "" {
		return
	}
	if !d.classifier.ObserveError(text) {
		return
	}
	confidence, evidence := d.classifier.Suspicion()
	d.log.Warn("provider limit suspected",
		"confidence", confidence, "wake_horizon", d.classifier.WakeHorizon(), "evidence", evidence)
	d.deliverResult(harness.Outcome{
		Kind:            harness.OutcomeLimitSuspected,
		WakeAt:          time.Now().Add(d.classifier.WakeHorizon()),
		Confidence:      confidence,
		Retryable:       true,
		AttemptConsumed: false,
	})
	go d.kill(client, "limit suspected")
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

// armWatchdog arms the silence/deadline watchdog for one run. When
// SilenceAfter is unset but MaxRun is set, the deadline alone arms it.
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
	d.escalation.Store(0)
	d.runStart = time.Now()
	d.tools = map[string]struct{}{}
	d.watchdog = proc.NewWatchdog(maxRun, silence, d.onExpiry)
}

// remainingRun returns what is left of MaxRun for the current run, so a
// re-armed watchdog keeps the original deadline. Caller holds wdMu.
func (d *Driver) remainingRun() time.Duration {
	if d.opts.MaxRun <= 0 {
		return 0
	}
	return max(time.Millisecond, d.opts.MaxRun-time.Since(d.runStart))
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

// recordSeen is the liveness hook: every valid stdout record resets the
// silence timer.
func (d *Driver) recordSeen() {
	d.wdMu.Lock()
	defer d.wdMu.Unlock()
	if d.watchdog != nil {
		d.watchdog.FrameSeen()
	}
}

// toolInFlight records a tool execution starting or ending. A tool that is
// still running keeps a quiet agent alive: see onExpiry.
func (d *Driver) toolInFlight(id string, running bool) {
	d.wdMu.Lock()
	defer d.wdMu.Unlock()
	if d.tools == nil {
		return
	}
	if running {
		d.tools[id] = struct{}{}
	} else {
		delete(d.tools, id)
	}
}

// onExpiry runs the escalation ladder. The first silence expiry sends a
// steer probe and re-arms for one probe window; a second expiry, or a
// deadline expiry, aborts and runs the kill ladder, delivering a
// retryable watchdog-kill outcome. Silence while a tool execution is in
// flight is not a stall: a long build or test run is quiet by nature, so the
// silence timer re-arms instead and only MaxRun bounds the tool.
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
	if reason == proc.ReasonSilence && d.opts.SilenceAfter > 0 {
		d.wdMu.Lock()
		busy := len(d.tools) > 0
		if busy {
			d.escalation.Store(0)
			d.watchdog = proc.NewWatchdog(d.remainingRun(), d.opts.SilenceAfter, d.onExpiry)
		}
		d.wdMu.Unlock()
		if busy {
			return
		}
	}
	if reason == proc.ReasonDeadline || d.escalation.Add(1) > 1 {
		d.kill(client, reason.String())
		return
	}
	d.log.Warn("silence watchdog fired; sending steer probe", "silence_after", d.opts.SilenceAfter)
	if err := client.Notify(Command{Type: CmdSteer, Message: d.opts.SteerProbeMessage}); err != nil {
		d.log.Warn("steer probe failed", "err", err)
	}
	d.wdMu.Lock()
	d.watchdog = proc.NewWatchdog(d.remainingRun(), d.opts.ProbeWindow, d.onExpiry)
	d.wdMu.Unlock()
}

// kill aborts the agent best-effort, delivers the retryable watchdog-kill
// outcome, then runs the group kill ladder. The outcome is delivered
// before the ladder so a Run racing process-exit still reports the kill.
// Idempotent per run.
func (d *Driver) kill(client *Client, reason string) {
	if !d.killed.CompareAndSwap(false, true) {
		return
	}
	d.stopWatchdog()
	d.log.Warn("killing agent process group", "reason", reason)
	ctx, cancel := context.WithTimeout(context.Background(), DefaultCallTimeout)
	defer cancel()
	_ = client.Notify(Command{Type: CmdAbort})
	d.deliverResult(harness.Outcome{
		Kind:            harness.OutcomeWatchdogKilled,
		Retryable:       true,
		AttemptConsumed: true,
		Err:             fmt.Errorf("watchdog kill: %s", reason), //nolint:err113 // the typed outcome carries it; error text is diagnostic only.
	})
	if err := client.Terminate(ctx, d.opts.TerminateGrace); err != nil {
		d.log.Warn("kill ladder failed", "err", err)
	}
}

// shutdown tears the process down and waits for the event loop; it does
// not close the event stream, so Resume can restart.
func (d *Driver) shutdown(ctx context.Context) error {
	d.stopWatchdog()
	d.mu.Lock()
	client := d.client
	d.client = nil
	d.result = nil
	done := d.loopDone
	d.mu.Unlock()
	if client == nil {
		return nil
	}
	err := client.Close(ctx, d.opts.TerminateGrace)
	if done != nil {
		<-done
	}
	return err
}
