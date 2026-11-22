package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andrew-bierman/lambe/internal/gate"
	"github.com/andrew-bierman/lambe/internal/tracker"
)

// seedReportTracker fills tr with nIssues issues split across the statuses
// the report reads, each in-progress issue carrying attempts 1..3. A failed
// per-issue Attempts read would be visible in the report, so the fixture
// also logs those calls for the assertion below.
func seedReportTracker(t *testing.T, nIssues int) (*fakeTracker, time.Time) {
	t.Helper()
	tr := newFakeTracker()
	wake := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	for i := 0; i < nIssues; i++ {
		id := fmt.Sprintf("bulk-%02d", i)
		issue := tracker.Issue{ID: id, Assignee: "lambe@test"}
		switch i % 3 {
		case 0:
			issue.Status = tracker.StatusInProgress
			tr.attempts[id] = []tracker.Attempt{
				{SessionID: id, Number: 1},
				{SessionID: id, Number: 2, PGID: 4242},
				{SessionID: id, Number: 3},
			}
		case 1:
			issue.Status = tracker.StatusOpen
			tr.attempts[id] = []tracker.Attempt{{SessionID: id, Number: 1, WakeAt: wake}}
		default:
			issue.Status = tracker.StatusBlocked
		}
		tr.issues[id] = issue
	}
	return tr, wake
}

// TestBuildReportGroupsAttemptsInOneRead is the perf contract: a report over
// many issues must answer every attempt join from the single grouped
// ListByMetadata read, never one Attempts query per issue.
// readinessCountingTracker adds a readiness source over a fakeTracker and
// counts both reads, so tests can pin the canonical queue read's cost.
type readinessCountingTracker struct {
	tracker.Tracker
	rows           []tracker.ReadinessRow
	readinessErr   error
	mu             sync.Mutex
	readinessCalls int
	idReads        int
}

func (t *readinessCountingTracker) Readiness(context.Context, []string, []string) ([]tracker.ReadinessRow, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.readinessCalls++
	if t.readinessErr != nil {
		return nil, t.readinessErr
	}
	return t.rows, nil
}

func (t *readinessCountingTracker) Dispatchable(ctx context.Context, q tracker.DispatchQuery) ([]tracker.Issue, error) {
	t.mu.Lock()
	if len(q.IDs) > 0 {
		t.idReads++
	}
	t.mu.Unlock()
	return t.Tracker.Dispatchable(ctx, q)
}

func TestBuildReportNeedsHumanReasonAndSince(t *testing.T) {
	t.Parallel()
	tr := newFakeTracker()
	updated := time.Now().Add(-30 * time.Minute).UTC().Truncate(time.Second)
	// The primary path's record shape: the escalation reason stamped in
	// metadata alongside the parked status.
	tr.issues["meta-reason"] = tracker.Issue{
		ID:        "meta-reason",
		Status:    gate.StatusNeedsHuman,
		UpdatedAt: updated,
		Metadata: map[string]string{
			gate.MetaNeedsHumanReason: "completion check failed",
			"lambe_last_gate_output":  "gate output blob\nmore lines",
		},
	}
	// The fallback shape: no recorded reason, only gate output.
	tr.issues["fallback"] = tracker.Issue{
		ID:        "fallback",
		Status:    tracker.StatusBlocked,
		UpdatedAt: updated,
		Metadata:  map[string]string{"lambe_last_gate_output": "gh:pr rejected\nfull log follows"},
	}

	rep, err := BuildReport(context.Background(), tr, nil, nil)
	if err != nil {
		t.Fatalf("build report: %v", err)
	}
	byID := map[string]NeedsHumanItem{}
	for _, item := range rep.NeedsHuman {
		byID[item.Issue] = item
	}
	meta, ok := byID["meta-reason"]
	if !ok {
		t.Fatalf("primary-path item missing: %+v", rep.NeedsHuman)
	}
	// Authoritative record first, gate summary only as detail.
	if meta.Reason != "completion check failed" {
		t.Errorf("primary-path reason = %q, want the recorded escalation reason", meta.Reason)
	}
	if meta.LastGateOutput != "gate output blob" {
		t.Errorf("primary-path gate summary = %q", meta.LastGateOutput)
	}
	if !meta.Since.Equal(updated) {
		t.Errorf("primary-path since = %s, want the record's update time %s", meta.Since, updated)
	}
	fallback, ok := byID["fallback"]
	if !ok {
		t.Fatalf("fallback item missing: %+v", rep.NeedsHuman)
	}
	// Without metadata the reason degrades to the gate-output summary —
	// never a hardcoded constant.
	if fallback.Reason != "gh:pr rejected" {
		t.Errorf("fallback reason = %q, want the gate-output summary", fallback.Reason)
	}
	if !fallback.Since.Equal(updated) {
		t.Errorf("fallback since = %s, want the record's update time", fallback.Since)
	}
}

