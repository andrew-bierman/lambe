package gate_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andrew-bierman/lambe/internal/gate"
	"github.com/andrew-bierman/lambe/internal/symphony"
	"github.com/andrew-bierman/lambe/internal/tracker"
)

// discardLog returns a logger that drops everything.
func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// ---- fakes ----

type closeCall struct {
	issue, ifAssignee, reason string
}

type deferCall struct {
	issue  string
	until  time.Time
	reason string
}

type needsHumanCall struct {
	issue, reason, lastOutput string
}

type commentCall struct {
	issue, body string
}

// fakeHub records every writeback the gate layer performs.
type fakeHub struct {
	mu         sync.Mutex
	comments   []commentCall
	closes     []closeCall
	defers     []deferCall
	needHuman  []needsHumanCall
	tombstones map[string]bool

	closeErr   error
	commentErr error
	// statuses is the IssueStatus answer per issue; an absent issue
	// reports "" (not terminal).
	statuses map[string]tracker.Status
	// statusErr injects an IssueStatus read failure.
	statusErr error
}

func newFakeHub() *fakeHub {
	return &fakeHub{tombstones: map[string]bool{}, statuses: map[string]tracker.Status{}}
}

func (h *fakeHub) Comment(_ context.Context, issue, body string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.commentErr != nil {
		return h.commentErr
	}
	h.comments = append(h.comments, commentCall{issue: issue, body: body})
	return nil
}

func (h *fakeHub) Close(_ context.Context, issue, ifAssignee, reason string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closeErr != nil {
		return h.closeErr
	}
	h.closes = append(h.closes, closeCall{issue: issue, ifAssignee: ifAssignee, reason: reason})
	return nil
}

func (h *fakeHub) Defer(_ context.Context, issue string, until time.Time, reason string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.defers = append(h.defers, deferCall{issue: issue, until: until, reason: reason})
	return nil
}

func (h *fakeHub) NeedsHuman(_ context.Context, issue, reason, lastOutput string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.needHuman = append(h.needHuman, needsHumanCall{issue: issue, reason: reason, lastOutput: lastOutput})
	return nil
}

func (h *fakeHub) Tombstone(_ context.Context, issue string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.tombstones[issue] = true
	return nil
}

func (h *fakeHub) IsTombstoned(_ context.Context, issue string) (bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.tombstones[issue], nil
}

func (h *fakeHub) IssueStatus(_ context.Context, issue string) (tracker.Status, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.statusErr != nil {
		return "", h.statusErr
	}
	return h.statuses[issue], nil
}

// memKV is the in-memory KV persistence for escalation counters.
type memKV struct {
	mu   sync.Mutex
	data map[string]map[string]string
}

func newMemKV() *memKV { return &memKV{data: map[string]map[string]string{}} }

func (m *memKV) Get(ctx context.Context, issue, key string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.data[issue][key], nil
}

func (m *memKV) Put(ctx context.Context, issue, key, value string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.data[issue] == nil {
		m.data[issue] = map[string]string{}
	}
	m.data[issue][key] = value
	return nil
}

type execCall struct {
	dir  string
	argv []string
}

// fakeExec records invocations and delegates to fn.
type fakeExec struct {
	mu    sync.Mutex
	calls []execCall
	fn    func(dir string, argv []string) (string, error)
}

func (e *fakeExec) exec(_ context.Context, dir string, argv ...string) (string, error) {
	e.mu.Lock()
	e.calls = append(e.calls, execCall{dir: dir, argv: argv})
	e.mu.Unlock()
	if e.fn == nil {
		return "fake ok", nil
	}
	return e.fn(dir, argv)
}

// fullSha is a stand-in full commit SHA for gate refs in tests.
const fullSha = "0123456789abcdef0123456789abcdef01234567"

// fakeGH pins one GitHub-side gate condition.
type fakeGH struct {
	mu      sync.Mutex
	result  gate.GHResult
	err     error
	head    string
	headErr error
	runRef  string
}

func (g *fakeGH) CheckRun(_ context.Context, ref string) (gate.GHResult, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.runRef = ref
	return g.result, g.err
}

func (g *fakeGH) CheckPR(context.Context, string) (gate.GHResult, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.result, g.err
}

func (g *fakeGH) ResolvePRHead(context.Context, string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.head == "" {
		return fullSha, g.headErr
	}
	return g.head, g.headErr
}

// ---- helpers ----

// writeFormula writes one formula file with the given gate TOML body.
func writeFormula(t *testing.T, dir, name, gateTOML string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name+".formula.toml")
	body := "[trigger]\ncommand = \"true\"\n\n[gate]\n" + gateTOML
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func testIssue() tracker.Issue {
	return tracker.Issue{ID: "bd-gate-1", Title: "keep tests green", Status: "in_progress"}
}

func gateReq(issue tracker.Issue, attempt int, rev symphony.Revision) symphony.GateRequest {
	return symphony.GateRequest{Issue: issue, Attempt: attempt, Revision: rev, Output: "agent claims done"}
}

func newTestRunner(t *testing.T, dir string, hub *fakeHub, kv *memKV, execFn gate.ExecFunc, gh gate.GHChecker) *gate.Runner {
	t.Helper()
	r := gate.NewRunner(dir, hub, gate.NewEscalator(kv), "lambe@test", discardLog())
	if execFn != nil {
		r.Exec = execFn
	}
	if gh != nil {
		r.Gh = gh
	}
	return r
}

// reqWithWorkspace builds a GateRequest with a real temporary workspace.
func reqWithWorkspace(t *testing.T, attempt int, rev symphony.Revision) symphony.GateRequest {
	t.Helper()
	req := gateReq(testIssue(), attempt, rev)
	req.Workspace = t.TempDir()
	return req
}

// ---- parser ----

