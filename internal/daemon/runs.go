package daemon

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/andrew-bierman/lambe/internal/mcp"
	"github.com/andrew-bierman/lambe/internal/proc"
	"github.com/andrew-bierman/lambe/internal/surface/rex"
)

// maxRunTail bounds the per-run output tail the registry retains for the
// Control adapter; the MCP server caps whatever it returns regardless.
const maxRunTail = 4096

// Run is one supervised run's live state. It is the daemon-side record the
// Control and StateSource adapters read; run execution itself belongs to the
// symphony loop and its wired runner. The run's identifier is the issue id:
// one run per issue at a time.
type Run struct {
	// IssueID is the tracker issue the run works on; it doubles as the
	// run's handle on the wire surface.
	IssueID string
	// Session is the harness session identifier once one exists.
	Session string
	// Agent names the supervised agent for surfaces, e.g. "issues".
	Agent string
	// Prompt is the run's initial prompt.
	Prompt string
	// Workspace is the run's working directory.
	Workspace string
	// Depth is the supervision depth the run executes at.
	Depth int
	// State is the run's typed lifecycle state.
	State mcp.RunState
	// Attempt is the current attempt number, starting at 1.
	Attempt int
	// LastEventAt is when the run last produced an observed event; zero
	// when none.
	LastEventAt time.Time
	// WakeAt is when a deferred run wakes; zero unless deferred.
	WakeAt time.Time
	// Outcome describes how a finished run ended.
	Outcome string
	// PGID is the run's live process group; zero when none is recorded.
	PGID int
	// Tail is the trailing slice of the run's recent output.
	Tail string
}

// RunRegistry is the daemon's live supervised-run state: registration,
// lifecycle transitions, and the feed to the Rex surface. It is safe for
// concurrent use; the mcp Control adapter, the httpstate StateSource
// adapter, and the tick loop all read and write it.
type RunRegistry struct {
	mu      sync.RWMutex
	runs    map[string]*Run
	order   []string
	surface *rex.Adapter
	log     *slog.Logger
}

// NewRunRegistry returns an empty registry. surface, when non-nil, receives
// run lifecycle events; log receives surface errors.
func NewRunRegistry(surface *rex.Adapter, log *slog.Logger) *RunRegistry {
	if log == nil {
		log = slog.Default()
	}
	return &RunRegistry{runs: map[string]*Run{}, surface: surface, log: log}
}

// SetSurface attaches (or replaces) the Rex surface after assembly; events
// from transitions after this call feed it.
func (r *RunRegistry) SetSurface(surface *rex.Adapter, log *slog.Logger) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.surface = surface
	if log != nil {
		r.log = log
	}
}

// Register adds a new pending run for the issue and returns its record. The
// handle is the issue id: one run per issue at a time.
func (r *RunRegistry) Register(issueID, agent, prompt, workspace string, depth int) Run {
	now := time.Now()
	run := &Run{
		IssueID:     issueID,
		Agent:       agent,
		Prompt:      prompt,
		Workspace:   workspace,
		Depth:       depth,
		State:       mcp.StatePending,
		Attempt:     1,
		LastEventAt: now,
	}
	r.mu.Lock()
	if _, exists := r.runs[issueID]; !exists {
		r.order = append(r.order, issueID)
	}
	r.runs[issueID] = run
	r.mu.Unlock()
	return *run
}

// Lookup returns a copy of one run's state; ok is false for unknown runs.
func (r *RunRegistry) Lookup(handle string) (Run, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	run, ok := r.runs[handle]
	if !ok {
		return Run{}, false
	}
	return *run, true
}

// List returns every run in registration order.
func (r *RunRegistry) List() []Run {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Run, 0, len(r.order))
	for _, id := range r.order {
		if run, ok := r.runs[id]; ok {
			out = append(out, *run)
		}
	}
	return out
}

// SetState moves a run to the given state, records the outcome text, and
// stamps the event time. Unknown handles are ignored. State changes feed the
// Rex surface.
func (r *RunRegistry) SetState(handle string, state mcp.RunState, outcome string) {
	r.mu.Lock()
	run, ok := r.runs[handle]
	if !ok {
		r.mu.Unlock()
		return
	}
	prev := run.State
	run.State = state
	run.Outcome = outcome
	run.LastEventAt = time.Now()
	if state != mcp.StateDeferred {
		run.WakeAt = time.Time{}
	}
	agent := run.Agent
	r.mu.Unlock()

	if state != prev {
		r.emit(handle, agent, state)
	}
}

// MarkRunning records that a run's agent attempt is in flight.
func (r *RunRegistry) MarkRunning(handle string) { r.SetState(handle, mcp.StateRunning, "") }