// TestBuildReportQueueReadsTrackerOnce pins the queue's cost: one report
// build answers the whole readiness projection with one readiness read and
// one grouped label read, and the served rows are lane-valid with
// ambiguity never presented as dispatchable.
func TestBuildReportQueueReadsTrackerOnce(t *testing.T) {
	t.Parallel()
	fake := newFakeTracker()
	qt := &readinessCountingTracker{
		Tracker: fake,
		rows: []tracker.ReadinessRow{
			{ID: "ready-1", Title: "ready task", Reason: "no blockers"},
			{
				ID: "blocked-1", Title: "blocked task", Reason: "waiting on api", Blocked: true,
				Blockers: []struct{ ID, Title string }{{ID: "dep-1", Title: "api migration"}},
			},
			{ID: "ambiguous", Title: "tagged twice"},
			{ID: "stray", Title: "outside every lane"},
		},
	}
	// The label records the post-filter verifies against; the readiness
	// rows above deliberately include issues the filters should have
	// excluded, mirroring the observed bd behavior.
	fake.issues["ready-1"] = tracker.Issue{ID: "ready-1", Labels: []string{"lane-a"}}
	fake.issues["blocked-1"] = tracker.Issue{ID: "blocked-1", Labels: []string{"lane-a"}, BlockedBy: []tracker.BlockerRef{{ID: "dep-1", Identifier: "dep-1"}}}
	fake.issues["ambiguous"] = tracker.Issue{ID: "ambiguous", Labels: []string{"lane-a", "lane-b"}}
	fake.issues["stray"] = tracker.Issue{ID: "stray", Labels: []string{"unrelated"}}

	rep, err := BuildReport(context.Background(), qt, nil, []string{"lane-a", "lane-b"})
	if err != nil {
		t.Fatalf("build report: %v", err)
	}
	if qt.readinessCalls != 1 || qt.idReads != 1 {
		t.Fatalf("tracker reads = (readiness %d, ids %d), want (1, 1)", qt.readinessCalls, qt.idReads)
	}
	byID := map[string]tracker.ReadinessRow{}
	for _, row := range rep.Queue {
		byID[row.ID] = row
	}
	if len(rep.Queue) != 3 {
		t.Fatalf("queue rows = %+v, want every lane-valid row", rep.Queue)
	}
	if row := byID["ready-1"]; row.Blocked {
		t.Errorf("ready row = %+v", row)
	}
	if row := byID["blocked-1"]; !row.Blocked || len(row.Blockers) != 1 || row.Blockers[0].ID != "dep-1" {
		t.Errorf("blocked row = %+v, want blockers carried", row)
	}
	if row := byID["ambiguous"]; !row.Blocked || row.Reason == "" {
		t.Errorf("ambiguous row = %+v, want blocked with a reason", row)
	}
	if _, ok := byID["stray"]; ok {
		t.Errorf("row outside every configured lane served")
	}
}

func TestBuildReportQueueEmptyTracker(t *testing.T) {
	t.Parallel()
	qt := &readinessCountingTracker{Tracker: newFakeTracker()}
	rep, err := BuildReport(context.Background(), qt, nil, nil)
	if err != nil {
		t.Fatalf("build report: %v", err)
	}
	if len(rep.Queue) != 0 {
		t.Errorf("queue = %+v, want an empty list, not an error", rep.Queue)
	}
}

func TestBuildReportGroupsAttemptsInOneRead(t *testing.T) {
	t.Parallel()
	tr, wake := seedReportTracker(t, 50)

	rep, err := BuildReport(context.Background(), tr, nil, nil)
	if err != nil {
		t.Fatalf("build report: %v", err)
	}

	attemptReads, metadataReads := tr.readCounts()
	if attemptReads != 0 {
		t.Errorf("per-issue Attempts calls = %d, want 0 (attempts must come from the grouped read)", attemptReads)
	}
	if metadataReads != 1 {
		t.Errorf("ListByMetadata calls = %d, want 1", metadataReads)
	}
	// A tracker without a readiness source serves no queue rows.
	if len(rep.Queue) != 0 {
		t.Errorf("queue rows = %+v, want none without a readiness source", rep.Queue)
	}

	if len(rep.Active) != 17 {
		t.Fatalf("active runs = %d, want 17", len(rep.Active))
	}
	for _, run := range rep.Active {
		if run.Attempt != 3 {
			t.Errorf("active run %s attempt = %d, want newest attempt 3", run.Issue, run.Attempt)
		}
	}
	if len(rep.Deferred) != 17 {
		t.Errorf("deferred runs = %d, want 17", len(rep.Deferred))
	}
	for _, d := range rep.Deferred {
		if !d.WakeAt.Equal(wake) {
			t.Errorf("deferred %s wake = %s, want %s", d.Issue, d.WakeAt, wake)
		}
	}
	if len(rep.NeedsHuman) != 16 {
		t.Errorf("needs human = %d, want 16", len(rep.NeedsHuman))
	}
}