func TestParseSpecFile(t *testing.T) {
	t.Run("shell gate", func(t *testing.T) {
		dir := t.TempDir()
		path := writeFormula(t, dir, "unit", "type = \"shell\"\ncommand = \"go test ./...\"")
		spec, err := gate.ParseSpecFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if spec.Config.Kind != gate.KindShell || spec.Config.Command != "go test ./..." {
			t.Fatalf("unexpected config %+v", spec.Config)
		}
		if spec.Name != "unit" || spec.Revision == "" {
			t.Fatalf("name/revision not populated: %+v", spec)
		}
	})

	t.Run("gh run gate accepts full sha, run id, or a pr", func(t *testing.T) {
		cases := []struct {
			name, key, value string
			wantRef          string
		}{
			{"ref full sha", "ref", fullSha, fullSha},
			{"ref run id", "ref", "12345", "12345"},
			{"run_id", "run_id", "12345", "12345"},
			{"pr only", "pr", "17", ""},
		}
		for _, tc := range cases {
			dir := t.TempDir()
			path := writeFormula(t, dir, "ci", fmt.Sprintf("type = \"gh:run\"\n%s = %q", tc.key, tc.value))
			spec, err := gate.ParseSpecFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if spec.Config.Kind != gate.KindGhRun || spec.Config.Ref != tc.wantRef {
				t.Fatalf("unexpected config for %s: %+v", tc.name, spec.Config)
			}
		}
	})

	t.Run("gh run gate rejects bare branch refs", func(t *testing.T) {
		for _, ref := range []string{"main", "refs/heads/main", "feature/x", "deadbeef"} {
			dir := t.TempDir()
			path := writeFormula(t, dir, "ci", fmt.Sprintf("type = \"gh:run\"\nref = %q", ref))
			_, err := gate.ParseSpecFile(path)
			if err == nil {
				t.Fatalf("ref %q must be rejected", ref)
			}
			for _, want := range []string{"bare branch-name ref", `pr = "<number-or-branch>"`, "full-commit-sha"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("ref %q error must mention %q, got: %v", ref, want, err)
				}
			}
		}
	})

	t.Run("gh pr gate", func(t *testing.T) {
		dir := t.TempDir()
		path := writeFormula(t, dir, "pr", "type = \"gh:pr\"\npr = \"17\"")
		spec, err := gate.ParseSpecFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if spec.Config.Kind != gate.KindGhPr || spec.Config.PR != "17" {
			t.Fatalf("unexpected config %+v", spec.Config)
		}
	})

	t.Run("invalid gates are errors", func(t *testing.T) {
		cases := []struct {
			name, toml, want string
		}{
			{"unknown type", "type = \"noise\"", "unknown gate type"},
			{"shell without command", "type = \"shell\"", "command is required"},
			{"gh run without ref or pr", "type = \"gh:run\"", "ref is required"},
			{"gh pr without pr", "type = \"gh:pr\"", "pr is required"},
			{"missing type", "command = \"true\"", "type is required"},
		}
		for _, tc := range cases {
			dir := t.TempDir()
			path := writeFormula(t, dir, "bad", tc.toml)
			if _, err := gate.ParseSpecFile(path); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("%s: got %v, want error containing %q", tc.name, err, tc.want)
			}
		}
	})

	t.Run("revision changes when the formula is edited", func(t *testing.T) {
		dir := t.TempDir()
		path := writeFormula(t, dir, "unit", "type = \"shell\"\ncommand = \"exit 0\"")
		before, err := gate.ParseSpecFile(path)
		if err != nil {
			t.Fatal(err)
		}
		writeFormula(t, dir, "unit", "type = \"shell\"\ncommand = \"exit 1\"")
		after, err := gate.ParseSpecFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if before.Revision == after.Revision {
			t.Fatal("revision must change when formula content changes")
		}
	})
}

func TestLoadDirSkipsFormulasWithoutGate(t *testing.T) {
	dir := t.TempDir()
	writeFormula(t, dir, "gated", "type = \"shell\"\ncommand = \"true\"")
	if err := os.WriteFile(filepath.Join(dir, "plain.formula.toml"), []byte("[trigger]\ncommand = \"true\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	specs, err := gate.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 1 || specs[0].Name != "gated" {
		t.Fatalf("want only the gated formula, got %+v", specs)
	}
}

// ---- three-valued evaluation ----

func TestEvaluateShellExportsIssueID(t *testing.T) {
	t.Setenv("LAMBE_ISSUE_ID", "stale-issue")
	dir := t.TempDir()
	command := `printf '%s|%s|' "$0" "$#"; sh -c 'printf "<%s>" "$LAMBE_ISSUE_ID"'; exit 1`
	writeFormula(t, dir, "tests", fmt.Sprintf("type = %q\ncommand = %q", "shell", command))
	r := newTestRunner(t, dir, newFakeHub(), newMemKV(), gate.OsExec, nil)
	req := reqWithWorkspace(t, 1, symphony.Revision("wf-1"))
	req.Issue.ID = "-ac 'quoted'; $(printf INJECTED)\n$HOME & 日本語"

	outcome, err := r.Evaluate(t.Context(), req)
	if want := "sh|0|<" + req.Issue.ID + ">"; err != nil || outcome.Verdict != symphony.GateRed || outcome.Output != want {
		t.Fatalf("shell gate got (%+v, %v), want red with output %q", outcome, err, want)
	}
}

func TestEvaluateShellRedWritesRejectionComment(t *testing.T) {
	dir := t.TempDir()
	writeFormula(t, dir, "tests", "type = \"shell\"\ncommand = \"echo boom-failing-tests >&2; exit 3\"")
	hub, kv := newFakeHub(), newMemKV()
	r := newTestRunner(t, dir, hub, kv, gate.OsExec, nil)

	outcome, err := r.Evaluate(context.Background(), reqWithWorkspace(t, 1, symphony.Revision("wf-1")))
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Verdict != symphony.GateRed {
		t.Fatalf("verdict = %d, want red", outcome.Verdict)
	}
	if !strings.Contains(outcome.Output, "boom-failing-tests") {
		t.Fatalf("gate output must carry the failing command output, got %q", outcome.Output)
	}
	if len(hub.comments) != 1 || !strings.Contains(hub.comments[0].body, "boom-failing-tests") {
		t.Fatalf("rejection comment missing on issue: %+v", hub.comments)
	}
	if len(hub.closes) != 0 || len(hub.needHuman) != 0 {
		t.Fatalf("red must not close or escalate: %+v %+v", hub.closes, hub.needHuman)
	}
	if v, _ := kv.Get(context.Background(), testIssue().ID, gate.MetaGateErrors); v != "0" {
		t.Fatalf("red must reset the error counter, got %q", v)
	}
}

func TestEvaluateShellGreenClosesAndTombstones(t *testing.T) {
	dir := t.TempDir()
	writeFormula(t, dir, "tests", "type = \"shell\"\ncommand = \"echo all green\"")
	hub, kv := newFakeHub(), newMemKV()
	r := newTestRunner(t, dir, hub, kv, gate.OsExec, nil)

	outcome, err := r.Evaluate(context.Background(), reqWithWorkspace(t, 3, symphony.Revision("wf-1")))
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Verdict != symphony.GateGreen {
		t.Fatalf("verdict = %d, want green", outcome.Verdict)
	}
	if len(hub.closes) != 1 || hub.closes[0].ifAssignee != "lambe@test" {
		t.Fatalf("CAS-guarded close missing: %+v", hub.closes)
	}
	ok, err := r.ShouldDispatch(context.Background(), testIssue().ID)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("tombstoned issue must not be re-dispatched")
	}
}

func TestEvaluateGateInfraErrorReturnsErrWithoutParking(t *testing.T) {
	dir := t.TempDir()
	writeFormula(t, dir, "tests", "type = \"shell\"\ncommand = \"go test ./...\"")
	hub, kv := newFakeHub(), newMemKV()
	fe := &fakeExec{fn: func(string, []string) (string, error) {
		return "", errors.New("CI unreachable")
	}}
	r := newTestRunner(t, dir, hub, kv, fe.exec, nil)

	outcome, err := r.Evaluate(context.Background(), reqWithWorkspace(t, 5, symphony.Revision("wf-1")))
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Verdict != symphony.GateErr {
		t.Fatalf("verdict = %d, want gate error", outcome.Verdict)
	}
	// The caller owns re-evaluation timing: the gate must not park the
	// issue itself.
	if len(hub.defers) != 0 {
		t.Fatalf("gate error must not defer the issue: %+v", hub.defers)
	}
	if len(hub.comments) != 0 || len(hub.closes) != 0 || len(hub.needHuman) != 0 {
		t.Fatalf("gate error must not write back verdicts: %+v", hub)
	}
	if v, _ := kv.Get(context.Background(), testIssue().ID, gate.MetaGateErrors); v != "1" {
		t.Fatalf("consecutive gate error counter = %q, want 1", v)
	}
}

