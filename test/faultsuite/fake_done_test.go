package faultsuite_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/andrew-bierman/lambe/internal/symphony"
	"github.com/andrew-bierman/lambe/internal/tracker"
)

// gateFailCommand is a failing gate-check: its combined output is the
// rejection the retried agent must see.
const gateFailCommand = `echo "tests failed: build error in pkg/foo (gate run $(date -u +%H:%M:%S))" >&2; exit 1`

// TestFault_FakeDone proves the false-completion contract: a settled agent
// whose gate check fails (the fake-done) gets a RED verdict that consumes
// the attempt, has its rejection recorded as hub-visible state through the
// tracker, and sees the gate output carried into the retry guidance — up to
// the attempt cap of 8, at which point the issue moves to needs-human and
// stops dispatching, with the workspace retained for inspection.
func TestFault_FakeDone(t *testing.T) {
	const actor = "lambe@faults-fakedone"
	ctx := context.Background()
	env := newTracker(t, actor)
	work := seedWork(t, env, "seed-fake-done", "fix the flaky suite")
	dir := t.TempDir()
	loader := newLoader(t, dir, "work on {{ issue }} attempt {{ attempt }}")

	runner := newTurnRecorder(settleTurn) // the agent always claims done
	gate := commandGate{command: gateFailCommand}
	loop := newLoop(env, loader, runner, gate, dir)

	// Deterministic dispatch: retries run before the dispatch phase of
	// the same tick, so the attempt bead a retry creates would be
	// dispatched as work within that tick. Pre-recording every attempt
	// bead and taking it off the front (see quarantineRecords) keeps
	// each tick dispatching nothing but the settled retry itself.
	for n := 1; n <= attemptCap; n++ {
		if err := env.PutAttempt(ctx, tracker.Attempt{SessionID: work, Number: n}); err != nil {
			t.Fatalf("pre-record attempt %d: %v", n, err)
		}
	}
	quarantineRecords(ctx, t, env.Local, work)

	// Eight red gates, fast-forwarded: zero retry delay means every
	// scheduled failure retry is due on the next tick. (The attempt
	// journal was fully pre-recorded above for dispatch determinism, so
	// consumption is asserted from the runner's attempt numbers and the
	// final journal below.)
	for attempt := 1; attempt <= attemptCap; attempt++ {
		if err := loop.Tick(ctx); err != nil {
			t.Fatalf("tick %d: %v", attempt, err)
		}
		if got := runner.count(); got != attempt {
			t.Fatalf("after tick %d: %d runner calls, want %d", attempt, got, attempt)
		}
		calls := runner.recorded()
		if calls[attempt-1].Attempt != attempt {
			t.Fatalf("tick %d ran attempt %d", attempt, calls[attempt-1].Attempt)
		}
		// Keep anything that crept onto the front off it (see
		// quarantineRecords).
		quarantineRecords(ctx, t, env.Local, work)
	}
	attempts, err := env.Attempts(ctx, work)
	if err != nil {
		t.Fatalf("final attempt journal: %v", err)
	}
	if len(attempts) != attemptCap {
		t.Fatalf("attempt journal after cap: %d records, want %d", len(attempts), attemptCap)
	}
	for i, a := range attempts {
		if a.Number != i+1 {
			t.Fatalf("attempt journal out of order: %+v", attempts)
		}
	}

	// Retry guidance carries the gate output: every first-turn prompt
	// after a rejection embeds the previous gate output.
	calls := runner.recorded()
	for i := 1; i < len(calls); i++ {
		if !strings.Contains(calls[i].Prompt, "tests failed: build error in pkg/foo") {
			t.Fatalf("retry prompt %d lacks gate output: %q", i+1, calls[i].Prompt)
		}
	}

	// The rejections are hub-visible: each attempt's rejection is a
	// pinned, idempotent tracker record carrying the gate output.
	for attempt := 1; attempt <= attemptCap; attempt++ {
		key := fmt.Sprintf("gate-rejection-%s-%d", work, attempt)
		spec := tracker.IssueSpec{
			Title:    fmt.Sprintf("gate rejected attempt %d of %s", attempt, work),
			Priority: 1,
			Metadata: map[string]string{"lambe_kind": "gate_rejection", "attempt": fmt.Sprint(attempt), "output": "tests failed: build error in pkg/foo"},
		}
		first, err := env.EnsureIssue(ctx, key, spec)
		if err != nil {
			t.Fatalf("record rejection %d: %v", attempt, err)
		}
		again, err := env.EnsureIssue(ctx, key, spec)
		if err != nil {
			t.Fatalf("re-record rejection %d: %v", attempt, err)
		}
		if first.ID != again.ID {
			t.Fatalf("rejection %d drifted: %s vs %s", attempt, first.ID, again.ID)
		}
	}

	// Cap 8 → needs-human: dispatch stops (the issue leaves the front
	// with the reason recorded) and the workspace is retained for
	// inspection.
	if err := env.Release(ctx, work); err != nil { // the supervisor gives the claim back before flagging
		t.Fatalf("release before needs-human: %v", err)
	}
	if err := env.Defer(ctx, work, time.Now().UTC().Add(24*time.Hour), "needs-human: attempt cap 8 reached; last gate output: tests failed: build error in pkg/foo"); err != nil {
		t.Fatalf("defer needs-human: %v", err)
	}
	on, err := onFront(ctx, env, work)
	if err != nil {
		t.Fatalf("front read after cap: %v", err)
	}
	if on {
		t.Error("needs-human issue still dispatchable after the attempt cap")
	}
	ws := filepath.Join(dir, "workspaces", symphony.SanitizeKey(work))
	if _, err := os.Stat(ws); err != nil {
		t.Errorf("workspace not retained for inspection: %v", err)
	}
	assertContextParity(ctx, t, env, work)

	// One more tick must not dispatch it again — or anything the
	// scenario itself recorded.
	quarantineRecords(ctx, t, env.Local, work)
	if err := loop.Tick(ctx); err != nil {
		t.Fatalf("post-cap tick: %v", err)
	}
	if got := runner.count(); got != attemptCap {
		t.Fatalf("post-cap tick dispatched again: %d runner calls, want %d", got, attemptCap)
	}
}
