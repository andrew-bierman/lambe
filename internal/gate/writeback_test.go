package gate

import (
	"os"
	"testing"

	"github.com/andrew-bierman/lambe/internal/tracker"
)

// bdBinForWriteback resolves the bd binary under the LAMBE_TEST_BD gate and
// reports whether the test should run, matching the tracker package's
// convention for bd-backed tests.
func bdBinForWriteback(t *testing.T) (string, bool) {
	t.Helper()
	if os.Getenv("LAMBE_TEST_BD") != "1" {
		t.Skip("bd binary not found; set LAMBE_TEST_BD=1 with bd on PATH to run")
	}
	bin, err := tracker.ResolveBdBin("")
	if err != nil {
		t.Skipf("bd binary not found: %v", err)
	}
	return bin, true
}

// TestNeedsHumanWritesReasonMetadataOnThePrimaryPath pins the authoritative
// reason record: when bd accepts the needs-human status, the escalation
// still stamps the reason metadata, so the served attention view reads the
// real reason from the tracker — the blocked fallback must not be the only
// path that keeps it.
func TestNeedsHumanWritesReasonMetadataOnThePrimaryPath(t *testing.T) {
	bin, ok := bdBinForWriteback(t)
	if !ok {
		return
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
	hub, err := NewBdHub(bin, dir, "lambe@testbox")
	if err != nil {
		t.Fatal(err)
	}
	if err := hub.EnsureNeedsHumanStatus(ctx); err != nil {
		t.Fatalf("configure needs-human status: %v", err)
	}
	issue, err := local.EnsureIssue(ctx, "primary-path-reason", tracker.IssueSpec{
		Title: "primary-path-reason", Priority: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := hub.NeedsHuman(ctx, issue.ID, "completion check failed", "gate output"); err != nil {
		t.Fatalf("needs human: %v", err)
	}
	status, err := hub.IssueStatus(ctx, issue.ID)
	if err != nil {
		t.Fatal(err)
	}
	if status != StatusNeedsHuman {
		t.Fatalf("status = %q, want the needs-human primary path (not the blocked fallback)", status)
	}
	reason, err := hub.Get(ctx, issue.ID, MetaNeedsHumanReason)
	if err != nil {
		t.Fatal(err)
	}
	if reason != "completion check failed" {
		t.Errorf("metadata reason = %q, want the escalation's recorded reason", reason)
	}
	output, err := hub.Get(ctx, issue.ID, MetaLastGateOutput)
	if err != nil {
		t.Fatal(err)
	}
	if output != "gate output" {
		t.Errorf("metadata gate output = %q, want the escalation's recorded output", output)
	}
}

// TestNeedsHumanWritesReasonMetadataOnTheBlockedFallback pins the fallback:
// with the custom status unconfigured, the issue still parks (as blocked)
// and keeps the reason in metadata.
func TestNeedsHumanWritesReasonMetadataOnTheBlockedFallback(t *testing.T) {
	bin, ok := bdBinForWriteback(t)
	if !ok {
		return
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
	hub, err := NewBdHub(bin, dir, "lambe@testbox")
	if err != nil {
		t.Fatal(err)
	}
	issue, err := local.EnsureIssue(ctx, "fallback-reason", tracker.IssueSpec{
		Title: "fallback-reason", Priority: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := hub.NeedsHuman(ctx, issue.ID, "completion check failed", "gate output"); err != nil {
		t.Fatalf("needs human: %v", err)
	}
	status, err := hub.IssueStatus(ctx, issue.ID)
	if err != nil {
		t.Fatal(err)
	}
	if status != tracker.StatusBlocked {
		t.Fatalf("status = %q, want the blocked fallback", status)
	}
	reason, err := hub.Get(ctx, issue.ID, MetaNeedsHumanReason)
	if err != nil {
		t.Fatal(err)
	}
	if reason != "completion check failed" {
		t.Errorf("metadata reason = %q, want the escalation's recorded reason", reason)
	}
	output, err := hub.Get(ctx, issue.ID, MetaLastGateOutput)
	if err != nil {
		t.Fatal(err)
	}
	if output != "gate output" {
		t.Errorf("metadata gate output = %q, want the escalation's recorded output", output)
	}
}
