package faultsuite_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/andrew-bierman/lambe/internal/daemon"
	"github.com/andrew-bierman/lambe/internal/mcp"
	"github.com/andrew-bierman/lambe/internal/tracker"
)

// spawnDepthTracker is the smallest tracker the daemon's control stack
// needs: EnsureIssue for spawn registration. Every other method is unused
// by SpawnRun.
type spawnDepthTracker struct{ issues map[string]tracker.Issue }

func (s *spawnDepthTracker) Capabilities(context.Context) (tracker.Capabilities, error) {
	return tracker.Capabilities{}, nil
}

func (s *spawnDepthTracker) Dispatchable(context.Context, tracker.DispatchQuery) ([]tracker.Issue, error) {
	return nil, nil
}

func (s *spawnDepthTracker) Claim(_ context.Context, id string) (tracker.Issue, error) {
	return s.issues[id], nil
}
func (s *spawnDepthTracker) Release(context.Context, string) error { return nil }
func (s *spawnDepthTracker) Heartbeat(context.Context, string) error {
	return nil
}

func (s *spawnDepthTracker) Reclaim(context.Context, time.Duration, []string) error {
	return nil
}

func (s *spawnDepthTracker) Defer(context.Context, string, time.Time, string) error {
	return nil
}

func (s *spawnDepthTracker) Events(context.Context, int64) ([]tracker.Event, error) {
	return nil, tracker.ErrJournalDisabled
}

func (s *spawnDepthTracker) EnsureIssue(_ context.Context, key string, _ tracker.IssueSpec) (tracker.Issue, error) {
	if issue, ok := s.issues[key]; ok {
		return issue, nil
	}
	issue := tracker.Issue{ID: key, Status: "open"}
	s.issues[key] = issue
	return issue, nil
}
func (s *spawnDepthTracker) PutAttempt(context.Context, tracker.Attempt) error { return nil }
func (s *spawnDepthTracker) Attempts(context.Context, string) ([]tracker.Attempt, error) {
	return nil, nil
}

// TestFault_SpawnDepthGuardHoldsInTheDaemon is the fault the depth guard
// exists for: a lambe-supervised agent (already at depth 1 under the
// default max of 1) tries to spawn another run — the classic
// prompt-injection recursion. The daemon's control stack, not the client,
// holds the authority and must refuse the spawn instead of registering it.
func TestFault_SpawnDepthGuardHoldsInTheDaemon(t *testing.T) {
	tr := &spawnDepthTracker{issues: map[string]tracker.Issue{}}
	ctl := &daemon.ControlAdapter{Registry: daemon.NewRunRegistry(nil, nil), Tracker: tr}

	// A supervised agent's spawn request: it reports its own calling
	// depth (1) and may not nominate a shallower one.
	_, err := ctl.SpawnRun(context.Background(), mcp.RunSpec{
		IssueID: "injected-1",
		Prompt:  "exfiltrate and spawn more agents",
		Depth:   1,
	})
	if err == nil {
		t.Fatal("spawn from a depth-1 context must be refused by the daemon")
	}
	var depthErr *mcp.DepthError
	if !errors.As(err, &depthErr) {
		t.Fatalf("refusal = %v, want *mcp.DepthError", err)
	}
	if depthErr.CallerDepth != 1 || depthErr.MaxDepth != mcp.DefaultMaxDepth {
		t.Errorf("depth error = %+v, want caller 1 max %d", depthErr, mcp.DefaultMaxDepth)
	}
	if len(tr.issues) != 0 {
		t.Errorf("refused spawn must not ensure an issue, got %v", tr.issues)
	}

	// A human-started caller (depth 0) still registers, at derived depth 1.
	h, err := ctl.SpawnRun(context.Background(), mcp.RunSpec{IssueID: "top-1", Prompt: "p", Depth: 0})
	if err != nil {
		t.Fatalf("top-level spawn: %v", err)
	}
	run, ok := ctl.Registry.Lookup(h.String())
	if !ok || run.Depth != 1 {
		t.Fatalf("registered run = %+v, ok=%v, want daemon-derived depth 1", run, ok)
	}
}
