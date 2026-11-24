package rex

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
)

// RunEvent is the narrow lifecycle-event seam the supervisor feeds to the
// Rex surface. The set is closed; unmapped events are ignored.
type RunEvent interface{ runEvent() }

// RunStartedEvent reports that a supervised agent run started. The adapter
// makes sure the agent has a Rex session named "lambe-<agent>", reusing an
// existing one.
type RunStartedEvent struct {
	// Agent names the supervised agent, e.g. "issues".
	Agent string
	// Prompt is the initial prompt text; used as session context.
	Prompt string
}

// RunStateEvent reports a transition in the run's typed state. The adapter
// maps it to an OSC 7501 status badge.
type RunStateEvent struct {
	// State is the run's new state.
	State RunState
	// RunID is the run's issue id. When set the badge goes to the per-run
	// record "lambe/<RunID>"; when empty it goes to the root record.
	RunID string
	// Block says why a RunStateBlocked run needs a human.
	Block Block
	// Msg is the badge's one-line message; when empty it is the state name.
	Msg string
}

// RunClearedEvent removes a run's badge record (or every lambe record when
// RunID is empty), for a run or daemon that has exited.
type RunClearedEvent struct {
	// RunID is the run whose record to remove; empty clears them all.
	RunID string
}

func (RunStartedEvent) runEvent() {}
func (RunStateEvent) runEvent()   {}
func (RunClearedEvent) runEvent() {}

// Adapter wires run lifecycle events to the two Rex surfaces: OSC 7501
// status badges on the writer (lambe's terminal) and one reused Rex session
// per supervised agent. Sessions are never closed: they are cheap, named by
// agent, and found again by name after a daemon restart. When Rex is unavailable it degrades to a no-op.
type Adapter struct {
	mu sync.Mutex

	// available gates every effect.
	available bool
	// client creates and reads Rex sessions.
	client Client
	// out receives OSC 7501 escape sequences; nil when the writer is not a
	// terminal.
	out io.Writer
	// sessions records the session in use for each agent name.
	sessions map[string]Session
}

// NewAdapter returns an Adapter for the given availability. When av reports
// Rex absent the adapter silently no-ops every event.
func NewAdapter(av Availability, client Client, out io.Writer) *Adapter {
	// Under launchd or systemd the daemon's stdout is a log file, where
	// escape sequences are just noise; only a terminal gets badges.
	if f, ok := out.(*os.File); ok {
		if fi, err := f.Stat(); err != nil || fi.Mode()&os.ModeCharDevice == 0 {
			out = nil
		}
	}
	return &Adapter{
		available: av.Available,
		client:    client,
		out:       out,
		sessions:  make(map[string]Session),
	}
}

// Handle applies one run lifecycle event. Session-creation errors propagate;
// badge emission errors after a successful session creation also propagate.
func (a *Adapter) Handle(ctx context.Context, ev RunEvent) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.available {
		return nil
	}
	switch e := ev.(type) {
	case RunStartedEvent:
		return a.startRun(ctx, e)
	case RunStateEvent:
		return a.emitState(e)
	case RunClearedEvent:
		if a.out == nil {
			return nil
		}
		return Clear(a.out, e.RunID)
	default:
		return nil
	}
}

// sessionPrefix namespaces lambe's sessions so a user's own session that
// shares an agent's name is never adopted.
const sessionPrefix = "lambe-"

// startRun makes sure the agent has a Rex session: the one already in use,
// else one found by name (made by an earlier daemon), else a new one.
func (a *Adapter) startRun(ctx context.Context, ev RunStartedEvent) error {
	if _, ok := a.sessions[ev.Agent]; ok {
		return nil
	}
	name := sessionPrefix + ev.Agent
	existing, err := a.client.ListSessions(ctx)
	if err != nil {
		return fmt.Errorf("rex sessions for agent %s: %w", ev.Agent, err)
	}
	for _, s := range existing {
		if s.Name == name {
			a.sessions[ev.Agent] = s
			return nil
		}
	}
	s, err := a.client.CreateSession(ctx, SessionRequest{
		Name:   name,
		Labels: map[string]string{"surface": "lambe", "agent": ev.Agent},
	})
	if err != nil {
		return fmt.Errorf("rex session for agent %s: %w", ev.Agent, err)
	}
	a.sessions[ev.Agent] = s
	return nil
}

// emitState maps a run state to its badge and writes the escape sequence.
func (a *Adapter) emitState(e RunStateEvent) error {
	if a.out == nil {
		return nil
	}
	msg := e.Msg
	if msg == "" {
		msg = e.State.String()
	}
	return Emit(a.out, Report{Status: MapRunState(e.State), Block: e.Block, ID: e.RunID, Title: e.RunID, Msg: msg})
}

// Sessions returns the sessions created so far, keyed by agent name.
func (a *Adapter) Sessions() map[string]Session {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[string]Session, len(a.sessions))
	for k, v := range a.sessions {
		out[k] = v
	}
	return out
}
