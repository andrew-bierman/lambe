// Package harness defines the backend-agnostic contract the supervisor uses
// to drive coding agents: a start/run lifecycle, a stream of typed events,
// steering and abort, session resume, per-turn usage stats, and a typed
// outcome for every supervised turn.
//
// The contract is deliberately narrow. Drivers translate their backend's
// native protocol into these types; the supervisor never parses backend
// frames or reasons about backend-specific strings.
package harness

import (
	"context"
	"encoding/json"
	"time"
)

// Capabilities advertises optional driver features so the supervisor can
// degrade instead of probing.
type Capabilities struct {
	// LimitSignal reports whether the driver detects provider limit
	// errors (even heuristically) and surfaces them as retryable
	// LimitSuspected outcomes carrying a suggested wake-at time.
	LimitSignal bool
}

// SessionStats is one snapshot of a session's token, cost, and context
// usage, normalized across backends.
type SessionStats struct {
	// SessionID identifies the backend session the stats describe.
	SessionID string
	// InputTokens is the cumulative input token count.
	InputTokens int64
	// OutputTokens is the cumulative output token count.
	OutputTokens int64
	// CacheReadTokens is the cumulative cache-read token count.
	CacheReadTokens int64
	// CacheWriteTokens is the cumulative cache-write token count.
	CacheWriteTokens int64
	// TotalTokens is the cumulative total token count.
	TotalTokens int64
	// CostUSD is the cumulative cost in US dollars.
	CostUSD float64
	// ContextTokens is the current context-window estimate in tokens;
	// nil when the backend cannot estimate it.
	ContextTokens *int64
	// ContextWindow is the model's context window in tokens; zero when
	// unknown.
	ContextWindow int64
	// ContextPercent is the current context-window usage in [0,100];
	// nil when the backend cannot estimate it.
	ContextPercent *float64
}

// TurnStatsRecorder receives per-turn usage stats so the supervisor can
// persist them into the session's attempt record. It is intentionally tiny:
// implementations must not call back into the driver and should not block.
type TurnStatsRecorder interface {
	// RecordTurnStats records the stats observed at the end of one
	// completed turn for the given session.
	RecordTurnStats(sessionID string, stats SessionStats)
}

// Event is one typed agent event. The set is closed: backends map their
// native events onto these types and anything unmapped becomes
// UnknownEvent.
type Event interface{ isEvent() }

// AgentStartedEvent reports that one low-level agent run started.
type AgentStartedEvent struct{}

// AgentEndedEvent reports that one low-level agent run ended. It is NOT
// completion: retries, compaction recovery, or queued steering may still
// follow. Completion is only AgentSettledEvent.
type AgentEndedEvent struct {
	// WillRetry reports whether the backend intends to retry the run.
	WillRetry bool
}

// AgentSettledEvent reports that the agent has no remaining automatic work
// for the session-level run. This is the only completion signal.
type AgentSettledEvent struct {
	// Aborted reports whether the run ended because it was aborted.
	Aborted bool
}

// MessageCompletedEvent reports one authoritative completed message.
type MessageCompletedEvent struct {
	// Role is the message role, e.g. "user" or "assistant".
	Role string
	// Text is the message's text content, best-effort extracted.
	Text string
	// StopReason is the backend stop reason; "error" marks a failed
	// message.
	StopReason string
}

// ToolExecutionStartedEvent reports that a tool execution began.
type ToolExecutionStartedEvent struct {
	// ToolName names the tool being executed.
	ToolName string
}

// ToolExecutionEndedEvent reports that a tool execution finished.
type ToolExecutionEndedEvent struct {
	// ToolName names the tool that was executed.
	ToolName string
	// IsError reports whether the tool reported an error.
	IsError bool
}

// AutoRetryScheduledEvent reports the backend scheduled an automatic retry
// after a failure.
type AutoRetryScheduledEvent struct {
	// Attempt is the upcoming attempt number.
	Attempt int
	// MaxAttempts is the backend's retry ceiling.
	MaxAttempts int
	// Delay is the backoff before the next attempt.
	Delay time.Duration
	// ErrorMessage describes the failure that triggered the retry.
	ErrorMessage string
}

// AutoRetryEndedEvent reports the retry loop finished.
type AutoRetryEndedEvent struct {
	// Success reports whether a retry attempt succeeded.
	Success bool
	// FinalError describes the final failure when Success is false.
	FinalError string
}

// DiagnosticEvent reports a backend-side error that is not a command
// failure, such as an extension handler throwing.
type DiagnosticEvent struct {
	// Message describes the diagnostic.
	Message string
}

// UnknownEvent carries an event the driver does not model.
type UnknownEvent struct {
	// Type is the native event type name.
	Type string
	// Data is the raw native event payload.
	Data json.RawMessage
}

func (AgentStartedEvent) isEvent()         {}
func (AgentEndedEvent) isEvent()           {}
func (AgentSettledEvent) isEvent()         {}
func (MessageCompletedEvent) isEvent()     {}
func (ToolExecutionStartedEvent) isEvent() {}
func (ToolExecutionEndedEvent) isEvent()   {}
func (AutoRetryScheduledEvent) isEvent()   {}
func (AutoRetryEndedEvent) isEvent()       {}
func (DiagnosticEvent) isEvent()           {}
func (UnknownEvent) isEvent()              {}

