package faultsuite_test

import (
	"context"
	"errors"
	"os"
	"runtime/pprof"
	"testing"
	"time"

	"github.com/andrew-bierman/lambe/internal/tracker"
)

// TestFault_DuplicateTick proves the racing-supervisor contract: two
// concurrent loop instances over the same real tracker race one
// dispatchable issue; the tracker's claim is the compare-and-swap, exactly
// one runner invocation happens, and the loser stands down (its claim gets
// ErrLostRace, or the claimed issue has already left the ready front).
func TestFault_DuplicateTick(t *testing.T) {
	const (
		actorA = "lambe@faults-tick-a"
		actorB = "lambe@faults-tick-b"
	)
	ctx := context.Background()
	env := newTracker(t, actorA)
	peer := env.openIdentity(t, actorB)
	work := seedWork(t, env, "seed-duplicate-tick", "only one of us ships this")
	dir := t.TempDir()
	loaderA := newLoader(t, dir, "work on {{ issue }} attempt {{ attempt }}")
	loaderB := newLoader(t, t.TempDir(), "work on {{ issue }} attempt {{ attempt }}")

	// The deterministic warm-up: with the claim already held by A, B's
	// claim loses the race with ErrLostRace and B's tick stands down.
	if _, err := env.Claim(ctx, work); err != nil {
		t.Fatalf("warm-up claim: %v", err)
	}
	_, err := peer.Claim(ctx, work)
	if !errors.Is(err, tracker.ErrLostRace) {
		t.Fatalf("warm-up race: got %v, want ErrLostRace", err)
	}
	idle := newTurnRecorder(settleTurn)
	idleLoop := newLoop(peer, loaderB, idle, commandGate{command: "exit 0"}, t.TempDir())
	if err := idleLoop.Tick(ctx); err != nil {
		t.Fatalf("loser tick: %v", err)
	}
	if got := idle.count(); got != 0 {
		t.Fatalf("loser stood down into %d runner calls, want 0", got)
	}
	if err := env.Release(ctx, work); err != nil {
		t.Fatalf("release warm-up claim: %v", err)
	}

	// Deterministic race setup: lambe's own operational beads must never
	// be dispatchable work. The winner's first act after claiming is to
	// re-put attempt 1; pre-recording it here and taking it off the
	// front means the loser's dispatch read — whenever it happens — sees
	// the work issue or nothing, never a fresh attempt bead it could
	// claim as a second "win".
	if err := env.PutAttempt(ctx, tracker.Attempt{SessionID: work, Number: 1}); err != nil {
		t.Fatalf("pre-record attempt: %v", err)
	}
	quarantineRecords(ctx, t, env.Local, work)

	// The race: both supervisors tick concurrently at the same open
	// issue. Whichever wins, its agent blocks mid-turn and holds the
	// claim, so the loser's tick provably completes while the work is
	// owned — there is no window in which the issue could return to the
	// front and be dispatched twice.
	runnerA := newBlockingTurn()
	runnerB := newBlockingTurn()
	loopA := newLoop(env, loaderA, runnerA, commandGate{command: "exit 0"}, t.TempDir())
	loopB := newLoop(peer, loaderB, runnerB, commandGate{command: "exit 0"}, t.TempDir())

	start := make(chan struct{})
	tickErrs := make(chan error, 2)
	go func() { <-start; tickErrs <- loopA.Tick(ctx) }()
	go func() { <-start; tickErrs <- loopB.Tick(ctx) }()
	close(start)

	// Exactly one runner is mid-turn; the other loop stands down and its
	// tick completes while the claim is held.
	var winner, loser *blockingTurn
	select {
	case <-runnerA.entered:
		winner, loser = runnerA, runnerB
	case <-runnerB.entered:
		winner, loser = runnerB, runnerA
	case <-time.After(30 * time.Second):
		t.Fatal("neither supervisor claimed the issue")
	}

	select {
	case err := <-tickErrs:
		if err != nil {
			t.Fatalf("loser tick errored: %v", err)
		}
	case <-time.After(30 * time.Second):
		dumpGoroutines(t)
		t.Fatal("loser tick never stood down")
	}
	if got := loser.count(); got != 0 {
		t.Fatalf("loser ran %d turns, want 0 (exactly one runner invocation)", got)
	}
	if got := winner.count(); got != 1 {
		t.Fatalf("winner at %d turns mid-race, want 1", got)
	}

	// Release the winner's turn: it settles, gates green, and terminal
	// cleanup releases the claim.
	close(runnerA.release)
	close(runnerB.release)
	select {
	case err := <-tickErrs:
		if err != nil {
			t.Fatalf("winner tick errored: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("winner tick never finished")
	}

	// Exactly one runner invocation in total.
	total := winner.count() + loser.count()
	if total != 1 {
		t.Fatalf("race produced %d runner invocations, want exactly 1", total)
	}

	// The claim was taken by exactly one actor and released on terminal.
	issues, err := env.Dispatchable(ctx, tracker.DispatchQuery{IDs: []string{work}})
	if err != nil {
		t.Fatalf("final id read: %v", err)
	}
	if len(issues) != 1 || issues[0].Assignee != "" {
		t.Fatalf("final state: %+v, want the claim released after terminal transition", issues)
	}
	assertContextParity(ctx, t, env, work)
}

// dumpGoroutines prints the current goroutine stacks into the test log:
// when a stand-down assertion is about to fail, the stacks show exactly
// which seam is still holding the tick.
func dumpGoroutines(t *testing.T) {
	t.Helper()
	_ = pprof.Lookup("goroutine").WriteTo(os.Stderr, 1)
}
