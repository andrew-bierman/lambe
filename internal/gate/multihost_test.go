package gate_test

import (
	"strings"
	"testing"

	"github.com/andrew-bierman/lambe/internal/gate"
	"github.com/andrew-bierman/lambe/internal/symphony"
	"github.com/andrew-bierman/lambe/internal/testdolt"
	"github.com/andrew-bierman/lambe/internal/tracker"
)

// TestMultiHostLostOwnerCannotPublishCompletion proves fence 4: host A's
// worker finishes a green gate after host B took the claim over, and the
// completion is refused. The issue stays B's, in_progress, not tombstoned.
func TestMultiHostLostOwnerCannotPublishCompletion(t *testing.T) {
	srv := testdolt.Start(t)
	for _, tc := range []struct {
		name       string
		peerClaims bool // B re-claims after the reclaim, vs the issue sitting open
		wantOwner  string
		wantStatus string
	}{
		{"peer took over", true, "host-b", "in_progress"},
		{"reclaimed and unowned", false, "", "open"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dirA := srv.Workspace(t, tc.wantStatus+"-a")
			dirB := srv.Workspace(t, tc.wantStatus+"-b")
			hubA, err := gate.NewBdHub(srv.Bd, dirA, "host-a")
			if err != nil {
				t.Fatal(err)
			}
			a := tracker.NewBeads(tracker.NewShellTransport(srv.Bd, dirA, "host-a"), "host-a")
			b := tracker.NewBeads(tracker.NewShellTransport(srv.Bd, dirB, "host-b"), "host-b")

			issue, err := a.EnsureIssue(t.Context(), "k-fence-"+tc.wantStatus, tracker.IssueSpec{Title: "fence", Priority: 2})
			if err != nil {
				t.Fatal(err)
			}
			id := issue.ID
			if _, err := a.Claim(t.Context(), id); err != nil {
				t.Fatal(err)
			}
			// Host A stalls (partition, laptop sleep); its lease ages out and
			// host B reclaims, optionally taking the work.
			srv.ExpireLease(t, dirB, id)
			if err := b.Reclaim(t.Context(), 0, []string{id}); err != nil {
				t.Fatal(err)
			}
			if tc.peerClaims {
				if _, err := b.Claim(t.Context(), id); err != nil {
					t.Fatal(err)
				}
			}

			formulas := t.TempDir()
			writeFormula(t, formulas, "tests", "type = \"shell\"\ncommand = \"echo green\"")
			runner := gate.NewRunner(formulas, hubA, gate.NewEscalator(hubA), "host-a", discardLog())
			runner.Exec = gate.OsExec
			// Host A's still-running worker reports a green gate.
			out, err := runner.Evaluate(t.Context(), symphony.GateRequest{
				Issue:     tracker.Issue{ID: id, Title: "fence", Status: "in_progress"},
				Attempt:   1,
				Workspace: t.TempDir(),
				Revision:  symphony.Revision("wf-fence"),
			})
			if err != nil {
				t.Fatal(err)
			}
			if out.Verdict == symphony.GateGreen {
				t.Fatalf("stale owner published a green completion: %+v", out)
			}
			show := srv.Bdx(t, dirB, "host-b", "show", id, "--json")
			if tc.wantOwner != "" && !strings.Contains(show, `"assignee": "`+tc.wantOwner+`"`) {
				t.Errorf("owner changed: %s", show)
			}
			if !strings.Contains(show, `"status": "`+tc.wantStatus+`"`) {
				t.Errorf("status changed, want %s: %s", tc.wantStatus, show)
			}
			if ok, err := hubA.IsTombstoned(t.Context(), id); err != nil || ok {
				t.Errorf("stale owner tombstoned the issue (ok=%v err=%v)", ok, err)
			}
		})
	}
}