func TestEvaluateMissingGateBinaryIsInfraError(t *testing.T) {
	// PATH manipulation: with an empty PATH the shell gate's sh cannot be
	// found, which is gate-infra failure, never a verdict.
	t.Setenv("PATH", t.TempDir())
	dir := t.TempDir()
	writeFormula(t, dir, "tests", "type = \"shell\"\ncommand = \"true\"")
	hub, kv := newFakeHub(), newMemKV()
	r := newTestRunner(t, dir, hub, kv, gate.OsExec, nil)

	outcome, err := r.Evaluate(context.Background(), reqWithWorkspace(t, 1, symphony.Revision("wf-1")))
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Verdict != symphony.GateErr {
		t.Fatalf("verdict = %d, want gate error for missing infra", outcome.Verdict)
	}
}

func TestConsecutiveGateErrorsEscalateToNeedsHuman(t *testing.T) {
	dir := t.TempDir()
	writeFormula(t, dir, "tests", "type = \"shell\"\ncommand = \"true\"")
	hub, kv := newFakeHub(), newMemKV()
	fe := &fakeExec{fn: func(string, []string) (string, error) { return "", errors.New("gh missing") }}
	r := newTestRunner(t, dir, hub, kv, fe.exec, nil)
	ctx := context.Background()

	for i := 1; i <= 5; i++ {
		outcome, err := r.Evaluate(ctx, reqWithWorkspace(t, 1, symphony.Revision("wf-1")))
		if err != nil {
			t.Fatal(err)
		}
		if outcome.Verdict != symphony.GateErr {
			t.Fatalf("evaluation %d verdict = %d, want gate error", i, outcome.Verdict)
		}
		wantEscalated := i == 5
		if got := len(hub.needHuman) == 1; got != wantEscalated {
			t.Fatalf("evaluation %d: needs-human = %v, want %v", i, got, wantEscalated)
		}
	}
	if v, _ := kv.Get(ctx, testIssue().ID, gate.MetaGateErrors); v != "5" {
		t.Fatalf("error counter = %q, want 5", v)
	}
	if len(hub.closes) != 0 {
		t.Fatal("gate errors must never close the issue")
	}
}

func TestGhGateResults(t *testing.T) {
	cases := []struct {
		name     string
		toml     string
		ghResult gate.GHResult
		ghErr    error
		verdict  symphony.GateVerdict
		wantKV   string // MetaGateErrors value after evaluation
	}{
		{
			name: "run pending defers without consuming attempt", toml: "type = \"gh:run\"\nref = \"" + fullSha + "\"",
			ghResult: gate.GHPending, verdict: symphony.GateErr,
		},
		{
			name: "run failed is red", toml: "type = \"gh:run\"\nref = \"123\"",
			ghResult: gate.GHFail, verdict: symphony.GateRed,
		},
		{
			name: "run green is green", toml: "type = \"gh:run\"\nref = \"123\"",
			ghResult: gate.GHPass, verdict: symphony.GateGreen,
		},
		{
			name: "run with pr resolves and judges the pr head", toml: "type = \"gh:run\"\npr = \"17\"",
			ghResult: gate.GHPass, verdict: symphony.GateGreen,
		},
		{
			name: "run with pr head resolution failure is gate error", toml: "type = \"gh:run\"\npr = \"17\"",
			ghErr: errors.New("gh pr view: no pull requests match"), verdict: symphony.GateErr, wantKV: "1",
		},
		{
			name: "pr merged is green", toml: "type = \"gh:pr\"\npr = \"7\"",
			ghResult: gate.GHPass, verdict: symphony.GateGreen,
		},
		{
			name: "checker infra error is gate error", toml: "type = \"gh:pr\"\npr = \"7\"",
			ghErr: errors.New("gh: command not found"), verdict: symphony.GateErr, wantKV: "1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFormula(t, dir, "gh", tc.toml)
			hub, kv := newFakeHub(), newMemKV()
			gh := &fakeGH{result: tc.ghResult, err: tc.ghErr}
			if tc.ghErr != nil && strings.Contains(tc.name, "head resolution") {
				gh = &fakeGH{result: tc.ghResult, headErr: tc.ghErr}
			}
			r := newTestRunner(t, dir, hub, kv, nil, gh)

			outcome, err := r.Evaluate(context.Background(), reqWithWorkspace(t, 1, symphony.Revision("wf-1")))
			if err != nil {
				t.Fatal(err)
			}
			if outcome.Verdict != tc.verdict {
				t.Fatalf("verdict = %d, want %d", outcome.Verdict, tc.verdict)
			}
			if strings.Contains(tc.name, "resolves and judges the pr head") && gh.runRef != fullSha {
				t.Fatalf("gh:run with pr must judge the resolved PR head, got ref %q", gh.runRef)
			}
			// Gate errors are returned bare; the caller owns retry timing.
			if len(hub.defers) != 0 {
				t.Fatalf("gate error must not park the issue: %+v", hub.defers)
			}
			if want := tc.wantKV; want != "" {
				if v, _ := kv.Get(context.Background(), testIssue().ID, gate.MetaGateErrors); v != want {
					t.Fatalf("error counter = %q, want %q", v, want)
				}
			}
		})
	}
}

func TestConsecutivePendingDeferralsEscalateToNeedsHuman(t *testing.T) {
	dir := t.TempDir()
	writeFormula(t, dir, "ci", "type = \"gh:run\"\nref = \""+fullSha+"\"")
	hub, kv := newFakeHub(), newMemKV()
	r := newTestRunner(t, dir, hub, kv, nil, &fakeGH{result: gate.GHPending})
	ctx := context.Background()

	for i := 1; i <= 5; i++ {
		outcome, err := r.Evaluate(ctx, reqWithWorkspace(t, 1, symphony.Revision("wf-1")))
		if err != nil {
			t.Fatal(err)
		}
		if outcome.Verdict != symphony.GateErr {
			t.Fatalf("evaluation %d verdict = %d, want gate error", i, outcome.Verdict)
		}
		if want := i == 5; (len(hub.needHuman) == 1) != want {
			t.Fatalf("evaluation %d: needs-human present = %v, want %v", i, len(hub.needHuman) == 1, want)
		}
	}
	if v, _ := kv.Get(ctx, testIssue().ID, gate.MetaGateDeferrals); v != "5" {
		t.Fatalf("deferral counter = %q, want 5", v)
	}
}

func TestAttemptCapEscalatesToNeedsHuman(t *testing.T) {
	dir := t.TempDir()
	writeFormula(t, dir, "tests", "type = \"shell\"\ncommand = \"echo still-failing; exit 1\"")
	hub, kv := newFakeHub(), newMemKV()
	r := newTestRunner(t, dir, hub, kv, gate.OsExec, nil)
	ctx := context.Background()

	// Attempt 8 of the default cap of 8 is the escalation point; attempt 7
	// is not.
	if outcome, err := r.Evaluate(ctx, reqWithWorkspace(t, 7, symphony.Revision("wf-1"))); err != nil || outcome.Verdict != symphony.GateRed {
		t.Fatalf("attempt 7: outcome %v err %v, want plain red", outcome.Verdict, err)
	}
	if len(hub.needHuman) != 0 {
		t.Fatal("attempt 7 must not escalate")
	}
	outcome, err := r.Evaluate(ctx, reqWithWorkspace(t, 8, symphony.Revision("wf-1")))
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Verdict != symphony.GateRed {
		t.Fatalf("attempt 8 verdict = %d, want red", outcome.Verdict)
	}
	if len(hub.needHuman) != 1 || !strings.Contains(hub.needHuman[0].lastOutput, "still-failing") {
		t.Fatalf("needs-human transition with last gate output missing: %+v", hub.needHuman)
	}
	if len(hub.closes) != 0 {
		t.Fatal("attempt cap must never close the issue")
	}
	if ok, _ := hub.IsTombstoned(ctx, testIssue().ID); ok {
		t.Fatal("attempt cap must not tombstone; workspace stays inspectable")
	}
}