// TestBuildReportDegradesWhenAttemptReadFails pins the degrade path: an
// unreadable attempt set leaves the report renderable with zero attempts and
// no wake timers, and still costs exactly one query.
func TestBuildReportDegradesWhenAttemptReadFails(t *testing.T) {
	t.Parallel()
	tr, _ := seedReportTracker(t, 6)
	tr.metadataErr["lambe_kind=attempt"] = errors.New("bd unreachable")

	rep, err := BuildReport(context.Background(), tr, nil, nil)
	if err != nil {
		t.Fatalf("build report: %v", err)
	}
	if attemptReads, metadataReads := tr.readCounts(); attemptReads != 0 || metadataReads != 1 {
		t.Errorf("reads = (attempts %d, metadata %d), want (0, 1)", attemptReads, metadataReads)
	}
	if len(rep.Active) != 2 {
		t.Fatalf("active runs = %d, want 2", len(rep.Active))
	}
	for _, run := range rep.Active {
		if run.Attempt != 0 {
			t.Errorf("active run %s attempt = %d, want 0 without attempt state", run.Issue, run.Attempt)
		}
	}
	if len(rep.Deferred) != 0 {
		t.Errorf("deferred = %+v, want none without attempt state", rep.Deferred)
	}
}

type parkedReadFailure struct {
	tracker.Tracker
	err error
}

func (tr parkedReadFailure) Dispatchable(ctx context.Context, query tracker.DispatchQuery) ([]tracker.Issue, error) {
	if len(query.Statuses) > 0 && query.Statuses[0] == tracker.StatusBlocked {
		return nil, tr.err
	}
	return tr.Tracker.Dispatchable(ctx, query)
}

func TestBuildReportPropagatesParkedReadFailure(t *testing.T) {
	t.Parallel()
	tr := parkedReadFailure{
		Tracker: newFakeTracker(),
		err:     &tracker.ExitError{Code: 1, Stderr: "Error: database unavailable\n"},
	}
	if _, err := BuildReport(t.Context(), tr, nil, nil); !errors.Is(err, tr.err) {
		t.Fatalf("BuildReport error = %v, want %v", err, tr.err)
	}
}

