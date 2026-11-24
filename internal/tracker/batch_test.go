package tracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// --- Grammar generation -------------------------------------------------

func TestBatchOpRender(t *testing.T) {
	p := func(n int) *int { return &n }
	cases := []struct {
		name    string
		op      BatchOp
		want    string
		wantErr bool
	}{
		{
			name: "close with comment",
			op:   BatchClose("x-1", "done: all gates green"),
			want: `close x-1 "done: all gates green"`,
		},
		{
			name: "close without comment",
			op:   BatchClose("x-1", ""),
			want: "close x-1",
		},
		{
			name:    "close without id",
			op:      BatchClose("", "why"),
			wantErr: true,
		},
		{
			name: "update status",
			op:   BatchUpdate("x-1", BatchFields{Status: "open"}),
			want: "update x-1 status=open",
		},
		{
			name: "update all fields sorted",
			op:   BatchUpdate("x-1", BatchFields{Status: "open", Title: "retitle me", Assignee: "lambe@h", Priority: p(1)}),
			want: `update x-1 status=open priority=1 title="retitle me" assignee=lambe@h`,
		},
		{
			name: "update force",
			op:   BatchUpdate("x-1", BatchFields{Status: "closed", Force: true}),
			want: "update x-1 status=closed force=true",
		},
		{
			name:    "update with no fields",
			op:      BatchUpdate("x-1", BatchFields{}),
			wantErr: true,
		},
		{
			name:    "update without id",
			op:      BatchUpdate("", BatchFields{Status: "open"}),
			wantErr: true,
		},
		{
			name: "create",
			op:   BatchCreate("task", 2, "attempt 3 of session s"),
			want: `create task 2 "attempt 3 of session s"`,
		},
		{
			name:    "create without title",
			op:      BatchCreate("task", 2, ""),
			wantErr: true,
		},
		{
			name:    "create without type",
			op:      BatchCreate("", 2, "t"),
			wantErr: true,
		},
		{
			name: "dep add with type",
			op:   BatchDepAdd("x-1", "x-2", "blocks"),
			want: "dep add x-1 x-2 blocks",
		},
		{
			name: "dep add default type",
			op:   BatchDepAdd("x-1", "x-2", ""),
			want: "dep add x-1 x-2",
		},
		{
			name:    "dep add without to",
			op:      BatchDepAdd("x-1", "", ""),
			wantErr: true,
		},
		{
			name: "dep remove",
			op:   BatchDepRemove("x-1", "x-2"),
			want: "dep remove x-1 x-2",
		},
		{
			name:    "dep remove without from",
			op:      BatchDepRemove("", "x-2"),
			wantErr: true,
		},
		{
			name:    "unknown verb",
			op:      BatchOp{Verb: BatchVerb("claim")},
			wantErr: true,
		},
		{
			name: "quote escaping",
			op:   BatchClose("x-1", `say "hi" \ done`),
			want: `close x-1 "say \"hi\" \\ done"`,
		},
	}
	for _, tc := range cases {
		got, err := tc.op.Render()
		if tc.wantErr {
			if err == nil {
				t.Errorf("%s: Render %v: want error, got line %q", tc.name, tc.op, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: Render: %v", tc.name, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: Render: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// --- Buffered writes ----------------------------------------------------

// batchRecorder captures Batch calls so buffering tests can prove
// exactly-one-flush semantics.
type batchRecorder struct {
	fakeTransport
	mu      sync.Mutex
	batches [][]BatchOp
}

func (f *batchRecorder) Batch(_ context.Context, ops []BatchOp) (Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.batches = append(f.batches, ops)
	return Result{}, nil
}

func TestFlushWritesSendsQueuedOpsInOneBatch(t *testing.T) {
	ft := &batchRecorder{}
	b := NewBeads(ft, "lambe@test")
	queue := func(err error) {
		if err != nil {
			t.Fatalf("queue: %v", err)
		}
	}
	queue(b.QueueClose("x-1", "done: gates green"))
	queue(b.QueueClose("x-2", ""))
	queue(b.QueueUpdate("x-3", BatchFields{Status: "open", Assignee: "lambe@test"}))
	queue(b.QueueCreate("task", 2, "attempt 1 of session s"))
	queue(b.QueueDepAdd("x-1", "x-3", ""))
	queue(b.QueueDepRemove("x-1", "x-2"))

	if got := b.BufferedWrites(); got != 6 {
		t.Fatalf("buffered writes: got %d, want 6", got)
	}
	if err := b.FlushWrites(t.Context()); err != nil {
		t.Fatalf("FlushWrites: %v", err)
	}
	if len(ft.batches) != 1 {
		t.Fatalf("batch calls: got %d, want exactly 1", len(ft.batches))
	}
	if got := len(ft.batches[0]); got != 6 {
		t.Fatalf("ops in batch: got %d, want 6", got)
	}
	if got := b.BufferedWrites(); got != 0 {
		t.Errorf("buffer not drained after flush: %d ops remain", got)
	}

	// A second flush with nothing queued makes no contact at all.
	if err := b.FlushWrites(t.Context()); err != nil {
		t.Fatalf("empty FlushWrites: %v", err)
	}
	if len(ft.batches) != 1 {
		t.Errorf("empty flush contacted the tracker: %d batch calls", len(ft.batches))
	}
}

// failingBatchTransport rejects every batch with a bd exit error.
type failingBatchTransport struct {
	fakeTransport
}

func (f *failingBatchTransport) Batch(_ context.Context, _ []BatchOp) (Result, error) {
	return Result{}, &ExitError{Code: 1, Stderr: "line 1 (close x-nope): not found: issue x-nope"}
}

func TestFlushWritesFailureKeepsBufferForRetry(t *testing.T) {
	b := NewBeads(&failingBatchTransport{}, "lambe@test")
	if err := b.QueueClose("x-nope", "stale"); err != nil {
		t.Fatalf("queue close: %v", err)
	}
	err := b.FlushWrites(t.Context())
	if err == nil {
		t.Fatal("FlushWrites: want error from failed batch")
	}
	if got := b.BufferedWrites(); got != 1 {
		t.Errorf("buffer after failed flush: got %d ops, want 1 (rolled back, retry-safe)", got)
	}
	if !strings.Contains(err.Error(), "flush writes") {
		t.Errorf("FlushWrites error lacks context: %v", err)
	}
}

func TestFlushWritesMapsTypedOutcomes(t *testing.T) {
	// exit 14 (migration freeze) on a batch is retryable like any write.
	b := NewBeads(&frozenBatchTransport{}, "lambe@test")
	if err := b.QueueClose("x-1", ""); err != nil {
		t.Fatalf("queue close: %v", err)
	}
	if err := b.FlushWrites(t.Context()); !errors.Is(err, ErrFrozen) {
		t.Fatalf("FlushWrites: got %v, want ErrFrozen", err)
	}
}

type frozenBatchTransport struct {
	fakeTransport
}

func (f *frozenBatchTransport) Batch(_ context.Context, _ []BatchOp) (Result, error) {
	return Result{}, &ExitError{Code: 14, Stderr: "migration freeze"}
}

func TestQueueRejectsGrammarIllegalOps(t *testing.T) {
	b := NewBeads(&batchRecorder{}, "lambe@test")
	if err := b.QueueClose("", "why"); err == nil {
		t.Error("QueueClose with empty id: want error")
	}
	if err := b.QueueUpdate("x-1", BatchFields{}); err == nil {
		t.Error("QueueUpdate with no fields: want error")
	}
	if got := b.BufferedWrites(); got != 0 {
		t.Errorf("rejected ops must not buffer: %d remain", got)
	}
}

// --- bd integration -----------------------------------------------------

func TestBDBatchFlushClosesAndComments(t *testing.T) {
	l := newLocal(t, "lambe@batch")
	a := mkIssue(t, l, "batch-a")
	b := mkIssue(t, l, "batch-b")
	if err := l.QueueClose(a, "done: gates green"); err != nil {
		t.Fatalf("queue close a: %v", err)
	}
	if err := l.QueueClose(b, ""); err != nil {
		t.Fatalf("queue close b: %v", err)
	}
	if err := l.FlushWrites(t.Context()); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if got := l.BufferedWrites(); got != 0 {
		t.Fatalf("buffer after flush: %d ops remain", got)
	}
	closed, err := l.Dispatchable(t.Context(), DispatchQuery{Statuses: []Status{"closed"}, IDs: []string{a, b}})
	if err != nil {
		t.Fatalf("read closed: %v", err)
	}
	if len(closed) != 2 {
		t.Fatalf("closed after batch: got %d of 2 (%+v)", len(closed), closed)
	}
	for _, iss := range closed {
		want := "done: gates green"
		if iss.ID == b {
			want = "Closed" // bd's default reason when none is given
		}
		if got := closedReason(t, l, iss.ID); got != want {
			t.Errorf("close_reason for %s: got %q, want %q", iss.ID, got, want)
		}
	}
}

// closedReason reads the close_reason field bd stamps on a closed issue.
func closedReason(t *testing.T, l *Local, id string) string {
	t.Helper()
	res, err := l.shell.Show(t.Context(), id)
	if err != nil {
		t.Fatalf("show %s: %v", id, err)
	}
	var rows []struct {
		CloseReason string `json:"close_reason"`
	}
	if err := json.Unmarshal(res.Data, &rows); err != nil {
		t.Fatalf("decode show %s: %v", id, err)
	}
	if len(rows) != 1 {
		t.Fatalf("show %s: got %d rows", id, len(rows))
	}
	return rows[0].CloseReason
}

func TestBDBatchFailureRollsBackAndKeepsBuffer(t *testing.T) {
	l := newLocal(t, "lambe@batchfail")
	a := mkIssue(t, l, "rollback-a")
	if err := l.QueueClose(a, ""); err != nil {
		t.Fatalf("queue close: %v", err)
	}
	if err := l.QueueClose("does-not-exist", ""); err != nil {
		t.Fatalf("queue bad close: %v", err)
	}
	err := l.FlushWrites(t.Context())
	if err == nil {
		t.Fatal("flush with a bad op: want error")
	}
	var exitErr *ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("flush error: got %T, want *ExitError", err)
	}
	if !strings.Contains(exitErr.Stderr+exitErr.Stdout, "does-not-exist") {
		t.Errorf("flush error does not name the failing line: %v", err)
	}
	if got := l.BufferedWrites(); got != 2 {
		t.Fatalf("buffer after failed flush: got %d ops, want 2 (rolled back, retry-safe)", got)
	}
	// Nothing was applied: the good close must not have landed.
	open, err := l.Dispatchable(t.Context(), DispatchQuery{Statuses: []Status{"open"}, IDs: []string{a}})
	if err != nil {
		t.Fatalf("read open: %v", err)
	}
	if len(open) != 1 {
		t.Errorf("valid close leaked through a failed batch: %+v", open)
	}
}

func TestBDBatchEmptyFlushMakesNoContact(t *testing.T) {
	l := newLocal(t, "lambe@noop")
	if err := l.FlushWrites(t.Context()); err != nil {
		t.Fatalf("empty flush: %v", err)
	}
}

// BenchmarkBDBatchVersusSingle measures the per-tick write-amplification
// win: a 5-write close batch (one bd batch invocation) against the same
// five writes one invocation at a time. Run with -benchtime=10x to match
// the recorded 10-iteration numbers.
func BenchmarkBDBatchVersusSingle(b *testing.B) {
	bin := bdBin()
	if bin == "" {
		b.Skip("bd binary not found")
	}
	dir := b.TempDir()
	l, err := NewLocal(bin, dir, "lambe@bench-batch")
	if err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	if err := l.Init(ctx); err != nil {
		b.Fatal(err)
	}
	var ids [5]string
	for i := range ids {
		benchIssueSeq++
		issue, err := l.EnsureIssue(ctx, fmt.Sprintf("bench-batch-%d", benchIssueSeq), IssueSpec{Title: "bench", Priority: 2})
		if err != nil {
			b.Fatal(err)
		}
		ids[i] = issue.ID
	}

	b.Run("five_writes_single", func(b *testing.B) {
		for b.Loop() {
			for _, id := range ids {
				if _, err := l.shell.Update(ctx, UpdateRequest{ID: id, Status: "open"}); err != nil {
					b.Fatal(err)
				}
			}
		}
	})
	b.Run("five_writes_batch", func(b *testing.B) {
		for b.Loop() {
			for _, id := range ids {
				if err := l.QueueUpdate(id, BatchFields{Status: "open"}); err != nil {
					b.Fatal(err)
				}
			}
			if err := l.FlushWrites(ctx); err != nil {
				b.Fatal(err)
			}
		}
	})
}
