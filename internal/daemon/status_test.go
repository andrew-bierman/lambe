package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

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
func TestBuildReportGroupsAttemptsInOneRead(t *testing.T) {
	t.Parallel()
	tr, wake := seedReportTracker(t, 50)

	rep, err := BuildReport(context.Background(), tr, nil)
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

	rep, err := BuildReport(context.Background(), tr, nil)
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

	rep, err := BuildReport(ctx, local, nil)
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
