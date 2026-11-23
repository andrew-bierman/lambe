package tracker

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// withLimits installs process-wide limits for one test and restores the
// defaults after. Tests using it must not run in parallel with each other.
func withLimits(t *testing.T, l Limits) {
	t.Helper()
	SetLimits(l)
	t.Cleanup(func() { SetLimits(Limits{}) })
}

// slowBD writes a fake bd that sleeps for secs before printing empty JSON.
func slowBD(t *testing.T, secs string) string {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "bd")
	writeExecutable(t, script, "#!/bin/sh\nsleep "+secs+"\necho '[]'\n")
	return script
}

// TestGateServesLanesByPriority proves the queue order: with the only slot
// held, a queued heartbeat, read, and write are released write first, read
// second, background last, regardless of arrival order.
func TestGateServesLanesByPriority(t *testing.T) {
	g := newGate(1)
	hold, err := g.acquire(t.Context(), LaneRead, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var (
		mu    sync.Mutex
		order []Lane
		wg    sync.WaitGroup
	)
	for _, lane := range []Lane{LaneBackground, LaneRead, LaneWrite} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := g.acquire(t.Context(), lane, time.Minute)
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			order = append(order, lane)
			mu.Unlock()
			release()
		}()
		time.Sleep(30 * time.Millisecond) // let each waiter enqueue before the next arrives
	}
	hold()
	wg.Wait()
	want := []Lane{LaneWrite, LaneRead, LaneBackground}
	if fmt.Sprint(order) != fmt.Sprint(want) {
		t.Fatalf("service order = %v, want %v", order, want)
	}
}

func waiting(g *gate) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, q := range g.queues {
		n += len(q)
	}
	return n
}

// TestWritesNotStarvedByQueuedHeartbeats queues 50 heartbeats behind a
// two-slot cap, then issues a write: it must finish in about one process
// run, long before the heartbeat backlog (50 runs / 2 slots) drains.
func TestWritesNotStarvedByQueuedHeartbeats(t *testing.T) {
	withLimits(t, Limits{MaxConcurrent: 2})
	tr := NewShellTransport(slowBD(t, "0.2"), t.TempDir(), "lambe@test")

	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := tr.Heartbeat(t.Context(), fmt.Sprintf("x-%d", i)); err != nil {
				t.Error(err)
			}
		}()
	}
	time.Sleep(150 * time.Millisecond) // both slots busy, the rest queued
	start := time.Now()
	if _, err := tr.Update(t.Context(), UpdateRequest{ID: "x-1", Status: StatusBlocked}); err != nil {
		t.Fatalf("write: %v", err)
	}
	took := time.Since(start)
	if took > 1500*time.Millisecond {
		t.Fatalf("write took %s behind 50 queued heartbeats; the backlog alone is ~5s", took)
	}
	if n := waiting(bdGate); n < 20 {
		t.Fatalf("only %d heartbeats still queued after the write finished; the write waited for the backlog", n)
	}
	wg.Wait()
}

// TestTimeoutClockExcludesQueueWait runs four 300ms calls through one slot
// with a 700ms run bound: the last waits about 900ms in the queue, which must
// not count against its bound.
func TestTimeoutClockExcludesQueueWait(t *testing.T) {
	withLimits(t, Limits{MaxConcurrent: 1})
	tr := NewShellTransport(slowBD(t, "0.3"), t.TempDir(), "lambe@test")
	tr.CommandTimeout = 700 * time.Millisecond

	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := tr.Show(t.Context(), "x-1")
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("a queued call timed out on queue wait: %v", err)
		}
	}
}

// TestQueueWaitIsBoundedSeparately proves a call that cannot get a slot
// fails with ErrQueueWait (also a deadline error), while the run bound
// stays untouched.
func TestQueueWaitIsBoundedSeparately(t *testing.T) {
	withLimits(t, Limits{MaxConcurrent: 1, QueueWait: 100 * time.Millisecond})
	tr := NewShellTransport(slowBD(t, "1"), t.TempDir(), "lambe@test")

	go func() { _, _ = tr.Show(t.Context(), "x-1") }()
	time.Sleep(100 * time.Millisecond)
	_, err := tr.Show(t.Context(), "x-2")
	if !errors.Is(err, ErrQueueWait) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want ErrQueueWait wrapping DeadlineExceeded", err)
	}
	if waiting(bdGate) != 0 {
		t.Fatal("timed-out waiter was left in the queue")
	}
}

