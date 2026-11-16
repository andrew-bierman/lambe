package faultsuite_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/andrew-bierman/lambe/internal/harness"
	"github.com/andrew-bierman/lambe/internal/harness/pi"
	"github.com/andrew-bierman/lambe/internal/tracker"
)

// TestFault_LimitLockout proves the provider-limit lockout contract in two
// wired halves:
//
//  1. A fake agent (this test binary re-exec'd) streams a limit-shaped
//     error report through the real pi driver, and the supervised turn
//     ends in a typed harness.Outcome of kind LimitSuspected — retryable,
//     attempt budget untouched, carrying a wake-at suggestion.
//  2. That outcome is applied to a real local tracker: the issue is
//     parked with a defer-with-wake (recorded on the attempt journal),
//     the wake lazily fires on the next dispatch read once its time
//     passes, and the attempt count stays at one across repeated
//     deferrals. The consecutive-defer escalation counter — the guarantee
//     that deferral is never an infinite silent loop — is modeled locally
//     (see the policy models in suite_test.go) and reaches needs-human
//     after five consecutive deferrals with no completed attempt.
func TestFault_LimitLockout(t *testing.T) {
	ctx := context.Background()
	// The tracker fixture is built before the supervised turn: the
	// outcome's wake-at starts counting at the turn, and the fixture
	// (bd init and friends) must not eat into it.
	env := newTracker(t, "lambe@faults-limit")
	out := runLimitStreamTurn(ctx, t)

	// The typed outcome: a suspected provider limit, not a failure.
	if out.Kind != harness.OutcomeLimitSuspected {
		t.Fatalf("outcome kind = %s, want limit_suspected", out.Kind)
	}
	if out.AttemptConsumed {
		t.Error("limit-suspected turn consumed the attempt; the budget must be untouched")
	}
	if !out.Retryable {
		t.Error("limit-suspected turn is not retryable")
	}
	if out.Confidence <= 0 {
		t.Errorf("limit confidence = %f, want > 0", out.Confidence)
	}
	started := time.Now()
	if out.WakeAt.Before(started) || out.WakeAt.After(started.Add(5*time.Second)) {
		t.Errorf("wake-at = %s, want shortly after the turn", out.WakeAt)
	}

	// Second half: the tracker side of the lockout.
	work := seedWork(t, env, "seed-limit-lockout", "batch the migrations")
	if _, err := env.Claim(ctx, work); err != nil {
		t.Fatalf("claim: %v", err)
	}

	// Fast-forward the wake when the fixture consumed the horizon. The
	// park must provably outlive the bd invocations around it, so the
	// lead is generous; the wake itself is fast-forwarded below by
	// re-deferring into the past instead of waiting on wall time.
	const wakeLead = 30 * time.Second
	if out.WakeAt.Before(time.Now().Add(wakeLead)) {
		out.WakeAt = time.Now().Add(wakeLead)
	}

	// Defer-with-wake recorded: the wake time rides on the attempt
	// record, and the defer parks the issue off the dispatch front.
	if err := env.PutAttempt(ctx, tracker.Attempt{SessionID: work, Number: 1, WakeAt: out.WakeAt}); err != nil {
		t.Fatalf("put attempt with wake: %v", err)
	}
	if err := applyLimitOutcome(ctx, env, work, out); err != nil {
		t.Fatalf("apply limit outcome: %v", err)
	}
	on, err := onFront(ctx, env, work)
	if err != nil {
		t.Fatalf("front read while deferred: %v", err)
	}
	if on {
		ids, _ := readyFront(ctx, env)
		issues, _ := env.Dispatchable(ctx, tracker.DispatchQuery{IDs: []string{work}})
		t.Fatalf("deferred issue still visible on the dispatch front: front=%v issue=%+v wake=%v", ids, issues, out.WakeAt)
	}
	assertContextParity(ctx, t, env, work)

	// The wake fires: once the wake-at passes, the next dispatch read
	// lazily returns the issue to the front. The clock is fast-forwarded
	// (a re-defer into the past) rather than waited out — the seam under
	// test is the lazy wake read, not the passage of time.
	wakeNow := time.Now().UTC().Add(-time.Minute)
	if err := env.Defer(ctx, work, wakeNow, "wake the locked-out issue (fast-forwarded)"); err != nil {
		t.Fatalf("fast-forward wake: %v", err)
	}
	waitFor(t, "deferred issue wakes back onto the front", 15*time.Second, func() (bool, error) {
		return onFront(ctx, env, work)
	})

	// Attempt budget untouched across repeated limit deferrals: every
	// re-put lands on the same pinned attempt record.
	for range 2 {
		if err := env.PutAttempt(ctx, tracker.Attempt{SessionID: work, Number: 1, WakeAt: out.WakeAt}); err != nil {
			t.Fatalf("re-put attempt during lockout: %v", err)
		}
	}
	attempts, err := env.Attempts(ctx, work)
	if err != nil {
		t.Fatalf("attempts after lockout: %v", err)
	}
	if len(attempts) != 1 || attempts[0].Number != 1 {
		t.Fatalf("attempt budget moved during lockout: %+v, want exactly attempt 1", attempts)
	}
	if attempts[0].WakeAt.IsZero() {
		t.Error("attempt record lost its wake timer")
	}

	// Escalation: repeated deferral with no completed attempt eventually
	// reaches needs-human instead of looping silently. Once a turn
	// completes, the streak resets.
	esc := &deferEscalator{}
	for i := 1; i < deferEscalationThreshold; i++ {
		if esc.Defer() {
			t.Fatalf("escalated after %d deferrals, want %d", i, deferEscalationThreshold)
		}
	}
	if !esc.Defer() {
		t.Fatalf("no escalation after %d consecutive deferrals", deferEscalationThreshold)
	}
	// The needs-human state itself is hub-visible through the committed
	// seams: escalate a fresh counter to its threshold, then park the
	// issue off the front with the reason in the defer.
	second := &deferEscalator{}
	secondEscalatedAt := 0
	for n := 1; ; n++ {
		if second.Defer() {
			secondEscalatedAt = n
			break
		}
	}
	if secondEscalatedAt != deferEscalationThreshold {
		t.Fatalf("fresh streak escalated at %d, want %d", secondEscalatedAt, deferEscalationThreshold)
	}
	if err := env.Defer(ctx, work, time.Now().UTC().Add(24*time.Hour), "needs-human: consecutive limit deferrals exhausted the escalation threshold"); err != nil {
		t.Fatalf("defer needs-human: %v", err)
	}
	on, err = onFront(ctx, env, work)
	if err != nil {
		t.Fatalf("front read after needs-human: %v", err)
	}
	if on {
		t.Error("needs-human issue still dispatchable")
	}
	assertContextParity(ctx, t, env, work)
}

// runLimitStreamTurn drives the real pi driver against the re-exec fake
// agent's limit script and returns the typed outcome of the turn.
func runLimitStreamTurn(ctx context.Context, t *testing.T) harness.Outcome {
	t.Helper()
	const horizon = 250 * time.Millisecond
	driver := pi.New(pi.Options{
		Path:         os.Args[0],
		Args:         []string{"--mode", "rpc"},
		Env:          append(os.Environ(), piFakeEnv+"=limit"),
		Dir:          t.TempDir(),
		SessionID:    "ses-fault-limit",
		LimitHorizon: horizon,
		Logger:       quietLog(),
	})
	// Events must be consumed concurrently with Run.
	eventsDone := make(chan struct{})
	go func() {
		defer close(eventsDone)
		for range driver.Events() {
		}
	}()
	defer func() {
		_ = driver.Close(context.WithoutCancel(ctx))
		<-eventsDone
	}()
	if err := driver.Start(ctx); err != nil {
		t.Fatalf("start fake agent driver: %v", err)
	}
	out, err := driver.Run(ctx, "do the work")
	if err != nil {
		t.Fatalf("run limit turn: %v", err)
	}
	return out
}