// MarkSettled records a normally finished run.
func (r *RunRegistry) MarkSettled(handle, outcome string) {
	r.SetState(handle, mcp.StateSettled, outcome)
}

// MarkFailed records a run that exhausted its attempts or errored out.
func (r *RunRegistry) MarkFailed(handle, outcome string) {
	r.SetState(handle, mcp.StateFailed, outcome)
}

// MarkCanceled records a caller-requested cancel.
func (r *RunRegistry) MarkCanceled(handle string) {
	r.SetState(handle, mcp.StateCanceled, "canceled by caller")
}

// MarkDeferred parks a run until its wake time.
func (r *RunRegistry) MarkDeferred(handle string, wakeAt time.Time) {
	r.mu.Lock()
	run, ok := r.runs[handle]
	if !ok {
		r.mu.Unlock()
		return
	}
	prev := run.State
	run.State = mcp.StateDeferred
	run.WakeAt = wakeAt
	run.LastEventAt = time.Now()
	agent := run.Agent
	r.mu.Unlock()

	if prev != mcp.StateDeferred {
		r.emit(handle, agent, mcp.StateDeferred)
	}
}

// SetSession records the harness session identifier and stamps the event time.
func (r *RunRegistry) SetSession(handle, session string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if run, ok := r.runs[handle]; ok {
		run.Session = session
		run.LastEventAt = time.Now()
	}
}

// SetAttempt records the current attempt number.
func (r *RunRegistry) SetAttempt(handle string, attempt int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if run, ok := r.runs[handle]; ok {
		run.Attempt = attempt
	}
}

// SetPGID records the run's live process group so cancel and shutdown can
// reach it.
func (r *RunRegistry) SetPGID(handle string, pgid int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if run, ok := r.runs[handle]; ok {
		run.PGID = pgid
	}
}

// Touch stamps the run's last-event time.
func (r *RunRegistry) Touch(handle string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if run, ok := r.runs[handle]; ok {
		run.LastEventAt = time.Now()
	}
}

// AppendTail appends output to the run's bounded tail.
func (r *RunRegistry) AppendTail(handle string, text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	run, ok := r.runs[handle]
	if !ok {
		return
	}
	run.Tail += text
	if len(run.Tail) > maxRunTail {
		run.Tail = run.Tail[len(run.Tail)-maxRunTail:]
	}
	run.LastEventAt = time.Now()
}

// TerminateGroup kills the run's recorded process group with the bounded
// ladder, if one is recorded and the run is still live. It is the cancel and
// shutdown path behind ControlAdapter.CancelRun.
func (r *RunRegistry) TerminateGroup(handle string, grace time.Duration) error {
	r.mu.RLock()
	run, ok := r.runs[handle]
	r.mu.RUnlock()
	if !ok || run.PGID <= 0 || !runLive(run.State) {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	return proc.TerminateGroup(ctx, run.PGID, grace)
}

// emit feeds one state transition to the Rex surface. Rex effects are
// cosmetic: they run on a background goroutine with their own timeout and
// their failures are logged, never propagated.
func (r *RunRegistry) emit(handle, agent string, state mcp.RunState) {
	if r.surface == nil {
		return
	}
	adapter := r.surface
	ev := rex.RunStateEvent{State: rexRunState(state)}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if agent != "" && state == mcp.StateRunning {
			if err := adapter.Handle(ctx, rex.RunStartedEvent{Agent: agent}); err != nil {
				r.log.Warn("rex surface: run started", "run", handle, "error", err)
			}
		}
		if err := adapter.Handle(ctx, ev); err != nil {
			r.log.Warn("rex surface: state badge", "run", handle, "error", err)
		}
	}()
}

// rexRunState maps a daemon run state to the badge-facing Rex run state.
// The mapping is lossy: Rex has no pending, failed, or canceled badge, and
// those states show as idle.
func rexRunState(s mcp.RunState) rex.RunState {
	switch s {
	case mcp.StateRunning:
		return rex.RunStateRunning
	case mcp.StateDeferred:
		return rex.RunStateDeferred
	case mcp.StateSettled:
		return rex.RunStateSettled
	default:
		return rex.RunStateIdle
	}
}

// runLive reports whether a run state leaves a process group that cancel and
// shutdown must reach.
func runLive(s mcp.RunState) bool {
	switch s {
	case mcp.StatePending, mcp.StateRunning, mcp.StateDeferred:
		return true
	default:
		return false
	}
}

// stateName returns the stable wire name of a run state.
func stateName(s mcp.RunState) string { return s.String() }
