package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/andrew-bierman/lambe/internal/mcp"
	"github.com/andrew-bierman/lambe/internal/tracker"
)

// cancelGrace bounds the kill ladder a Control cancel may run against a
// live run's process group.
const cancelGrace = 10 * time.Second

// ControlAdapter implements mcp.Control over daemon state: the live run
// registry plus the primary tracker for spawn registration. It is a thin
// adapter — SpawnRun only registers, it never waits on run progress — and
// holds no state of its own beyond the seams it reads.
type ControlAdapter struct {
	// Registry is the daemon's live run state.
	Registry *RunRegistry
	// Tracker is the primary tracker; spawns ensure their issue exists.
	Tracker tracker.Tracker
	// Log receives cancel outcomes; nil uses the default logger.
	Log *slog.Logger
}

// Compile-time check that ControlAdapter satisfies the MCP control seam.
var _ mcp.Control = (*ControlAdapter)(nil)

// SpawnRun registers a new supervised run: the issue is ensured in the
// tracker (idempotent) and a pending run is recorded under the issue id as
// its handle. The next tick dispatches it. It returns immediately.
func (a *ControlAdapter) SpawnRun(ctx context.Context, spec mcp.RunSpec) (mcp.RunHandle, error) {
	if a.Registry == nil || a.Tracker == nil {
		return mcp.RunHandle{}, errors.New("daemon control: registry and tracker are required")
	}
	if spec.IssueID == "" {
		return mcp.RunHandle{}, errors.New("daemon control: issue id is required")
	}
	title := spec.Prompt
	if title == "" {
		title = "lambe run " + spec.IssueID
	}
	if _, err := a.Tracker.EnsureIssue(ctx, spec.IssueID, tracker.IssueSpec{
		Title:    firstLine(title),
		Priority: 2,
		Metadata: map[string]string{"lambe_spawn_depth": fmt.Sprintf("%d", spec.Depth)},
	}); err != nil {
		return mcp.RunHandle{}, fmt.Errorf("ensure issue %s: %w", spec.IssueID, err)
	}
	a.Registry.Register(spec.IssueID, "issues", spec.Prompt, spec.Workspace, spec.Depth)
	return mcp.NewRunHandle(spec.IssueID), nil
}

// RunStatus returns the run's current typed snapshot.
func (a *ControlAdapter) RunStatus(_ context.Context, handle mcp.RunHandle) (mcp.RunStatus, error) {
	run, ok := a.Registry.Lookup(handle.String())
	if !ok {
		return mcp.RunStatus{}, fmt.Errorf("unknown run %q", handle.String())
	}
	return mcp.RunStatus{
		State:       run.State,
		Attempt:     run.Attempt,
		LastEventAt: run.LastEventAt,
		WakeAt:      run.WakeAt,
		Outcome:     run.Outcome,
	}, nil
}

// RunOutput returns the run's artifact paths and bounded tail. The daemon
// does not yet own transcript artifacts (that lands with the harness
// wiring), so artifacts are empty and the tail is the registry's bounded
// record of recent output.
func (a *ControlAdapter) RunOutput(_ context.Context, handle mcp.RunHandle) (mcp.RunOutput, error) {
	run, ok := a.Registry.Lookup(handle.String())
	if !ok {
		return mcp.RunOutput{}, fmt.Errorf("unknown run %q", handle.String())
	}
	return mcp.RunOutput{Tail: run.Tail}, nil
}

// CancelRun requests a best-effort cancel: the run is marked canceled and,
// when a live process group is recorded, the bounded kill ladder runs
// against it.
func (a *ControlAdapter) CancelRun(_ context.Context, handle mcp.RunHandle) (mcp.CancelResult, error) {
	run, ok := a.Registry.Lookup(handle.String())
	if !ok {
		return mcp.CancelResult{}, fmt.Errorf("unknown run %q", handle.String())
	}
	if !runLive(run.State) {
		return mcp.CancelResult{State: run.State, Outcome: run.Outcome}, nil
	}
	if err := a.Registry.TerminateGroup(handle.String(), cancelGrace); err != nil {
		log := a.Log
		if log == nil {
			log = slog.Default()
		}
		log.Warn("cancel: group termination reported an error", "run", handle.String(), "error", err)
	}
	a.Registry.MarkCanceled(handle.String())
	return mcp.CancelResult{State: mcp.StateCanceled, Outcome: "canceled by caller"}, nil
}

// ListRuns returns a snapshot of all runs in registration order.
func (a *ControlAdapter) ListRuns(context.Context) ([]mcp.RunInfo, error) {
	runs := a.Registry.List()
	out := make([]mcp.RunInfo, 0, len(runs))
	for _, run := range runs {
		out = append(out, mcp.RunInfo{
			Handle:  mcp.NewRunHandle(run.IssueID),
			IssueID: run.IssueID,
			Status: mcp.RunStatus{
				State:       run.State,
				Attempt:     run.Attempt,
				LastEventAt: run.LastEventAt,
				WakeAt:      run.WakeAt,
				Outcome:     run.Outcome,
			},
		})
	}
	return out, nil
}

// firstLine reduces a prompt to its first line for an issue title.
func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}