func TestPinnedRevisionJudgesThePinnedFormula(t *testing.T) {
	dir := t.TempDir()
	writeFormula(t, dir, "tests", "type = \"shell\"\ncommand = \"exit 0\"")
	hub, kv := newFakeHub(), newMemKV()
	r := newTestRunner(t, dir, hub, kv, gate.OsExec, nil)
	rev := symphony.Revision("wf-claim-time")
	if err := r.Pin(rev); err != nil {
		t.Fatal(err)
	}

	// Operator edits the formula mid-attempt: the attempt is still judged
	// by the revision pinned at claim time.
	writeFormula(t, dir, "tests", "type = \"shell\"\ncommand = \"exit 1\"")

	outcome, err := r.Evaluate(context.Background(), reqWithWorkspace(t, 1, rev))
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Verdict != symphony.GateGreen {
		t.Fatalf("pinned revision must judge the run: verdict = %d, want green", outcome.Verdict)
	}

	// An unpinned revision sees the current formula.
	outcome, err = r.Evaluate(context.Background(), reqWithWorkspace(t, 1, symphony.Revision("wf-later")))
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Verdict != symphony.GateRed {
		t.Fatalf("current revision verdict = %d, want red", outcome.Verdict)
	}
}

func TestNoGateFormulasIsGateError(t *testing.T) {
	dir := t.TempDir()
	hub, kv := newFakeHub(), newMemKV()
	r := newTestRunner(t, dir, hub, kv, gate.OsExec, nil)
	outcome, err := r.Evaluate(context.Background(), reqWithWorkspace(t, 1, symphony.Revision("wf-1")))
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Verdict != symphony.GateErr {
		t.Fatalf("unverifiable completion must be a gate error, got %d", outcome.Verdict)
	}
}

// ---- loop-level semantics through the symphony seam ----

// memTracker is a minimal tracker.Tracker for loop tests.
type memTracker struct {
	mu       sync.Mutex
	issues   map[string]tracker.Issue
	attempts []tracker.Attempt
}

func newMemTracker(issues ...tracker.Issue) *memTracker {
	m := &memTracker{issues: map[string]tracker.Issue{}}
	for _, i := range issues {
		m.issues[i.ID] = i
	}
	return m
}

func (m *memTracker) Capabilities(context.Context) (tracker.Capabilities, error) {
	return tracker.Capabilities{}, nil
}

func (m *memTracker) Dispatchable(_ context.Context, q tracker.DispatchQuery) ([]tracker.Issue, error) {
	if q.Empty() {
		return nil, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []tracker.Issue
	for _, issue := range m.issues {
		if len(q.IDs) > 0 {
			for _, id := range q.IDs {
				if id == issue.ID {
					out = append(out, issue)
				}
			}
			continue
		}
		if issue.Status == "ready" && issue.Assignee == "" {
			out = append(out, issue)
		}
	}
	return out, nil
}

func (m *memTracker) Claim(_ context.Context, id string) (tracker.Issue, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	issue, ok := m.issues[id]
	if !ok {
		return tracker.Issue{}, fmt.Errorf("no issue %s", id)
	}
	if issue.Assignee != "" && issue.Assignee != "lambe@test" {
		return tracker.Issue{}, fmt.Errorf("%w: held by %s", tracker.ErrLostRace, issue.Assignee)
	}
	issue.Assignee = "lambe@test"
	issue.Status = "in_progress"
	m.issues[id] = issue
	return issue, nil
}

func (m *memTracker) Release(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	issue := m.issues[id]
	issue.Assignee = ""
	issue.Status = "open"
	m.issues[id] = issue
	return nil
}

func (m *memTracker) Heartbeat(context.Context, string) error { return nil }
func (m *memTracker) Reclaim(context.Context, time.Duration, []string) error {
	return nil
}

func (m *memTracker) Defer(context.Context, string, time.Time, string) error {
	return nil
}

func (m *memTracker) Events(context.Context, int64) ([]tracker.Event, error) {
	return nil, tracker.ErrJournalDisabled
}

func (m *memTracker) EnsureIssue(_ context.Context, key string, _ tracker.IssueSpec) (tracker.Issue, error) {
	return tracker.Issue{ID: key}, nil
}

func (m *memTracker) PutAttempt(_ context.Context, a tracker.Attempt) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.attempts = append(m.attempts, a)
	return nil
}

func (m *memTracker) Attempts(_ context.Context, sessionID string) ([]tracker.Attempt, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []tracker.Attempt
	for _, a := range m.attempts {
		if a.SessionID == sessionID {
			out = append(out, a)
		}
	}
	return out, nil
}

// ListByMetadata serves nothing: this fixture only drives the gate, which
// reads attempts per session.
func (m *memTracker) ListByMetadata(context.Context, string, string) ([]tracker.Issue, error) {
	return nil, nil
}

// status reads the current tracker status for an issue.
func (m *memTracker) status(id string) tracker.Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.issues[id].Status
}

// agentRunner records prompts and always settles.
type agentRunner struct {
	mu       sync.Mutex
	prompts  []string
	attempts []int
	settled  bool
}

func (a *agentRunner) RunTurn(_ context.Context, req symphony.TurnRequest) (symphony.TurnResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.prompts = append(a.prompts, req.Prompt)
	a.attempts = append(a.attempts, req.Attempt)
	return symphony.TurnResult{Exit: symphony.TurnSuccess, Output: "agent declares done", Settled: a.settled}, nil
}

// loopFixture wires a full symphony loop around a gate runner.
type loopFixture struct {
	tr     *memTracker
	agent  *agentRunner
	gates  *gate.Runner
	hub    *fakeHub
	loop   *waitingLoop
	spaces *symphony.WorkspaceManager
}

