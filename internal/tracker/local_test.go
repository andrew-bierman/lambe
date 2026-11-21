package tracker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLocalInitDoesNotInitializeAfterStatusTimeout(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bd")
	writeExecutable(t, bin, `#!/bin/sh
case "$1" in
  status) printf started > status-started; exec sleep 10 ;;
  init) printf unexpected > init-started ;;
esac
`)
	l, err := NewLocal(bin, dir, "test")
	if err != nil {
		t.Fatal(err)
	}
	// Exercise the transport's own timeout while the caller can still run commands.
	l.shell.CommandTimeout = time.Second
	if err := l.Init(t.Context()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Init = %v, want status timeout", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "status-started")); err != nil {
		t.Fatalf("status did not start: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "init-started")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("init must not run after status timeout: %v", err)
	}
}

// bdBin returns the bd binary path, or "" when the tests must skip. These
// tests are skipped under -short and when bd is unavailable; a CI runner
// that wants them must install bd and run the full suite. LAMBE_TEST_BD
// gates discovery per AGENTS.md: unset, "0", or "false" skips (returns "");
// "1" or "true" means "use the first bd found on PATH or in ~/.local/bin";
// any other value names an explicit binary path.
func bdBin() string {
	gate := os.Getenv("LAMBE_TEST_BD")
	switch gate {
	case "", "0", "false":
		return ""
	case "1", "true":
	default:
		return gate
	}
	if bin, err := exec.LookPath("bd"); err == nil {
		return bin
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidate := filepath.Join(home, ".local", "bin", "bd")
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	return ""
}

func needBD(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping bd integration test in -short mode")
	}
	bin := bdBin()
	if bin == "" {
		t.Skip("bd binary not found; skipping bd integration test")
	}
	return bin
}

// newLocal spins up a throwaway tracker in a temp directory with its own
// identity, per the disposable-identity discipline for scratch bd repos.
func newLocal(t *testing.T, actor string) *Local {
	t.Helper()
	bin := needBD(t)
	dir := t.TempDir()
	l, err := NewLocal(bin, dir, actor)
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	if err := l.Init(t.Context()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	return l
}

// mkIssue creates one plain open issue and returns its id.
func mkIssue(t *testing.T, l *Local, title string) string {
	t.Helper()
	issue, err := l.EnsureIssue(t.Context(), "seed-"+title, IssueSpec{Title: title, Priority: 2})
	if err != nil {
		t.Fatalf("seed issue %q: %v", title, err)
	}
	return issue.ID
}

func TestBDTwoReadDispatchReturnsDispatchable(t *testing.T) {
	l := newLocal(t, "lambe@race-a")
	a := mkIssue(t, l, "alpha")
	b := mkIssue(t, l, "beta")

	byStatus, err := l.Dispatchable(t.Context(), DispatchQuery{Statuses: []string{"open"}})
	if err != nil {
		t.Fatalf("dispatch by statuses: %v", err)
	}
	var got int
	for _, iss := range byStatus {
		if iss.ID == a || iss.ID == b {
			got++
		}
	}
	if got != 2 {
		t.Errorf("status read: got %d of the seeded issues, want 2 (%+v)", got, byStatus)
	}

	byIDs, err := l.Dispatchable(t.Context(), DispatchQuery{IDs: []string{a, b}})
	if err != nil {
		t.Fatalf("dispatch by ids: %v", err)
	}
	if len(byIDs) != 2 {
		t.Errorf("id read: got %d issues, want 2", len(byIDs))
	}
	if byIDs[0].Status == "" {
		t.Errorf("id read returned issues without status: %+v", byIDs)
	}
}

func TestBDEmptyQuerySkipsBdEntirely(t *testing.T) {
	l := newLocal(t, "lambe@empty")
	issues, err := l.Dispatchable(t.Context(), DispatchQuery{})
	if err != nil {
		t.Fatalf("Dispatchable: %v", err)
	}
	if issues != nil {
		t.Errorf("issues: got %v, want nil", issues)
	}
}

func TestBDClaimRaceLoserStandsDown(t *testing.T) {
	winner := newLocal(t, "lambe@race-a")
	loser, err := NewLocal(winner.shell.Bin, winner.shell.Dir, "lambe@race-b")
	if err != nil {
		t.Fatalf("NewLocal loser: %v", err)
	}
	id := mkIssue(t, winner, "raced")

	issue, err := winner.Claim(t.Context(), id)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if issue.Status != "in_progress" || issue.Assignee != "lambe@race-a" {
		t.Errorf("first claim: got status %q assignee %q, want in_progress/lambe@race-a", issue.Status, issue.Assignee)
	}

	_, err = loser.Claim(t.Context(), id)
	if !errors.Is(err, ErrLostRace) {
		t.Fatalf("second claim: got %v, want ErrLostRace (stand down)", err)
	}
}

func TestBDGuardedWriteExitThirteenIsLostRace(t *testing.T) {
	l := newLocal(t, "lambe@guard")
	id := mkIssue(t, l, "guarded")
	if _, err := l.Claim(t.Context(), id); err != nil {
		t.Fatalf("claim: %v", err)
	}
	// A guarded write whose precondition no longer holds: bd exits 13,
	// which the tracker layer maps to ErrLostRace.
	nobody := "not-the-holder"
	_, err := l.shell.Update(t.Context(), UpdateRequest{
		ID:         id,
		IfAssignee: &nobody,
		Assignee:   "lambe@guard",
		Status:     "open",
	})
	if err == nil {
		t.Fatal("guarded update: unexpectedly succeeded")
	}
	if typed := classifyExit(err); !errors.Is(typed, ErrLostRace) {
		t.Fatalf("guarded update exit %v: classified %v, want ErrLostRace", err, typed)
	}
}

func TestBDHeartbeatAfterLossIsStopSignal(t *testing.T) {
	l := newLocal(t, "lambe@hb")
	id := mkIssue(t, l, "heartbeat")
	if _, err := l.Claim(t.Context(), id); err != nil {
		t.Fatalf("claim: %v", err)
	}
	// A reaper (reclaim or unclaim) takes the claim away.
	if err := l.Release(t.Context(), id); err != nil {
		t.Fatalf("release: %v", err)
	}
	err := l.Heartbeat(t.Context(), id)
	if !errors.Is(err, ErrNotOwner) {
		t.Fatalf("heartbeat after loss: got %v, want ErrNotOwner (stop signal)", err)
	}
}

func TestBDReclaimIsScopedAndHarmless(t *testing.T) {
	l := newLocal(t, "lambe@reap")
	id := mkIssue(t, l, "reclaimed")
	if _, err := l.Claim(t.Context(), id); err != nil {
		t.Fatalf("claim: %v", err)
	}
	// The lease has not expired, so a reclaim must not steal the live claim.
	if err := l.Reclaim(t.Context(), 0, []string{id}); err != nil {
		t.Fatalf("reclaim live lease: %v", err)
	}
	if err := l.Heartbeat(t.Context(), id); err != nil {
		t.Fatalf("heartbeat after reclaim of live lease: %v", err)
	}
}

func TestBDDeferUntilPastReentersReady(t *testing.T) {
	l := newLocal(t, "lambe@wake")
	id := mkIssue(t, l, "snoozed")

	past := time.Now().UTC().Add(-time.Minute)
	if err := l.ArmWake(t.Context(), id, past, "wake immediately"); err != nil {
		t.Fatalf("defer: %v", err)
	}

	// The ready-front read lazily wakes the issue whose until has passed.
	ready, err := l.Dispatchable(t.Context(), DispatchQuery{Statuses: []string{"ready"}})
	if err != nil {
		t.Fatalf("ready read: %v", err)
	}
	var woke bool
	for _, iss := range ready {
		if iss.ID == id {
			woke = true
		}
	}
	if !woke {
		t.Errorf("deferred issue %s did not re-enter the ready front: %+v", id, ready)
	}
}

func TestBDPinnedDuplicateCreateIsNoOp(t *testing.T) {
	l := newLocal(t, "lambe@pin")
	spec := IssueSpec{Title: "pinned", Priority: 1, Metadata: map[string]string{"k": "v"}}
	first, err := l.EnsureIssue(t.Context(), "pin-key", spec)
	if err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	second, err := l.EnsureIssue(t.Context(), "pin-key", spec)
	if err != nil {
		t.Fatalf("duplicate ensure: %v", err)
	}
	if first.ID != second.ID {
		t.Errorf("pinned id drifted: %s vs %s", first.ID, second.ID)
	}
	if second.Title != "pinned" {
		t.Errorf("existing issue not returned: %+v", second)
	}
}

func TestBDAttemptRecordsRoundTrip(t *testing.T) {
	l := newLocal(t, "lambe@att")
	wake := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	if err := l.PutAttempt(t.Context(), Attempt{SessionID: "ses-1", Number: 1, PGID: 4242}); err != nil {
		t.Fatalf("put attempt 1: %v", err)
	}
	if err := l.PutAttempt(t.Context(), Attempt{SessionID: "ses-1", Number: 2, PGID: 9999, WakeAt: wake}); err != nil {
		t.Fatalf("put attempt 2: %v", err)
	}
	// Re-put is idempotent (update, not a second record).
	if err := l.PutAttempt(t.Context(), Attempt{SessionID: "ses-1", Number: 2, PGID: 7777, ProcessIdentity: "darwin:1794840000:123456", WakeAt: wake}); err != nil {
		t.Fatalf("re-put attempt 2: %v", err)
	}

	if err := l.PutAttempt(t.Context(), Attempt{SessionID: "ses-1", Number: 2, WakeAt: wake}); err != nil {
		t.Fatal(err)
	}
	attempts, err := l.Attempts(t.Context(), "ses-1")
	if err != nil {
		t.Fatalf("attempts: %v", err)
	}
	if len(attempts) != 2 {
		t.Fatalf("attempts: got %d, want 2 (%+v)", len(attempts), attempts)
	}
	if attempts[0].Number != 1 || attempts[0].PGID != 4242 {
		t.Errorf("attempt 1: got %+v", attempts[0])
	}
	if attempts[1].PGID != 7777 || attempts[1].ProcessIdentity != "darwin:1794840000:123456" {
		t.Errorf("attempt 2 re-put did not update pgid: %+v", attempts[1])
	}
	if !attempts[1].WakeAt.Equal(wake) {
		t.Errorf("attempt 2 wake: got %v, want %v", attempts[1].WakeAt, wake)
	}
}

func TestBDEventsConsumeCheckpointAndRebaseline(t *testing.T) {
	l := newLocal(t, "lambe@ev")

	var sawSeq int64
	for i := 0; i < 3; i++ {
		mkIssue(t, l, fmt.Sprintf("journaled-%d", i))
		events, err := l.Events(t.Context(), sawSeq)
		if err != nil {
			t.Fatalf("events after %d: %v", i, err)
		}
		for _, ev := range events {
			if ev.Seq <= sawSeq {
				t.Fatalf("event %d not past checkpoint %d", ev.Seq, sawSeq)
			}
			sawSeq = ev.Seq
		}
	}
	if sawSeq < 3 {
		t.Fatalf("checkpoint %d after three journaled creates, want >= 3", sawSeq)
	}

	// Force truncation below the checkpoint: cut retention to the bone and
	// prune everything retained so far.
	if _, err := l.shell.run(t.Context(), []string{"config", "set", "events-journal-retain-rows", "1"}); err != nil {
		t.Fatalf("set retain rows: %v", err)
	}
	if _, err := l.shell.run(t.Context(), []string{"config", "set", "events-journal-retain-days", "0"}); err != nil {
		t.Fatalf("set retain days: %v", err)
	}
	if _, err := l.shell.run(t.Context(), []string{"events", "prune", "--before", fmt.Sprint(sawSeq + 1)}); err != nil {
		t.Fatalf("prune: %v", err)
	}

	_, err := l.Events(t.Context(), 0)
	var trunc *TruncatedError
	if !errors.As(err, &trunc) {
		t.Fatalf("events after prune: got %v, want TruncatedError", err)
	}
	rebased, err := l.Events(t.Context(), trunc.Floor)
	if err != nil {
		t.Fatalf("re-baseline read: %v", err)
	}
	for _, ev := range rebased {
		if ev.Seq < trunc.Floor {
			t.Errorf("re-baseline returned pruned event %d", ev.Seq)
		}
	}
}

func TestBDCapabilitiesFromInstalledVersion(t *testing.T) {
	l := newLocal(t, "lambe@caps")
	ver, err := l.shell.Version(t.Context())
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	t.Logf("bd under test: %s", strings.TrimSpace(ver))
	caps, err := l.Capabilities(t.Context())
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	t.Logf("capabilities: %+v", caps)
}

func TestBDRecordRunWritesWisp(t *testing.T) {
	l := newLocal(t, "lambe@wisp")
	issue, err := l.RecordRun(t.Context(), RunRecord{Title: "session ses-9 turn 3", Kind: "run", At: time.Now()})
	if err != nil {
		t.Fatalf("record run: %v", err)
	}
	if issue.ID == "" {
		t.Fatal("record run returned no id")
	}
}

// BenchmarkBDOperations measures the per-operation cost of the embedded
// shell transport: every bd invocation pays engine startup. These numbers
// inform (but do not yet drive) the batch-vs-single-write decision.
func BenchmarkBDOperations(b *testing.B) {
	bin := bdBin()
	if bin == "" {
		b.Skip("bd binary not found")
	}
	dir := b.TempDir()
	l, err := NewLocal(bin, dir, "lambe@bench")
	if err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	if err := l.Init(ctx); err != nil {
		b.Fatal(err)
	}
	id := mkBenchIssue(b, l)
	hbID := mkBenchIssue(b, l)
	if _, err := l.Claim(ctx, hbID); err != nil {
		b.Fatal(err)
	}

	b.Run("create", func(b *testing.B) {
		for b.Loop() {
			if _, err := l.shell.Create(ctx, CreateRequest{Title: "bench", Priority: 2}); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("show", func(b *testing.B) {
		for b.Loop() {
			if _, err := l.shell.Show(ctx, id); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("list", func(b *testing.B) {
		for b.Loop() {
			if _, err := l.shell.List(ctx, DispatchQuery{Statuses: []string{"open"}}); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("claim_release_pair", func(b *testing.B) {
		for b.Loop() {
			if _, err := l.Claim(ctx, id); err != nil {
				b.Fatal(err)
			}
			if err := l.Release(ctx, id); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("heartbeat", func(b *testing.B) {
		for b.Loop() {
			if err := l.Heartbeat(ctx, hbID); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("defer", func(b *testing.B) {
		for b.Loop() {
			until := time.Now().Add(time.Hour)
			if err := l.Defer(ctx, id, until, "bench"); err != nil {
				b.Fatal(err)
			}
		}
	})
}

var benchIssueSeq int

func mkBenchIssue(b *testing.B, l *Local) string {
	b.Helper()
	benchIssueSeq++
	issue, err := l.EnsureIssue(context.Background(), fmt.Sprintf("bench-%d", benchIssueSeq), IssueSpec{Title: "bench", Priority: 2})
	if err != nil {
		b.Fatal(err)
	}
	return issue.ID
}

func TestLocalReadinessIncludesBlocker(t *testing.T) {
	l := newLocal(t, "lambe@readiness")
	parent := mkIssue(t, l, "Prepare input")
	child := mkIssue(t, l, "Process input")
	if _, err := l.shell.run(t.Context(), []string{"dep", "add", child, parent}); err != nil {
		t.Fatal(err)
	}
	rows, err := l.Readiness(t.Context(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var ready, blocked bool
	for _, row := range rows {
		switch row.ID {
		case parent:
			ready = !row.Blocked
		case child:
			blocked = row.Blocked && len(row.Blockers) == 1 && row.Blockers[0].ID == parent
		}
	}
	if !ready || !blocked {
		t.Fatalf("expected ready parent and blocked child, got %+v", rows)
	}
}