func TestBuildReportListsEscalationsAgainstBdTracker(t *testing.T) {
	bin := bdBin()
	if bin == "" {
		t.Skip("bd binary not found; set LAMBE_TEST_BD=1 with bd on PATH to run")
	}
	ctx := t.Context()
	dir := t.TempDir()
	local, err := tracker.NewLocal(bin, dir, "lambe@testbox")
	if err != nil {
		t.Fatal(err)
	}
	if err := local.Init(ctx); err != nil {
		t.Fatal(err)
	}
	hub, err := gate.NewBdHub(bin, dir, "lambe@testbox")
	if err != nil {
		t.Fatal(err)
	}

	park := func(title, label, status string) string {
		t.Helper()
		issue, err := local.EnsureIssue(ctx, title, tracker.IssueSpec{Title: title, Priority: 2, Labels: []string{label}})
		if err != nil {
			t.Fatal(err)
		}
		if err := hub.NeedsHuman(ctx, issue.ID, "completion check failed", "test output"); err != nil {
			t.Fatal(err)
		}
		rows, err := local.Dispatchable(ctx, tracker.DispatchQuery{IDs: []string{issue.ID}})
		if err != nil || len(rows) != 1 || rows[0].Status != status {
			t.Fatalf("parked issue = %+v, err = %v, want status %q", rows, err, status)
		}
		return issue.ID
	}
	assertParked := func(want ...string) {
		t.Helper()
		rep, err := BuildReport(ctx, local, []string{"supervised"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		got := make(map[string]bool)
		for _, issue := range rep.NeedsHuman {
			got[issue.Issue] = true
		}
		if len(rep.NeedsHuman) != len(want) {
			t.Fatalf("needs human = %+v, want %v", rep.NeedsHuman, want)
		}
		for _, id := range want {
			if !got[id] {
				t.Errorf("needs human lacks %s: %+v", id, rep.NeedsHuman)
			}
		}
	}
	_, err = local.Dispatchable(ctx, tracker.DispatchQuery{Statuses: []string{tracker.StatusBlocked, gate.StatusNeedsHuman}})
	var exitErr *tracker.ExitError
	if !errors.As(err, &exitErr) || !strings.Contains(exitErr.Stderr, `Error: invalid status "needs-human" in multi-status filter`) {
		t.Fatalf("unconfigured status error = %v, want wrapped native invalid-status error on stderr", err)
	}

	fallback := park("status-fallback", "supervised", tracker.StatusBlocked)
	assertParked(fallback)
	if err := hub.EnsureNeedsHumanStatus(ctx); err != nil {
		t.Fatal(err)
	}
	custom := park("status-custom", "supervised", gate.StatusNeedsHuman)
	park("status-unrelated", "unrelated", gate.StatusNeedsHuman)
	assertParked(fallback, custom)

	// Every parked item — the blocked fallback and the custom-status
	// primary path alike — carries the recorded escalation reason and a
	// real wait time, so the served attention view is truthful.
	rep, err := BuildReport(ctx, local, []string{"supervised"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, item := range rep.NeedsHuman {
		got[item.Issue] = true
		if item.Reason != "completion check failed" {
			t.Errorf("parked %s reason = %q, want the recorded escalation reason", item.Issue, item.Reason)
		}
		if item.Since.IsZero() {
			t.Errorf("parked %s carries no wait time", item.Issue)
		}
	}
	for _, id := range []string{fallback, custom} {
		if !got[id] {
			t.Errorf("report lacks parked item %s", id)
		}
	}
}

func TestRenderReportMarksGateHistoryUnavailable(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	if err := RenderReport(&out, Report{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "last-night gate rejections:\n  (history unavailable)\n") {
		t.Fatalf("gate history not marked unavailable:\n%s", &out)
	}
}

// TestBuildReportBulkAttemptsAgainstBdTracker proves the grouped read
// against the real CLI: twenty seeded attempt records surface in one report
// without a query per record.
func TestBuildReportBulkAttemptsAgainstBdTracker(t *testing.T) {
	bin := bdBin()
	if bin == "" {
		t.Skip("bd binary not found; set LAMBE_TEST_BD=1 with bd on PATH to run")
	}

	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	local, err := tracker.NewLocal(bin, dir, "lambe@testbox")
	if err != nil {
		t.Fatalf("new local: %v", err)
	}
	if err := local.Init(ctx); err != nil {
		t.Fatalf("init: %v", err)
	}

	wake := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	keys := []string{"bd-bulk-run", "bd-bulk-later", "bd-bulk-idle-1", "bd-bulk-idle-2"}
	ids := make(map[string]string, len(keys))
	for _, key := range keys {
		issue, err := local.EnsureIssue(ctx, key, tracker.IssueSpec{Title: key, Priority: 2})
		if err != nil {
			t.Fatalf("ensure %s: %v", key, err)
		}
		ids[key] = issue.ID
		for n := 1; n <= 5; n++ {
			a := tracker.Attempt{SessionID: issue.ID, Number: n, PGID: 1000 + n}
			if key == "bd-bulk-later" && n == 5 {
				a.WakeAt = wake
			}
			if err := local.PutAttempt(ctx, a); err != nil {
				t.Fatalf("put attempt %s/%d: %v", issue.ID, n, err)
			}
		}
	}
	if _, err := local.Claim(ctx, ids["bd-bulk-run"]); err != nil {
		t.Fatalf("claim: %v", err)
	}

	rep, err := BuildReport(ctx, local, nil, nil)
	if err != nil {
		t.Fatalf("build report: %v", err)
	}
	runID, laterID := ids["bd-bulk-run"], ids["bd-bulk-later"]
	if len(rep.Active) != 1 || rep.Active[0].Issue != runID || rep.Active[0].Attempt != 5 {
		t.Fatalf("active = %+v, want %s at attempt 5", rep.Active, runID)
	}
	if len(rep.Deferred) != 1 || rep.Deferred[0].Issue != laterID || !rep.Deferred[0].WakeAt.Equal(wake) {
		t.Fatalf("deferred = %+v, want %s waking at %s", rep.Deferred, laterID, wake)
	}
	for _, nh := range rep.NeedsHuman {
		if nh.Issue == runID || nh.Issue == laterID {
			t.Errorf("attempt record %q leaked into needs-human", nh.Issue)
		}
	}
}

func TestResolveStateDirReportsItsSource(t *testing.T) {
	t.Setenv("HOME", "/home/x")
	t.Setenv("LAMBE_STATE_DIR", "")
	if dir, src := ResolveStateDir(); dir != "/home/x/.local/state/lambe" || src != StateDirDefault {
		t.Errorf("default = %q, %q", dir, src)
	}
	t.Setenv("LAMBE_STATE_DIR", "/override")
	if dir, src := ResolveStateDir(); dir != "/override" || src != StateDirEnv || DefaultStateDir() != "/override" {
		t.Errorf("override = %q, %q", dir, src)
	}
}