func newLoopFixture(t *testing.T, gateTOML string) *loopFixture {
	t.Helper()
	root := t.TempDir()
	formulas := filepath.Join(root, "formulas")
	writeFormula(t, formulas, "tests", gateTOML)

	workflowPath := filepath.Join(root, "WORKFLOW.md")
	body := "---\nmax_turns: 3\n---\nWork on {{ issue }} (attempt {{ attempt }}).\n"
	if err := os.WriteFile(workflowPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	loader, err := symphony.NewLoader(workflowPath, symphony.OSEnv, discardLog())
	if err != nil {
		t.Fatal(err)
	}

	tr := newMemTracker(tracker.Issue{ID: "bd-loop-1", Title: "fix the flake", Status: "ready"})
	agent := &agentRunner{settled: true}
	hub := newFakeHub()
	kv := newMemKV()
	gates := gate.NewRunner(formulas, hub, gate.NewEscalator(kv), "lambe@test", discardLog())
	gates.Exec = gate.OsExec

	spaces := symphony.NewWorkspaceManager(filepath.Join(root, "spaces"), discardLog())
	loop := symphony.NewLoop(tr, loader, agent, gates, spaces, discardLog())
	loop.FailureDelay = func(int) time.Duration { return 2 * time.Millisecond }
	loop.ContinuationDelay = 2 * time.Millisecond
	return &loopFixture{tr: tr, agent: agent, gates: gates, hub: hub, loop: &waitingLoop{loop}, spaces: spaces}
}

func TestLoopRedConsumesAttemptAndFeedsOutputToNextPrompt(t *testing.T) {
	f := newLoopFixture(t, "type = \"shell\"\ncommand = \"echo gate-says-tests-fail >&2; exit 1\"")
	ctx := context.Background()

	if err := f.loop.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if err := f.loop.Tick(ctx); err != nil {
		t.Fatal(err)
	}

	f.agent.mu.Lock()
	defer f.agent.mu.Unlock()
	if len(f.agent.attempts) != 2 {
		t.Fatalf("red must consume an attempt: attempt numbers %v", f.agent.attempts)
	}
	if f.agent.attempts[0] != 1 || f.agent.attempts[1] != 2 {
		t.Fatalf("attempt numbers = %v, want [1 2]", f.agent.attempts)
	}
	if !strings.Contains(f.agent.prompts[1], "rejected by the gate") || !strings.Contains(f.agent.prompts[1], "gate-says-tests-fail") {
		t.Fatalf("next attempt's prompt must explain the rejection, got %q", f.agent.prompts[1])
	}
	if len(f.hub.comments) < 1 || !strings.Contains(f.hub.comments[len(f.hub.comments)-1].body, "gate-says-tests-fail") {
		t.Fatalf("rejection comment must be written to the hub: %+v", f.hub.comments)
	}
}

func TestLoopGateErrorReusesAttempt(t *testing.T) {
	f := newLoopFixture(t, "type = \"shell\"\ncommand = \"true\"")
	// CI unreachable: the gate command's interpreter cannot even start.
	evalCalls := 0
	f.gates.Exec = func(context.Context, string, ...string) (string, error) {
		evalCalls++
		return "", errors.New("CI unreachable")
	}
	ctx := context.Background()

	if err := f.loop.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if err := f.loop.Tick(ctx); err != nil {
		t.Fatal(err)
	}

	f.agent.mu.Lock()
	defer f.agent.mu.Unlock()
	if len(f.agent.attempts) != 1 {
		t.Fatalf("gate error must not re-prompt the agent: got %d agent attempts, want 1 (gate retries re-evaluate on the settled output)", len(f.agent.attempts))
	}
	if n := f.agent.attempts[0]; n != 1 {
		t.Fatalf("gate errors must not consume attempts: attempt = %d", n)
	}
	if got := evalCalls; got < 2 {
		t.Fatalf("gate evaluations = %d, want >= 2 (gate-only retry on the same output)", got)
	}
	if len(f.hub.defers) != 0 {
		t.Fatal("gate error must not park the issue; the loop owns retry timing")
	}
	if f.tr.status("bd-loop-1") != "in_progress" {
		t.Fatalf("issue status = %q, want in_progress (never terminal on gate error)", f.tr.status("bd-loop-1"))
	}
}

// TestLoopClosesOnlyWhenTheGateIsGreen pins completion verification for every
// gate type: an agent that settles and declares itself done never closes the
// issue unless its gate (shell, gh:run, gh:pr) judges the work green.
func TestLoopClosesOnlyWhenTheGateIsGreen(t *testing.T) {
	cases := []struct {
		name   string
		toml   string
		gh     gate.GHResult
		closed bool
	}{
		{"shell red", "type = \"shell\"\ncommand = \"exit 1\"", 0, false},
		{"shell green", "type = \"shell\"\ncommand = \"exit 0\"", 0, true},
		{"gh:run failed", "type = \"gh:run\"\nref = \"123\"", gate.GHFail, false},
		{"gh:run pending", "type = \"gh:run\"\nref = \"123\"", gate.GHPending, false},
		{"gh:run green", "type = \"gh:run\"\nref = \"123\"", gate.GHPass, true},
		{"gh:pr not merged", "type = \"gh:pr\"\npr = \"7\"", gate.GHFail, false},
		{"gh:pr pending", "type = \"gh:pr\"\npr = \"7\"", gate.GHPending, false},
		{"gh:pr merged", "type = \"gh:pr\"\npr = \"7\"", gate.GHPass, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newLoopFixture(t, tc.toml)
			f.gates.Gh = &fakeGH{result: tc.gh}
			if err := f.loop.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := len(f.hub.closes) == 1; got != tc.closed {
				t.Fatalf("closed = %v, want %v (closes: %+v)", got, tc.closed, f.hub.closes)
			}
			if !tc.closed && f.tr.status("bd-loop-1") == tracker.StatusClosed {
				t.Fatal("issue closed without a green gate")
			}
		})
	}
}

func TestLoopGreenClosesReleasesAndCleansWorkspace(t *testing.T) {
	f := newLoopFixture(t, "type = \"shell\"\ncommand = \"exit 0\"")
	ctx := context.Background()

	if err := f.loop.Tick(ctx); err != nil {
		t.Fatal(err)
	}

	if len(f.hub.closes) != 1 {
		t.Fatalf("green must CAS-close the issue: %+v", f.hub.closes)
	}
	if ok, _ := f.gates.ShouldDispatch(ctx, "bd-loop-1"); ok {
		t.Fatal("closed issue must be tombstoned against re-dispatch")
	}
	entries, err := os.ReadDir(filepath.Join(f.spaces.Root))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("terminal workspace must be cleaned, got %v", entries)
	}
	if issue := f.tr.issues["bd-loop-1"]; issue.Assignee != "" {
		t.Fatal("claim must be released after green")
	}
}

func TestLoopAttemptCapRetainsWorkspace(t *testing.T) {
	f := newLoopFixture(t, "type = \"shell\"\ncommand = \"exit 1\"")
	f.gates.Esc.AttemptCap = 1
	ctx := context.Background()

	if err := f.loop.Tick(ctx); err != nil {
		t.Fatal(err)
	}

	if len(f.hub.needHuman) != 1 {
		t.Fatalf("attempt cap must escalate to needs-human: %+v", f.hub.needHuman)
	}
	if len(f.hub.closes) != 0 {
		t.Fatal("attempt cap must never close")
	}
	entries, err := os.ReadDir(filepath.Join(f.spaces.Root))
	if err != nil {
		t.Fatal(err)
	}
	// Workspace retention is the flip side of the green test: only a
	// green terminal close may clean the workspace, so a human can still
	// inspect what eight red attempts produced.
	if len(entries) != 1 {
		t.Fatalf("needs-human workspace must be retained, got %d entries", len(entries))
	}
}

// ---- escalation thresholds ----

// TestEscalatorDeferThresholdIndependent proves DeferThreshold is
// consulted: with DeferThreshold=2 and ErrorThreshold=5, deferrals
// escalate at 2 while errors still need 5.
func TestEscalatorDeferThresholdIndependent(t *testing.T) {
	ctx := context.Background()
	kv := newMemKV()
	esc := gate.NewEscalator(kv)
	esc.DeferThreshold = 2
	esc.ErrorThreshold = 5

	for i := 1; i <= 2; i++ {
		escalated, err := esc.RecordDeferral(ctx, "bd-esc-1")
		if err != nil {
			t.Fatal(err)
		}
		if want := i == 2; escalated != want {
			t.Fatalf("deferral %d: escalated = %v, want %v", i, escalated, want)
		}
	}
	if escalated, err := esc.RecordGateError(ctx, "bd-esc-1"); err != nil || escalated {
		t.Fatalf("error streak must not borrow the deferral threshold: escalated=%v err=%v", escalated, err)
	}

	for i := 1; i <= 5; i++ {
		escalated, err := esc.RecordGateError(ctx, "bd-esc-2")
		if err != nil {
			t.Fatal(err)
		}
		if want := i == 5; escalated != want {
			t.Fatalf("error %d: escalated = %v, want %v", i, escalated, want)
		}
	}
	if escalated, err := esc.RecordDeferral(ctx, "bd-esc-2"); err != nil || escalated {
		t.Fatalf("defer threshold must not be raised by the error threshold: escalated=%v err=%v", escalated, err)
	}
}