// TestLimitsResolveRunTimeouts pins the per-class bounds: reads use
// Timeout, writes use WriteTimeout when set, and a transport override wins.
func TestLimitsResolveRunTimeouts(t *testing.T) {
	withLimits(t, Limits{Timeout: 90 * time.Second, WriteTimeout: 180 * time.Second})
	tr := NewShellTransport("bd", ".", "a")
	if got := tr.commandTimeout(LaneRead); got != 90*time.Second {
		t.Errorf("read bound = %v", got)
	}
	if got := tr.commandTimeout(LaneBackground); got != 90*time.Second {
		t.Errorf("heartbeat bound = %v", got)
	}
	if got := tr.commandTimeout(LaneWrite); got != 180*time.Second {
		t.Errorf("write bound = %v", got)
	}
	tr.CommandTimeout = time.Second
	if got := tr.commandTimeout(LaneWrite); got != time.Second {
		t.Errorf("transport override = %v", got)
	}
	SetLimits(Limits{Timeout: 90 * time.Second})
	tr.CommandTimeout = 0
	if got := tr.commandTimeout(LaneWrite); got != 90*time.Second {
		t.Errorf("write bound without WriteTimeout = %v, want Timeout", got)
	}
}

// attemptTransport extends fakeTransport with a pinned-id store, so the
// attempt flow's call shape is observable.
type attemptTransport struct {
	fakeTransport
	exists  map[string]bool
	creates []CreateRequest
	updates []UpdateRequest
}

func (a *attemptTransport) Create(_ context.Context, r CreateRequest) (Result, error) {
	a.calls = append(a.calls, "create")
	a.creates = append(a.creates, r)
	if a.exists[r.ID] {
		return Result{}, &ExitError{Code: 1, Stderr: r.ID + " already exists; use bd update"}
	}
	a.exists[r.ID] = true
	return Result{}, nil
}

func (a *attemptTransport) Update(_ context.Context, r UpdateRequest) (Result, error) {
	a.calls = append(a.calls, "update")
	a.updates = append(a.updates, r)
	if !a.exists[r.ID] {
		return Result{}, &ExitError{Code: 1, Stdout: `{"error":"resolving issue: no issue found matching \"` + r.ID + `\""}`}
	}
	return Result{}, nil
}

func (a *attemptTransport) ConfigGet(_ context.Context, _ string) (string, error) { return "t", nil }

// TestPutAttemptCallShape proves the cheap attempt flow: a new record is one
// create already carrying the blocked status and metadata, a re-put is one
// update, an already-existing record is create-then-update, and a stale
// cache entry heals itself with a create.
func TestPutAttemptCallShape(t *testing.T) {
	knownAttempts.Range(func(k, _ any) bool { knownAttempts.Delete(k); return true })
	at := &attemptTransport{exists: map[string]bool{}}
	b := NewBeads(at, "lambe@test")
	put := func(a Attempt) []string {
		at.calls = nil
		if err := b.PutAttempt(t.Context(), a); err != nil {
			t.Fatalf("PutAttempt %+v: %v", a, err)
		}
		return at.calls
	}

	if got := put(Attempt{SessionID: "s1", Number: 1}); strings.Join(got, ",") != "create" {
		t.Fatalf("new record calls = %v, want one create", got)
	}
	c := at.creates[0]
	if c.ID != "t-att-s1-1" || c.Status != StatusBlocked || c.Metadata[metaSession] != "s1" || c.Metadata[metaKind] != kindAttempt {
		t.Fatalf("create request = %+v", c)
	}
	if got := put(Attempt{SessionID: "s1", Number: 1, PGID: 42, ProcessIdentity: "id"}); strings.Join(got, ",") != "update" {
		t.Fatalf("re-put calls = %v, want one update", got)
	}
	if u := at.updates[0]; u.Metadata[metaPGID] != "42" || u.Status != StatusBlocked {
		t.Fatalf("update request = %+v", u)
	}

	// Existing on the tracker but unknown to this process (a daemon restart,
	// or a second tracker instance): create reports "already exists", then
	// the update lands. Treated as success.
	at.exists["t-att-s2-1"] = true
	if got := put(Attempt{SessionID: "s2", Number: 1, PGID: 7}); strings.Join(got, ",") != "create,update" {
		t.Fatalf("existing record calls = %v, want create,update", got)
	}

	// Known locally but gone from the tracker: update misses, create heals.
	delete(at.exists, "t-att-s1-1")
	if got := put(Attempt{SessionID: "s1", Number: 1, PGID: 9}); strings.Join(got, ",") != "update,create" {
		t.Fatalf("stale cache calls = %v, want update,create", got)
	}
}

// TestAttemptsReadsByDeterministicIDs proves the read no longer scans
// metadata: one id-addressed list covers the candidate attempt numbers.
func TestAttemptsReadsByDeterministicIDs(t *testing.T) {
	ft := &fakeTransport{}
	b := NewBeads(ft, "lambe@test")
	if _, err := b.Attempts(t.Context(), "s1"); err != nil {
		t.Fatal(err)
	}
	if len(ft.lists) != 1 || len(ft.lists[0].Metadata) != 0 {
		t.Fatalf("lists = %+v, want one id-addressed read with no metadata filter", ft.lists)
	}
	ids := ft.lists[0].IDs
	if len(ids) != attemptProbeMax || ids[0] != "x-att-s1-1" || ids[len(ids)-1] != fmt.Sprintf("x-att-s1-%d", attemptProbeMax) {
		t.Fatalf("ids = %v...", ids[:2])
	}
}