// OutcomeKind is the typed way one supervised turn ended. Callers branch on
// this, never on error strings.
type OutcomeKind int

const (
	// OutcomeSettled means the agent settled: no automatic work remains.
	OutcomeSettled OutcomeKind = iota
	// OutcomeHandled means the prompt was consumed by an input handler
	// and no agent run will start, so no settle event will follow.
	OutcomeHandled
	// OutcomeLimitSuspected means a provider limit error was heuristically
	// detected. The turn is retryable and does not consume an attempt.
	OutcomeLimitSuspected
	// OutcomeWatchdogKilled means the silence/deadline watchdog exhausted
	// its escalation ladder and killed the process group. Retryable.
	OutcomeWatchdogKilled
	// OutcomeProcessExited means the agent process exited before settling.
	OutcomeProcessExited
	// OutcomeFailed means the driver could not run the turn at all.
	OutcomeFailed
)

// String returns the kind's name for logs and test messages.
func (k OutcomeKind) String() string {
	switch k {
	case OutcomeSettled:
		return "settled"
	case OutcomeHandled:
		return "handled"
	case OutcomeLimitSuspected:
		return "limit_suspected"
	case OutcomeWatchdogKilled:
		return "watchdog_killed"
	case OutcomeProcessExited:
		return "process_exited"
	case OutcomeFailed:
		return "failed"
	default:
		return "unknown"
	}
}

// Outcome is the typed result of one supervised turn.
type Outcome struct {
	// Kind says how the turn ended.
	Kind OutcomeKind
	// Aborted is set for OutcomeSettled: the run settled because it was
	// aborted.
	Aborted bool
	// WakeAt is the suggested earliest retry time for
	// OutcomeLimitSuspected.
	WakeAt time.Time
	// Confidence is the limit classifier's confidence in [0,1]; zero
	// unless Kind is OutcomeLimitSuspected.
	Confidence float64
	// Stats is the per-turn usage snapshot when one was available.
	Stats *SessionStats
	// ExitClean reports a zero process exit for OutcomeProcessExited.
	ExitClean bool
	// Retryable reports whether the supervisor may retry the turn.
	Retryable bool
	// AttemptConsumed reports whether the turn counts against the
	// attempt cap. Limit-suspected turns do not.
	AttemptConsumed bool
	// Err carries the failure detail for OutcomeFailed and non-clean
	// exits.
	Err error
}

// Driver is the supervisor's contract with one agent backend instance.
//
// Lifecycle: Start spawns (or Resume respawns) the agent with event
// listeners already attached; Run supervises exactly one turn at a time and
// returns a typed Outcome; Close tears the process down via an orderly
// shutdown followed by the kill ladder. Events must be consumed
// concurrently with Run or streaming backends can stall on backpressure.
type Driver interface {
	// Capabilities reports the features this driver supports.
	Capabilities() Capabilities
	// Start launches the agent process in its target working directory.
	Start(ctx context.Context) error
	// Events returns the driver's typed event stream. The channel is
	// closed when Close completes.
	Events() <-chan Event
	// Prompt submits a prompt, transparently steering instead when the
	// agent is mid-run, so prompting never races a streaming agent.
	Prompt(ctx context.Context, text string) error
	// Steer queues a steering message for an in-flight run.
	Steer(ctx context.Context, text string) error
	// Abort asks the agent to abort its current operation.
	Abort(ctx context.Context) error
	// Resume restarts the driver's process continuing the session with
	// the given backend session id.
	Resume(ctx context.Context, sessionID string) error
	// SessionStats returns the session's current usage snapshot.
	SessionStats(ctx context.Context) (SessionStats, error)
	// Run supervises one full turn: it submits the prompt and blocks
	// until the turn settles, is limit-suspected, is killed by the
	// watchdog, or the process exits.
	Run(ctx context.Context, prompt string) (Outcome, error)
	// Close shuts the agent process down: orderly stdin close, a grace
	// period, then the group kill ladder.
	Close(ctx context.Context) error
}

// RunError maps an outcome onto Driver.Run's error return: only outright
// failure and an unclean exit are errors; retryable supervision outcomes
// are values, not errors.
func (o Outcome) RunError() error {
	switch o.Kind {
	case OutcomeFailed:
		return o.Err
	case OutcomeProcessExited:
		if !o.ExitClean {
			return o.Err
		}
	case OutcomeSettled, OutcomeHandled, OutcomeLimitSuspected, OutcomeWatchdogKilled:
		// Supervision outcomes: values, not errors.
	}
	return nil
}

// Emit delivers one typed event on the stream unless closed closes first,
// so a driver shutting down never blocks on an unconsumed event stream.
func Emit(ch chan<- Event, ev Event, closed <-chan struct{}) {
	select {
	case ch <- ev:
	case <-closed:
	}
}

// DeliverResult hands one outcome to the active Run's result channel, if
// any; it is a no-op when no run is active (nil channel) and drops the
// outcome when one was already delivered.
func DeliverResult(ch chan<- Outcome, out Outcome) {
	if ch == nil {
		return
	}
	select {
	case ch <- out:
	default:
	}
}