// ---- gh CLI verdict mapping ----

// TestGhCLICheckRunConclusions covers every CheckRun conclusion mapping
// plus malformed JSON, with the process execution stubbed.
func TestGhCLICheckRunConclusions(t *testing.T) {
	cases := []struct {
		name       string
		json       string
		execErr    error
		want       gate.GHResult
		wantErr    bool
		wantArgvOk bool
	}{
		{name: "success passes", json: `{"status":"completed","conclusion":"success"}`, want: gate.GHPass},
		{name: "failure fails", json: `{"status":"completed","conclusion":"failure"}`, want: gate.GHFail},
		{name: "cancelled fails", json: `{"status":"completed","conclusion":"cancelled"}`, want: gate.GHFail},
		{name: "timed_out fails", json: `{"status":"completed","conclusion":"timed_out"}`, want: gate.GHFail},
		{name: "startup_failure fails", json: `{"status":"completed","conclusion":"startup_failure"}`, want: gate.GHFail},
		{name: "unknown conclusion pends", json: `{"status":"completed","conclusion":"weird"}`, want: gate.GHPending},
		{name: "in_progress pends", json: `{"status":"in_progress","conclusion":""}`, want: gate.GHPending},
		{name: "queued pends", json: `{"status":"queued"}`, want: gate.GHPending},
		{name: "malformed json is an error", json: `{"status":`, want: gate.GHPending, wantErr: true},
		{name: "non-object json is an error", json: `[1,2]`, want: gate.GHPending, wantErr: true},
		{name: "exec failure is an error", execErr: errors.New("gh: command not found"), want: gate.GHPending, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			cli := &gate.GhCLI{Exec: func(_ context.Context, _ string, argv ...string) (string, error) {
				called = true
				want := []string{"gh", "run", "view", fullSha, "--json", "status,conclusion"}
				if fmt.Sprint(argv) != fmt.Sprint(want) {
					t.Errorf("argv = %v, want %v", argv, want)
				}
				return tc.json, tc.execErr
			}}
			res, err := cli.CheckRun(context.Background(), fullSha)
			if called != true {
				t.Fatal("exec seam not invoked")
			}
			if tc.wantErr != (err != nil) {
				t.Fatalf("err = %v, want error=%v", err, tc.wantErr)
			}
			if res != tc.want {
				t.Fatalf("result = %d, want %d", res, tc.want)
			}
		})
	}
}

// TestGhCLICheckPRStates covers the CheckPR state mapping with the
// process execution stubbed.
func TestGhCLICheckPRStates(t *testing.T) {
	cases := []struct {
		name    string
		json    string
		execErr error
		want    gate.GHResult
		wantErr bool
	}{
		{name: "merged passes", json: `{"state":"MERGED"}`, want: gate.GHPass},
		{name: "closed fails", json: `{"state":"CLOSED"}`, want: gate.GHFail},
		{name: "open pends", json: `{"state":"OPEN"}`, want: gate.GHPending},
		{name: "malformed json is an error", json: `nope`, want: gate.GHPending, wantErr: true},
		{name: "exec failure is an error", execErr: errors.New("connection refused"), want: gate.GHPending, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cli := &gate.GhCLI{Exec: func(_ context.Context, _ string, argv ...string) (string, error) {
				want := []string{"gh", "pr", "view", "17", "--json", "state"}
				if fmt.Sprint(argv) != fmt.Sprint(want) {
					t.Errorf("argv = %v, want %v", argv, want)
				}
				return tc.json, tc.execErr
			}}
			res, err := cli.CheckPR(context.Background(), "17")
			if tc.wantErr != (err != nil) {
				t.Fatalf("err = %v, want error=%v", err, tc.wantErr)
			}
			if res != tc.want {
				t.Fatalf("result = %d, want %d", res, tc.want)
			}
		})
	}
}

// TestGhCLIResolvePRHead covers the PR-head resolution gh:run gates use.
func TestGhCLIResolvePRHead(t *testing.T) {
	t.Run("resolves headRefOid", func(t *testing.T) {
		cli := &gate.GhCLI{Exec: func(_ context.Context, _ string, argv ...string) (string, error) {
			want := []string{"gh", "pr", "view", "17", "--json", "headRefOid"}
			if fmt.Sprint(argv) != fmt.Sprint(want) {
				t.Errorf("argv = %v, want %v", argv, want)
			}
			return `{"headRefOid":"` + fullSha + `"}`, nil
		}}
		head, err := cli.ResolvePRHead(context.Background(), "17")
		if err != nil {
			t.Fatal(err)
		}
		if head != fullSha {
			t.Fatalf("head = %q, want %q", head, fullSha)
		}
	})
	t.Run("missing headRefOid is an error", func(t *testing.T) {
		cli := &gate.GhCLI{Exec: func(context.Context, string, ...string) (string, error) {
			return `{}`, nil
		}}
		if _, err := cli.ResolvePRHead(context.Background(), "17"); err == nil {
			t.Fatal("empty headRefOid must be an error")
		}
	})
}

// ---- close race and request-path pinning ----

// TestGreenCloseRaceSkipsTombstoneReturnsGateErr proves only the close
// winner tombstones: a lost CAS race against a non-terminal issue (someone
// else holds it in_progress) neither tombstones nor reports green — the
// caller re-reconciles.
func TestGreenCloseRaceSkipsTombstoneReturnsGateErr(t *testing.T) {
	dir := t.TempDir()
	writeFormula(t, dir, "tests", "type = \"shell\"\ncommand = \"exit 0\"")
	hub, kv := newFakeHub(), newMemKV()
	hub.closeErr = fmt.Errorf("close bd-gate-1: %w", gate.ErrCloseRace)
	hub.statuses[testIssue().ID] = "in_progress"
	r := newTestRunner(t, dir, hub, kv, gate.OsExec, nil)
	ctx := context.Background()

	outcome, err := r.Evaluate(ctx, reqWithWorkspace(t, 1, symphony.Revision("wf-1")))
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Verdict != symphony.GateErr {
		t.Fatalf("verdict = %d, want gate error on lost close race with a non-terminal issue", outcome.Verdict)
	}
	if ok, _ := hub.IsTombstoned(ctx, testIssue().ID); ok {
		t.Fatal("close-race loser must not tombstone; the close winner owns it")
	}
	if len(hub.needHuman) != 0 {
		t.Fatal("a close race is not an escalation")
	}
}

// TestGreenCloseRaceOnClosedIssueStandsGreen pins the close-race
// reconcile: when the race winner already moved the issue terminal, the
// completion stands as green without a tombstone or counter churn — no
// noisy retry-and-reconcile cycle (first live trial finding).
func TestGreenCloseRaceOnClosedIssueStandsGreen(t *testing.T) {
	dir := t.TempDir()
	writeFormula(t, dir, "tests", "type = \"shell\"\ncommand = \"echo all green\"")
	hub, kv := newFakeHub(), newMemKV()
	hub.closeErr = fmt.Errorf("close bd-gate-1: %w", gate.ErrCloseRace)
	hub.statuses[testIssue().ID] = gate.StatusClosed
	r := newTestRunner(t, dir, hub, kv, gate.OsExec, nil)
	ctx := context.Background()

	outcome, err := r.Evaluate(ctx, reqWithWorkspace(t, 1, symphony.Revision("wf-1")))
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Verdict != symphony.GateGreen {
		t.Fatalf("verdict = %d, want green when the close race winner already closed the issue", outcome.Verdict)
	}
	if ok, _ := hub.IsTombstoned(ctx, testIssue().ID); ok {
		t.Fatal("close-race loser must not tombstone; the close winner owns it")
	}
	if len(hub.needHuman) != 0 {
		t.Fatal("a close race is not an escalation")
	}
}

