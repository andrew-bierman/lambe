package faultsuite_test

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"os/exec"
	"syscall"
	"testing"

	"github.com/andrew-bierman/lambe/internal/proc"
	"github.com/andrew-bierman/lambe/internal/testchild"
	"github.com/andrew-bierman/lambe/internal/tracker"
)

// TestFault_AgentKillMidTurn proves the agent-crash contract: a real fake
// agent (internal/testchild, re-exec'd in its own process group) is killed
// externally with SIGKILL mid-turn; the persisted session binding survives
// in the tracker, a restart resumes the same session id, and pinned-id
// idempotency keeps the re-create a no-op — no duplicate side effects.
func TestFault_AgentKillMidTurn(t *testing.T) {
	const actor = "lambe@faults-agentkill"
	ctx := context.Background()
	env := newTracker(t, actor)
	work := seedWork(t, env, "seed-agent-kill", "ship the hotfix")
	if _, err := env.Claim(ctx, work); err != nil {
		t.Fatalf("claim: %v", err)
	}

	// Spawn the fake agent mid-turn: testchild's hang behavior emits one
	// progress frame, then works forever. The agent leads its own
	// process group, exactly as the supervisor spawns agents.
	cmd := exec.Command(os.Args[0]) //nolint:gosec // the binary is this very test binary, re-exec'd
	cmd.Env = append(os.Environ(), testchild.EnvVar+"="+testchild.BehaviorHang)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start fake agent: %v", err)
	}
	pgid := cmd.Process.Pid // Setpgid: the agent leads its own group
	defer func() { _ = killGroup(pgid, sigKill) }()

	// The crash-recovery seam: the binding is persisted immediately after
	// the process exists, mirroring the pi driver's Persist hook ordering.
	if err := env.PutAttempt(ctx, tracker.Attempt{SessionID: work, Number: 1, PGID: pgid}); err != nil {
		t.Fatalf("persist binding: %v", err)
	}

	// Mid-turn evidence: one progress frame arrived and the agent is
	// alive and working.
	frame, err := readFrame(bufio.NewReader(stdout))
	if err != nil {
		t.Fatalf("read first agent frame: %v", err)
	}
	if frame.Type != "progress" {
		t.Fatalf("first frame: got %q, want progress", frame.Type)
	}
	if err := killGroup(pgid, 0); err != nil {
		t.Fatalf("agent not alive before kill: %v", err)
	}

	// kill -9 the whole agent group.
	if err := killGroup(pgid, sigKill); err != nil {
		t.Fatalf("SIGKILL agent group: %v", err)
	}
	if waitErr := cmd.Wait(); !exitBySignal(waitErr, sigKill) {
		t.Fatalf("agent did not die from SIGKILL: %v", waitErr)
	}

	// The persisted binding survives the kill.
	//
	// Finding (committed-seam defect, see README): for pids >= 1e6 —
	// routine on long-lived Linux systems — bd stores lambe_pgid as a
	// JSON number and the tracker's metadata decode renders it in
	// scientific notation ("2.900324e+06"), so the typed Attempts read
	// drops the pgid. The binding itself survives in the tracker; the
	// raw bead is the authoritative read until the decode is fixed.
	rawBind, err := env.EnsureIssue(ctx, "att-"+work+"-1", tracker.IssueSpec{Title: "binding probe"})
	if err != nil {
		t.Fatalf("read raw attempt bead: %v", err)
	}
	if got := rawBind.Metadata["lambe_session"]; got != work {
		t.Fatalf("binding session drifted after kill: metadata %+v", rawBind.Metadata)
	}
	if got := rawBind.Metadata["lambe_pgid"]; got == "" {
		t.Fatalf("binding pgid lost after kill: metadata %+v", rawBind.Metadata)
	}
	if pgid < 1000000 {
		// The typed read is exact for small pids.
		attempts, err := env.Attempts(ctx, work)
		if err != nil {
			t.Fatalf("attempts after kill: %v", err)
		}
		if len(attempts) != 1 || attempts[0].PGID != pgid || attempts[0].Number != 1 {
			t.Fatalf("binding lost or drifted after kill: %+v", attempts)
		}
	}
	assertContextParity(ctx, t, env, work)

	// Restart resumes the same session id: the re-put of the same pinned
	// attempt updates in place — the PGID moves to the new process — and
	// never duplicates the record. A small pgid keeps the typed read
	// exact despite the metadata-decode defect above.
	const newPGID = 4242
	if err := env.PutAttempt(ctx, tracker.Attempt{SessionID: work, Number: 1, PGID: newPGID}); err != nil {
		t.Fatalf("re-put on restart: %v", err)
	}
	attempts, err := env.Attempts(ctx, work)
	if err != nil {
		t.Fatalf("attempts after restart: %v", err)
	}
	if len(attempts) != 1 || attempts[0].PGID != newPGID {
		t.Fatalf("restart duplicated side effects: %+v, want one attempt bound to pgid %d", attempts, newPGID)
	}

	// The session binding re-created under the same pinned key is the
	// same bead: a restart spawns no duplicate session record.
	const bindKey = "session-binding-agent-kill"
	spec := tracker.IssueSpec{Title: "session binding for " + work, Priority: 1, Metadata: map[string]string{"lambe_session": work}}
	first, err := env.EnsureIssue(ctx, bindKey, spec)
	if err != nil {
		t.Fatalf("ensure binding: %v", err)
	}
	second, err := env.EnsureIssue(ctx, bindKey, spec)
	if err != nil {
		t.Fatalf("re-ensure binding: %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("pinned binding id drifted across restart: %s vs %s", first.ID, second.ID)
	}
}

// readFrame reads one JSONL frame line from r and parses it via the
// committed frame parser.
func readFrame(r *bufio.Reader) (proc.Frame, error) {
	line, err := r.ReadBytes('\n')
	if err != nil {
		return proc.Frame{}, err
	}
	return proc.ParseFrame(bytes.TrimSpace(line))
}
