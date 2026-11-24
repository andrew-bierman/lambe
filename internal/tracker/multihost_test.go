package tracker_test

import (
	"errors"
	"sync"
	"testing"

	"github.com/andrew-bierman/lambe/internal/testdolt"
	"github.com/andrew-bierman/lambe/internal/tracker"
)

// twoHosts returns trackers for two simulated hosts that share one Dolt
// server and database but write as distinct actors, plus a helper that
// creates an open issue.
func twoHosts(t *testing.T, srv *testdolt.Server) (a, b *tracker.Beads, dirA, dirB string, mk func(title string) string) {
	t.Helper()
	dirA = srv.Workspace(t, t.Name()+"-a")
	dirB = srv.Workspace(t, t.Name()+"-b")
	a = tracker.NewBeads(tracker.NewShellTransport(srv.Bd, dirA, "host-a"), "host-a")
	b = tracker.NewBeads(tracker.NewShellTransport(srv.Bd, dirB, "host-b"), "host-b")
	mk = func(title string) string {
		t.Helper()
		issue, err := a.EnsureIssue(t.Context(), "k-"+title, tracker.IssueSpec{Title: title, Priority: 2})
		if err != nil {
			t.Fatalf("seed %s: %v", title, err)
		}
		return issue.ID
	}
	return
}

func testContestedClaimHasOneWinner(t *testing.T, srv *testdolt.Server) {
	t.Helper()
	a, b, _, _, mk := twoHosts(t, srv)
	const rounds = 15
	for i := range rounds {
		id := mk("race-" + string(rune('a'+i)))
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for j, h := range []*tracker.Beads{a, b} {
			wg.Add(1)
			go func() { defer wg.Done(); _, errs[j] = h.Claim(t.Context(), id) }()
		}
		wg.Wait()
		wins := 0
		for _, err := range errs {
			switch {
			case err == nil:
				wins++
			case !errors.Is(err, tracker.ErrLostRace):
				t.Fatalf("round %d: loser got %v, want ErrLostRace", i, err)
			}
		}
		if wins != 1 {
			t.Fatalf("round %d: %d winners (a=%v b=%v), want exactly 1", i, wins, errs[0], errs[1])
		}
	}
}

func testHealthyLeaseSurvivesPeerReclaim(t *testing.T, srv *testdolt.Server) {
	t.Helper()
	a, b, _, _, mk := twoHosts(t, srv)
	id := mk("healthy")
	if _, err := a.Claim(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if err := a.Heartbeat(t.Context(), id); err != nil {
		t.Fatalf("owner heartbeat: %v", err)
	}
	if err := b.Reclaim(t.Context(), 0, nil); err != nil {
		t.Fatalf("peer reclaim: %v", err)
	}
	if err := b.Heartbeat(t.Context(), id); !errors.Is(err, tracker.ErrNotOwner) {
		t.Fatalf("peer heartbeat on a live claim: got %v, want ErrNotOwner", err)
	}
	if err := a.Heartbeat(t.Context(), id); err != nil {
		t.Fatalf("owner lost its healthy lease to a peer reclaim: %v", err)
	}
}

func testPeerRecoversExpiredLease(t *testing.T, srv *testdolt.Server) {
	t.Helper()
	a, b, _, dirB, mk := twoHosts(t, srv)
	id := mk("orphan")
	if _, err := a.Claim(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	// Host A dies: no more heartbeats; its lease ages out.
	srv.ExpireLease(t, dirB, id)
	if err := b.Reclaim(t.Context(), 0, []string{id}); err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	got, err := b.Claim(t.Context(), id)
	if err != nil {
		t.Fatalf("peer claim after reclaim: %v", err)
	}
	if got.Assignee != "host-b" || got.Status != tracker.StatusInProgress {
		t.Fatalf("after takeover: assignee=%q status=%q", got.Assignee, got.Status)
	}
	// The recovered original owner learns it lost the claim: the stop signal.
	if err := a.Heartbeat(t.Context(), id); !errors.Is(err, tracker.ErrNotOwner) {
		t.Fatalf("dead owner heartbeat: got %v, want ErrNotOwner", err)
	}
	if err := a.Release(t.Context(), id); !errors.Is(err, tracker.ErrNotOwner) {
		t.Fatalf("dead owner release: got %v, want ErrNotOwner", err)
	}
}

// TestMultiHost shares one isolated Dolt server across its subtests; each
// subtest gets its own pair of host workspaces (server start is the slow part).
func TestMultiHost(t *testing.T) {
	srv := testdolt.Start(t)
	t.Run("ContestedClaimHasOneWinner", func(t *testing.T) { testContestedClaimHasOneWinner(t, srv) })
	t.Run("HealthyLeaseSurvivesPeerReclaim", func(t *testing.T) { testHealthyLeaseSurvivesPeerReclaim(t, srv) })
	t.Run("PeerRecoversExpiredLease", func(t *testing.T) { testPeerRecoversExpiredLease(t, srv) })
}