// TestGreenCloseRaceStatusReadFailureIsGateErr pins the conservative
// fallback: when the post-race status re-read itself fails, the outcome is
// a gate error so the caller re-reconciles rather than guessing terminal.
func TestGreenCloseRaceStatusReadFailureIsGateErr(t *testing.T) {
	dir := t.TempDir()
	writeFormula(t, dir, "tests", "type = \"shell\"\ncommand = \"exit 0\"")
	hub, kv := newFakeHub(), newMemKV()
	hub.closeErr = fmt.Errorf("close bd-gate-1: %w", gate.ErrCloseRace)
	hub.statusErr = errors.New("tracker unreachable")
	r := newTestRunner(t, dir, hub, kv, gate.OsExec, nil)

	outcome, err := r.Evaluate(context.Background(), reqWithWorkspace(t, 1, symphony.Revision("wf-1")))
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Verdict != symphony.GateErr {
		t.Fatalf("verdict = %d, want gate error when the status re-read fails", outcome.Verdict)
	}
}

// TestEvaluatePinsRevisionFromRequest proves claim-time pinning engages
// through the gate request: the first evaluation carrying a revision
// snapshots the formulas, and a mid-attempt formula edit does not change
// the rules the attempt is judged by.
func TestEvaluatePinsRevisionFromRequest(t *testing.T) {
	dir := t.TempDir()
	writeFormula(t, dir, "tests", "type = \"shell\"\ncommand = \"exit 0\"")
	hub, kv := newFakeHub(), newMemKV()
	r := newTestRunner(t, dir, hub, kv, gate.OsExec, nil)
	rev := symphony.Revision("wf-request-pin")

	outcome, err := r.Evaluate(context.Background(), reqWithWorkspace(t, 1, rev))
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Verdict != symphony.GateGreen {
		t.Fatalf("first evaluation verdict = %d, want green", outcome.Verdict)
	}

	// Operator edits the formula mid-attempt.
	writeFormula(t, dir, "tests", "type = \"shell\"\ncommand = \"exit 1\"")
	outcome, err = r.Evaluate(context.Background(), reqWithWorkspace(t, 1, rev))
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Verdict != symphony.GateGreen {
		t.Fatalf("pinned revision must judge the attempt: verdict = %d, want green", outcome.Verdict)
	}

	// A different revision sees the edited formula.
	outcome, err = r.Evaluate(context.Background(), reqWithWorkspace(t, 1, symphony.Revision("wf-next")))
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Verdict != symphony.GateRed {
		t.Fatalf("new revision must see the live formula: verdict = %d, want red", outcome.Verdict)
	}
}

// ---- bd integration (gated) ----

func runBd(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("bd", args...) // #nosec G204 -- fixed argv against a test rig
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "BD_JSON_ENVELOPE=1", "BEADS_ACTOR=lambe-integration")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bd %v: %v: %s", args, err, out)
	}
	return string(out)
}

// bdCreateIssue creates an issue and returns its id.
func bdCreateIssue(t *testing.T, dir, title string) string {
	t.Helper()
	var env struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(runBd(t, dir, "create", "--title", title, "--json")), &env); err != nil {
		t.Fatal(err)
	}
	if env.Data.ID == "" {
		t.Fatal("no issue id in bd create output")
	}
	return env.Data.ID
}

func TestBdIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("bd integration skipped under -short")
	}
	if os.Getenv("LAMBE_TEST_BD") != "1" {
		t.Skip("set LAMBE_TEST_BD=1 to run the bd integration test")
	}
	if _, err := exec.LookPath("bd"); err != nil {
		t.Skip("bd not on PATH")
	}

	rigDir := t.TempDir()
	runBd(t, rigDir, "init")
	runBd(t, rigDir, "config", "set", "status.custom", gate.StatusNeedsHuman.String())

	hub, err := gate.NewBdHub("", rigDir, "lambe-integration")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	formulas := filepath.Join(rigDir, "formulas")
	writeFormula(t, formulas, "tests", "type = \"shell\"\ncommand = \"echo integration-red; exit 1\"")
	gates := gate.NewRunner(formulas, hub, gate.NewEscalator(hub), "lambe-integration", discardLog())
	gates.Exec = gate.OsExec

	id := bdCreateIssue(t, rigDir, "gate integration")
	runBd(t, rigDir, "update", id, "--claim", "--json")

	req := symphony.GateRequest{
		Issue:     tracker.Issue{ID: id, Title: "gate integration", Status: "in_progress"},
		Attempt:   1,
		Workspace: rigDir,
		Revision:  symphony.Revision("wf-int"),
		Output:    "agent declares done",
	}

	// Red: attempt consumed, rejection comment readable through bd.
	outcome, err := gates.Evaluate(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Verdict != symphony.GateRed {
		t.Fatalf("verdict = %d, want red", outcome.Verdict)
	}
	comments := runBd(t, rigDir, "comments", id)
	if !strings.Contains(comments, "integration-red") {
		t.Fatalf("rejection comment missing in bd output:\n%s", comments)
	}

	// Green after the operator fixes the formula: CAS close + tombstone.
	// The formula edit means a fresh gate revision; the pinned revision
	// would still judge by the old formula.
	writeFormula(t, formulas, "tests", "type = \"shell\"\ncommand = \"exit 0\"")
	req.Revision = symphony.Revision("wf-int-green")
	outcome, err = gates.Evaluate(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Verdict != symphony.GateGreen {
		t.Fatalf("verdict = %d, want green", outcome.Verdict)
	}
	show := runBd(t, rigDir, "show", id, "--json")
	if !strings.Contains(show, `"status": "closed"`) && !strings.Contains(show, `"status":"closed"`) {
		t.Fatalf("issue not closed:\n%s", show)
	}
	ok, err := hub.IsTombstoned(ctx, id)
	if err != nil || !ok {
		t.Fatalf("tombstone missing (ok=%v err=%v)", ok, err)
	}

	// Attempt cap: needs-human status, never closed.
	capID := bdCreateIssue(t, rigDir, "cap integration")
	runBd(t, rigDir, "update", capID, "--claim", "--json")
	esc := gate.NewEscalator(hub)
	esc.AttemptCap = 2
	writeFormula(t, formulas, "tests", "type = \"shell\"\ncommand = \"exit 1\"")
	capRunner := gate.NewRunner(formulas, hub, esc, "lambe-integration", discardLog())
	capRunner.Exec = gate.OsExec
	capReq := symphony.GateRequest{
		Issue:     tracker.Issue{ID: capID, Title: "cap integration", Status: "in_progress"},
		Attempt:   2,
		Workspace: rigDir,
		Revision:  symphony.Revision("wf-int"),
		Output:    "agent declares done",
	}
	outcome, err = capRunner.Evaluate(ctx, capReq)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Verdict != symphony.GateRed {
		t.Fatalf("verdict = %d, want red at cap", outcome.Verdict)
	}
	show = runBd(t, rigDir, "show", capID, "--json")
	if !strings.Contains(show, gate.StatusNeedsHuman.String()) {
		t.Fatalf("issue not in needs-human:\n%s", show)
	}
	// Dispatch stops: the needs-human status removes the issue from the
	// dispatch front, so no further attempts are handed out.
	front := runBd(t, rigDir, "list", "--status", "open", "--json")
	if strings.Contains(front, capID) {
		t.Fatalf("needs-human issue must be off the dispatch front:\n%s", front)
	}
}

// TestBdCounterRestartSurvival proves the escalation counters persist in
// tracker metadata across supervisor restarts: a fresh hub over the same
// tracker directory sees the accumulated streak and escalates exactly
// when the fifth consecutive error lands.
func TestBdCounterRestartSurvival(t *testing.T) {
	if testing.Short() {
		t.Skip("bd integration skipped under -short")
	}
	if os.Getenv("LAMBE_TEST_BD") != "1" {
		t.Skip("set LAMBE_TEST_BD=1 to run the bd integration test")
	}
	if _, err := exec.LookPath("bd"); err != nil {
		t.Skip("bd not on PATH")
	}

	rigDir := t.TempDir()
	runBd(t, rigDir, "init")
	id := bdCreateIssue(t, rigDir, "restart survival")

	hub1, err := gate.NewBdHub("", rigDir, "lambe-integration")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	esc1 := gate.NewEscalator(hub1)
	for i := 0; i < 2; i++ {
		if escalated, err := esc1.RecordGateError(ctx, id); err != nil || escalated {
			t.Fatalf("pre-restart error %d: escalated=%v err=%v", i+1, escalated, err)
		}
	}

	// Supervisor restart: a new hub over the same tracker directory.
	hub2, err := gate.NewBdHub("", rigDir, "lambe-integration")
	if err != nil {
		t.Fatal(err)
	}
	esc2 := gate.NewEscalator(hub2)
	for i := 3; i <= 4; i++ {
		if escalated, err := esc2.RecordGateError(ctx, id); err != nil || escalated {
			t.Fatalf("post-restart error %d: escalated=%v err=%v", i, escalated, err)
		}
	}
	escalated, err := esc2.RecordGateError(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !escalated {
		t.Fatal("the fifth consecutive gate error must escalate, even across a restart")
	}

	// A decided gate resets the streak, matching the faultsuite policy
	// model where a completed attempt clears the deferral counter.
	if err := esc2.ResetCounters(ctx, id); err != nil {
		t.Fatal(err)
	}
	if v, err := hub2.Get(ctx, id, gate.MetaGateErrors); err != nil || v != "0" {
		t.Fatalf("reset must zero the persisted counter, got %q (err %v)", v, err)
	}
	if escalated, err := esc2.RecordGateError(ctx, id); err != nil || escalated {
		t.Fatalf("after a decided gate the streak must start over: escalated=%v err=%v", escalated, err)
	}
}

func TestOsExecTimeoutIsInfrastructureFailure(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, err := gate.OsExec(ctx, t.TempDir(), "sh", "-c", "exec sleep 10")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout error = %v, want deadline exceeded", err)
	}
	var red *gate.ExitFailure
	if errors.As(err, &red) {
		t.Fatal("timed-out gate classified as rejection")
	}
}

func TestLoopGateTimeoutPersistsEscalation(t *testing.T) {
	f := newLoopFixture(t, "type = \"shell\"\ncommand = \"true\"")
	f.gates.CheckTimeout = 20 * time.Millisecond
	f.gates.Exec = func(ctx context.Context, _ string, _ ...string) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}
	for range 5 {
		if err := f.loop.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(f.hub.needHuman) != 1 {
		t.Fatalf("needs-human = %d, want escalation after 5 timeouts", len(f.hub.needHuman))
	}
	if len(f.agent.attempts) != 1 {
		t.Fatalf("agent ran %d times, want 1", len(f.agent.attempts))
	}
}

// waitingLoop joins dispatched workers for turn/gate protocol assertions.
type waitingLoop struct{ *symphony.Loop }

func (l *waitingLoop) Tick(ctx context.Context) error {
	if err := l.Loop.Tick(ctx); err != nil {
		return err
	}
	return l.Wait(ctx)
}

// TestNeedsHumanFallsBackToBlocked pins issue #18: bd rejects the
// needs-human status unless the custom status is configured, and escalation
// must still park the issue. Without EnsureNeedsHumanStatus the writeback
// falls back to blocked, which keeps the issue out of the dispatch front.
func TestNeedsHumanFallsBackToBlocked(t *testing.T) {
	if os.Getenv("LAMBE_TEST_BD") != "1" {
		t.Skip("set LAMBE_TEST_BD=1 to run the bd integration test")
	}
	if _, err := exec.LookPath("bd"); err != nil {
		t.Skip("bd not on PATH")
	}

	dir := t.TempDir()
	runBd(t, dir, "init") // deliberately does NOT configure status.custom

	hub, err := gate.NewBdHub("", dir, "lambe-integration")
	if err != nil {
		t.Fatal(err)
	}
	id := bdCreateIssue(t, dir, "needs-human fallback")

	if err := hub.NeedsHuman(context.Background(), id, "no gate formulas configured", "gate output"); err != nil {
		t.Fatalf("NeedsHuman should fall back to blocked, got error: %v", err)
	}
	show := runBd(t, dir, "show", id, "--json")
	if !strings.Contains(show, `"status": "blocked"`) && !strings.Contains(show, `"status":"blocked"`) {
		t.Fatalf("issue not parked as blocked:\n%s", show)
	}
	comments := runBd(t, dir, "comments", id)
	if !strings.Contains(comments, "no gate formulas configured") {
		t.Fatalf("parking reason missing from comments:\n%s", comments)
	}
}

func TestCheckpointGateSnapshotSurvivesRunnerRestart(t *testing.T) {
	dir := t.TempDir()
	writeFormula(t, dir, "tests", "type = \"shell\"\ncommand = \"exit 0\"")
	specs, err := gate.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := json.Marshal(specs)
	if err != nil {
		t.Fatal(err)
	}
	writeFormula(t, dir, "tests", "type = \"shell\"\ncommand = \"exit 1\"")
	r := newTestRunner(t, dir, newFakeHub(), newMemKV(), gate.OsExec, nil)
	req := reqWithWorkspace(t, 1, symphony.Revision("named-workflow"))
	req.GateSpec = snapshot
	outcome, err := r.Evaluate(t.Context(), req)
	if err != nil || outcome.Verdict != symphony.GateGreen {
		t.Fatalf("checkpoint-pinned gate: verdict=%v err=%v", outcome.Verdict, err)
	}
}

// TestRestoredCheckpointWithoutSnapshotUsesLiveFormulas pins the restart path
// for a single-workflow deployment: no gate snapshot was taken at claim time,
// the checkpoint round-trips the empty GateSpec as JSON null, and the
// recovered session must still be judged by the formulas directory rather
// than fail with "no gate formulas configured".
func TestRestoredCheckpointWithoutSnapshotUsesLiveFormulas(t *testing.T) {
	dir := t.TempDir()
	writeFormula(t, dir, "tests", "type = \"shell\"\ncommand = \"exit 0\"")
	raw, err := json.Marshal(symphony.Workflow{})
	if err != nil {
		t.Fatal(err)
	}
	var restored symphony.Workflow
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	r := newTestRunner(t, dir, newFakeHub(), newMemKV(), gate.OsExec, nil)
	req := reqWithWorkspace(t, 1, symphony.Revision("wf-1"))
	req.GateSpec = restored.GateSpec
	outcome, err := r.Evaluate(t.Context(), req)
	if err != nil || outcome.Verdict != symphony.GateGreen {
		t.Fatalf("restored checkpoint (GateSpec=%q): verdict=%v err=%v", restored.GateSpec, outcome.Verdict, err)
	}
}
