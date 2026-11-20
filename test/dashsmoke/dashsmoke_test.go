// Package dashsmoke is the dashboard dev smoke: the assembled, binary-level
// proof that the real lambe daemon — bearer-authed, descriptor-publishing —
// serves all four operator views' data through the shared
// internal/stateapi client, over isolated fixture state.
//
// Production safety is by construction, not by assertion: the smoke builds
// its own binary, seeds its own disposable bd tracker in a temp directory,
// points the daemon at its own LAMBE_STATE_DIR, and never invokes launchd
// or any process-management surface on the operator's daemons. The harness
// owns every process it starts; the client owns none.
//
// The seeding drives one real run end-to-end (dispatch → fake ACP agent →
// shell gate → settle) so history and run detail carry actual finished-run
// facts, and seeds deterministic parked and blocked rows for the attention
// and queue views. Gating follows the repo convention: skip (never fail)
// without LAMBE_TEST_BD=1 and a bd binary, and under -short.
package dashsmoke_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/andrew-bierman/lambe/internal/gate"
	"github.com/andrew-bierman/lambe/internal/httpstate"
	"github.com/andrew-bierman/lambe/internal/stateapi"
	"github.com/andrew-bierman/lambe/internal/tracker"
)

// smokeLabels are the named-workflow routing labels under test: the queue
// projection must serve exactly these lanes — nothing off-lane, nothing
// ambiguous-dispatchable.
const (
	routeA = "lambe:smoke-a"
	routeB = "lambe:smoke-b"
)

// bdBin returns the bd binary path, or "" when unavailable. It honors
// LAMBE_TEST_BD as a path and as the CI gate value "1", matching
// test/faultsuite's pattern.
func bdBin() string {
	if v := os.Getenv("LAMBE_TEST_BD"); v != "" && v != "1" {
		return v
	}
	if v, err := exec.LookPath("bd"); err == nil {
		return v
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidate := filepath.Join(home, ".local", "bin", "bd")
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	return ""
}

// needBD skips the test when a real bd is unavailable and returns its path.
func needBD(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("-short skips the dashboard smoke")
	}
	if os.Getenv("LAMBE_TEST_BD") != "1" {
		t.Skip("set LAMBE_TEST_BD=1 for the real-tracker dashboard smoke")
	}
	bin := bdBin()
	if bin == "" {
		t.Skip("bd binary not found; set LAMBE_TEST_BD=1 with bd on PATH")
	}
	return bin
}

// buildGo builds one in-repo package into the test's temp dir, so the
// smoke exercises binaries, not seams.
func buildGo(t *testing.T, pkg, name string) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), name)
	build := exec.Command("go", "build", "-o", bin, pkg) //nolint:gosec // G204: fixed in-repo package paths; output lands in the test's own temp dir
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v: %s", pkg, err, out)
	}
	return bin
}

// processLogBuffer is a concurrency-safe capture of the daemon's output,
// for token-leak and failure-diagnosis assertions.
type processLogBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *processLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *processLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// quoteYAML renders a path as a double-quoted YAML scalar.
func quoteYAML(path string) string {
	return "\"" + strings.ReplaceAll(path, "\\", "\\\\") + "\""
}

// waitFor polls cond until it holds or the deadline expires, failing with
// the daemon's captured logs for diagnosis.
func waitFor(t *testing.T, what string, deadline time.Duration, logs *processLogBuffer, cond func() (bool, error)) {
	t.Helper()
	end := time.Now().Add(deadline)
	for {
		ok, err := cond()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if ok {
			return
		}
		if time.Now().After(end) {
			t.Fatalf("%s: condition not met within %s; daemon logs:\n%s", what, deadline, logs.String())
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// rawRequest performs one request against the surface with an optional
// bearer token; empty means absent. It is the smoke's stand-in for an
// unauthenticated attacker client: the typed stateapi.Client always
// authenticates, so refusal checks use the raw path.
func rawRequest(t *testing.T, method, url, tok string) (int, httpstate.ErrorResponse) {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	var env httpstate.ErrorResponse
	_ = json.NewDecoder(resp.Body).Decode(&env)
	return resp.StatusCode, env
}

// seededTracker is the fixture tracker with its seeded view facts.
type seededTracker struct {
	*tracker.Local
	dir        string
	blocker    tracker.Issue
	depBlocked tracker.Issue
	ambiguous  tracker.Issue
	offLane    tracker.Issue
	work       tracker.Issue
	parked     tracker.Issue
}

// seedTracker creates a disposable bd tracker and seeds the four views'
// deterministic facts. Every issue that must never be dispatched carries
// either no configured route label or an ambiguity the dispatcher skips;
// only `work` is dispatchable.
func seedTracker(t *testing.T, bin string) *seededTracker {
	t.Helper()
	ctx := context.Background()
	project := t.TempDir()
	l, err := tracker.NewLocal(bin, project, "lambe@smoke")
	if err != nil {
		t.Fatalf("tracker NewLocal: %v", err)
	}
	if err := l.Init(ctx); err != nil {
		t.Fatalf("tracker init: %v", err)
	}
	// The parked status must be accepted by bd before the daemon's own
	// idempotent EnsureNeedsHumanStatus runs; the escalation path writes
	// this same configuration.
	cfg := exec.Command(bin, "config", "set", "status.custom", gate.StatusNeedsHuman) //nolint:gosec // G204: fixed seeded bd command
	cfg.Dir = project
	if out, err := cfg.CombinedOutput(); err != nil {
		t.Fatalf("configure needs-human status: %v: %s", err, out)
	}

	create := func(title string, labels []string) tracker.Issue {
		t.Helper()
		issue, err := l.CreateTask(ctx, title, labels)
		if err != nil {
			t.Fatalf("create %s: %v", title, err)
		}
		return issue
	}
	s := &seededTracker{Local: l, dir: project}
	s.blocker = create("blocker: seeded dependency", nil)
	s.depBlocked = create("blocked by seeded dependency", []string{routeA})
	// depBlocked depends on blocker, so bd's readiness reports it blocked
	// with blocker named in blocked_by.
	if err := l.QueueDepAdd(s.depBlocked.ID, s.blocker.ID, ""); err != nil {
		t.Fatalf("seed dependency: %v", err)
	}
	if err := l.FlushWrites(ctx); err != nil {
		t.Fatalf("flush dependency: %v", err)
	}
	s.ambiguous = create("ambiguous route labels", []string{routeA, routeB})
	s.offLane = create("off-lane task that must never be served", []string{"lambe:unrelated"})
	s.work = create("smoke work item", []string{routeA})
	s.parked = create("parked: needs a human", nil)
	// The attention view's facts ride the real escalation write path
	// (the same gate.Hub.NeedsHuman the daemon calls): the status move,
	// the authoritative reason metadata, and the gate-output record.
	hub, err := gate.NewBdHub(bin, project, "lambe@smoke")
	if err != nil {
		t.Fatalf("new gate hub: %v", err)
	}
	if err := hub.NeedsHuman(ctx, s.parked.ID, "seeded escalation: verify gate red", "seeded gate output: verify exited 1"); err != nil {
		t.Fatalf("park issue as needs-human: %v", err)
	}
	return s
}

// workflowFixture writes the shared WORKFLOW.md and the green verify gate
// formula, the same fixture shape cmd/lambe's assembled tests use.
func workflowFixture(t *testing.T, project string) (workflowPath string) {
	t.Helper()
	workflowPath = filepath.Join(project, "WORKFLOW.md")
	if err := os.WriteFile(workflowPath, []byte("---\n---\nWork on {{ issue }}.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	formulas := filepath.Join(project, ".beads", "formulas")
	if err := os.MkdirAll(formulas, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(formulas, "verify.formula.toml"), []byte("[gate]\ntype = \"shell\"\ncommand = \"exit 0\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return workflowPath
}

// smokeConfig renders the enabled daemon config over the fixture.
func smokeConfig(t *testing.T, fakeagentBin, workflowPath, project string) string {
	t.Helper()
	cfgYAML := "harness: acp\n" +
		"acp:\n  command: [" + quoteYAML(fakeagentBin) + ", \"work\"]\n" +
		"supervisor:\n  tick_seconds: 1\n" +
		"workflows:\n" +
		"  - name: smoke-a\n    label: " + routeA + "\n    path: " + quoteYAML(workflowPath) + "\n" +
		"  - name: smoke-b\n    label: " + routeB + "\n    path: " + quoteYAML(workflowPath) + "\n" +
		"trackers:\n  - name: local\n    type: repo\n    path: " + quoteYAML(project) + "\n" +
		"stateapi:\n  enable: true\n  addr: 127.0.0.1:0\n"
	configPath := filepath.Join(t.TempDir(), "lambe.yaml")
	if err := os.WriteFile(configPath, []byte(cfgYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	return configPath
}

// daemonProcess is one started `lambe run` life with its kill guard.
type daemonProcess struct {
	cmd  *exec.Cmd
	logs *processLogBuffer
	done chan error
	// reaped reports that Wait already ran (graceful path), so the
	// cleanup guard must not signal a reaped process.
	reaped bool
}

// startDaemon boots the built binary over its own state dir.
func startDaemon(t *testing.T, bin, configPath, stateDir string) *daemonProcess {
	t.Helper()
	cmd := exec.Command(bin, "run", "-config", configPath) //nolint:gosec // G204: binary built by this test into a temp dir
	cmd.Env = append(os.Environ(), "LAMBE_STATE_DIR="+stateDir)
	logs := &processLogBuffer{}
	cmd.Stdout = logs
	cmd.Stderr = logs
	if err := cmd.Start(); err != nil {
		t.Fatalf("start lambe run: %v", err)
	}
	d := &daemonProcess{cmd: cmd, logs: logs, done: make(chan error, 1)}
	go func() { d.done <- cmd.Wait() }()
	t.Cleanup(d.kill)
	return d
}

// kill is the cleanup guard: SIGKILL whatever is still running, so a
// failed assertion never leaks a daemon.
func (d *daemonProcess) kill() {
	if d.reaped {
		return // stopTerm already waited; done is drained, waiting would hang
	}
	select {
	case err := <-d.done:
		_ = err // already exited; cleanup discards the exit status
		return
	default:
	}
	_ = d.cmd.Process.Signal(syscall.SIGKILL)
	exitErr := <-d.done
	_ = exitErr // reap after SIGKILL; exit status is irrelevant in cleanup
}

// stopTerm drives the graceful shutdown ladder with SIGTERM and fails if
// the daemon does not exit inside the bound.
func (d *daemonProcess) stopTerm(t *testing.T) {
	t.Helper()
	if err := d.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM: %v", err)
	}
	select {
	case err := <-d.done:
		d.reaped = true
		if err != nil {
			t.Fatalf("lambe run exit: %v; logs:\n%s", err, d.logs.String())
		}
	case <-time.After(45 * time.Second):
		t.Fatalf("daemon did not exit after SIGTERM; logs:\n%s", d.logs.String())
	}
}

// descriptorFiles lists the published stateapi descriptors in the live
// dir, returning nothing when the dir does not exist yet.
func descriptorFiles(t *testing.T, stateDir string) []string {
	t.Helper()
	live := filepath.Join(stateDir, "live")
	entries, err := os.ReadDir(live)
	if err != nil {
		if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		return nil
	}
	var matches []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, "stateapi-") && strings.HasSuffix(name, ".json") {
			matches = append(matches, filepath.Join(live, name))
		}
	}
	return matches
}

// resolveEndpoint polls stateapi.Resolve — descriptor enumeration exactly
// like any client, no lock-name math — until the instance publishes.
func resolveEndpoint(t *testing.T, stateDir string, logs *processLogBuffer) stateapi.Endpoint {
	t.Helper()
	ctx := context.Background()
	waitFor(t, "stateapi descriptor publication", 45*time.Second, logs, func() (bool, error) {
		_, err := stateapi.Resolve(ctx, stateDir, stateapi.Selector{})
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, stateapi.ErrNotFound) {
			return false, err
		}
		return false, nil
	})
	if files := descriptorFiles(t, stateDir); len(files) != 1 {
		t.Fatalf("descriptor files = %v, want exactly one live instance", files)
	}
	ep, err := stateapi.Resolve(ctx, stateDir, stateapi.Selector{})
	if err != nil {
		t.Fatalf("resolve after publication: %v", err)
	}
	return ep
}

// assertEndpointFacts checks the resolved descriptor against the process
// it belongs to and the contract's identity facts.
func assertEndpointFacts(t *testing.T, ep stateapi.Endpoint, pid int) {
	t.Helper()
	if ep.PID != pid {
		t.Errorf("descriptor pid = %d, want %d", ep.PID, pid)
	}
	if !strings.HasPrefix(ep.Addr, "127.0.0.1:") {
		t.Errorf("descriptor addr = %q, want literal loopback", ep.Addr)
	}
	if ep.Token == "" {
		t.Error("descriptor carries no bearer token")
	}
	if ep.Version == "" {
		t.Error("descriptor carries no version")
	}
	if _, err := time.Parse(time.RFC3339, ep.StartedAt); err != nil {
		t.Errorf("descriptor started_at %q: %v", ep.StartedAt, err)
	}
}

// assertRefusedEveryRoute proves the private-by-default posture at binary
// level: missing and wrong tokens are refused 401 on every route, refresh
// included, with the uniform envelope.
func assertRefusedEveryRoute(t *testing.T, addr, issueID, realToken string) {
	t.Helper()
	base := "http://" + addr
	for _, rt := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/state"},
		{http.MethodGet, "/api/v1/queue"},
		{http.MethodGet, "/api/v1/history"},
		{http.MethodGet, "/api/v1/" + issueID},
		{http.MethodPost, "/api/v1/refresh"},
	} {
		for name, tok := range map[string]string{"missing": "", "wrong": "not-the-token"} {
			code, env := rawRequest(t, rt.method, base+rt.path, tok)
			if code != http.StatusUnauthorized {
				t.Errorf("%s %s with %s token: status %d, want 401", rt.method, rt.path, name, code)
			}
			if env.SchemaVersion != httpstate.SchemaVersion || env.Error != "unauthorized" || env.Message == "" {
				t.Errorf("%s %s with %s token: envelope %+v", rt.method, rt.path, name, env)
			}
		}
	}
	_ = realToken // readability: the real token is deliberately absent from every call above
}

// TestDashSmoke_AssembledBinaryServesAllViews is the whole proof: the
// built binary, over a disposable tracker and state dir, serves queue,
// attention, history, and run detail with the seeded facts through the
// shared client; refuses unauthenticated calls on every route; and cleans
// its descriptor on shutdown, issuing a fresh token on restart — with the
// finished run's history surviving that restart.
func TestDashSmoke_AssembledBinaryServesAllViews(t *testing.T) {
	bdPath := needBD(t)
	lambeBin := buildGo(t, "./cmd/lambe", "lambe")
	fakeagentBin := buildGo(t, "./internal/harness/acp/fakeagent", "fakeagent")

	seeded := seedTracker(t, bdPath)
	workflowPath := workflowFixture(t, seeded.dir)
	configPath := smokeConfig(t, fakeagentBin, workflowPath, seeded.dir)
	stateDir := t.TempDir()

	d := startDaemon(t, lambeBin, configPath, stateDir)

	// Discovery precedes every attach: enumerate descriptors like any
	// client and validate the facts against the process we started.
	ep := resolveEndpoint(t, stateDir, d.logs)
	assertEndpointFacts(t, ep, d.cmd.Process.Pid)
	if info, err := os.Stat(descriptorFiles(t, stateDir)[0]); err != nil {
		t.Fatal(err)
	} else if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("descriptor mode = %o, want 600 (the token rides in it)", got)
	}

	client, err := stateapi.NewClient(ep, stateapi.WithCacheTTL(0))
	if err != nil {
		t.Fatalf("new stateapi client: %v", err)
	}
	ctx := context.Background()

	// Auth posture first: an unauthenticated or wrong-token caller is
	// refused on every route before any view assertion runs.
	assertRefusedEveryRoute(t, ep.Addr, seeded.work.ID, ep.Token)

	// The refresh POST is an operator action the views never take; the
	// Go harness exercises the route once to prove the authenticated
	// answer, never as a client pattern.
	refreshReq, err := http.NewRequest(http.MethodPost, "http://"+ep.Addr+"/api/v1/refresh", nil)
	if err != nil {
		t.Fatal(err)
	}
	refreshReq.Header.Set("Authorization", "Bearer "+ep.Token)
	refreshResp, err := (&http.Client{Timeout: 5 * time.Second}).Do(refreshReq)
	if err != nil {
		t.Fatalf("authenticated refresh: %v", err)
	}
	refreshBody, err := io.ReadAll(refreshResp.Body) //nolint:gosec // G110: test-owned loopback response
	if err != nil {
		t.Fatal(err)
	}
	_ = refreshResp.Body.Close()
	if refreshResp.StatusCode != http.StatusAccepted {
		t.Errorf("authenticated refresh = %d body %s, want 202", refreshResp.StatusCode, refreshBody)
	}

	// Attention view: the parked item with its real recorded reason, the
	// gate-output detail, and a machine-readable since.
	var snap httpstate.SnapshotResponse
	waitFor(t, "attention view serves the parked item", 45*time.Second, d.logs, func() (bool, error) {
		var err error
		snap, err = client.State(ctx)
		if err != nil {
			return false, err
		}
		if snap.SchemaVersion != httpstate.SchemaVersion {
			return false, fmt.Errorf("snapshot schema_version = %d, want %d", snap.SchemaVersion, httpstate.SchemaVersion)
		}
		for _, nh := range snap.NeedsHuman {
			if nh.Issue == seeded.parked.ID {
				return nh.Reason == "seeded escalation: verify gate red", nil
			}
		}
		return false, nil
	})
	if snap.Daemon.PID != d.cmd.Process.Pid {
		t.Errorf("snapshot daemon pid = %d, want %d", snap.Daemon.PID, d.cmd.Process.Pid)
	}
	if snap.Daemon.Machine == "" {
		t.Error("snapshot daemon block carries no machine identity")
	}
	for _, nh := range snap.NeedsHuman {
		if nh.Issue != seeded.parked.ID {
			continue
		}
		if nh.Detail != "seeded gate output: verify exited 1" {
			t.Errorf("parked detail = %q, want the seeded gate output", nh.Detail)
		}
		if _, err := time.Parse(time.RFC3339, nh.Since); err != nil {
			t.Errorf("parked since %q: %v", nh.Since, err)
		}
	}

	// Queue view: readiness rows for the configured lanes only — the
	// blocked and ambiguous facts, never an off-lane row (bd has been
	// observed to ignore label filters; the shared projection filters
	// them, so a raw wrong-lane row would be the regression here).
	var queue httpstate.QueueResponse
	waitFor(t, "queue view serves the configured lanes", 45*time.Second, d.logs, func() (bool, error) {
		var err error
		queue, err = client.Queue(ctx)
		if err != nil {
			return false, err
		}
		if queue.SchemaVersion != httpstate.SchemaVersion {
			return false, fmt.Errorf("queue schema_version = %d, want %d", queue.SchemaVersion, httpstate.SchemaVersion)
		}
		var amb, dep bool
		for _, row := range queue.Rows {
			assertNoWrongLaneRow(t, row, seeded)
			switch row.ID {
			case seeded.ambiguous.ID:
				amb = row.Blocked && strings.Contains(row.Reason, "ambiguous workflow labels")
			case seeded.depBlocked.ID:
				dep = row.Blocked
			}
		}
		return amb && dep, nil
	})
	for _, row := range queue.Rows {
		if row.ID != seeded.depBlocked.ID {
			continue
		}
		if len(row.Blockers) == 0 || row.Blockers[0].ID != seeded.blocker.ID {
			t.Errorf("blocked row blockers = %+v, want the seeded blocker", row.Blockers)
		}
	}

	// History and run detail: the seeded work item is dispatched, runs
	// through the fake ACP agent, passes the green shell gate, and lands
	// in the merged history with its run detail answering.
	var history httpstate.HistoryResponse
	waitFor(t, "finished run lands in history", 90*time.Second, d.logs, func() (bool, error) {
		var err error
		history, err = client.History(ctx)
		if err != nil {
			return false, err
		}
		if history.SchemaVersion != httpstate.SchemaVersion {
			return false, fmt.Errorf("history schema_version = %d, want %d", history.SchemaVersion, httpstate.SchemaVersion)
		}
		if len(history.Runs) > 100 {
			return false, fmt.Errorf("history carries %d runs, over the 100 bound", len(history.Runs))
		}
		for _, run := range history.Runs {
			if run.Issue == seeded.work.ID && strings.HasPrefix(run.Phase, "stopped:") && run.Attempt >= 1 {
				return true, nil
			}
		}
		return false, nil
	})
	var previous string
	for _, run := range history.Runs {
		if previous != "" && run.UpdatedAt > previous {
			t.Errorf("history not newest-first: %q follows %q", run.UpdatedAt, previous)
		}
		previous = run.UpdatedAt
	}
	var detail httpstate.IssueResponse
	waitFor(t, "run detail answers for the finished run", 30*time.Second, d.logs, func() (bool, error) {
		var err error
		detail, err = client.Issue(ctx, seeded.work.ID)
		if err != nil {
			var status *stateapi.StatusError
			if errors.As(err, &status) && status.Code == http.StatusNotFound {
				return false, nil // not yet dispatched or not yet registered
			}
			return false, err
		}
		return detail.Attempt >= 1, nil
	})
	if detail.Workflow != "smoke-a" {
		t.Errorf("run detail workflow = %q, want the routed workflow", detail.Workflow)
	}
	if detail.State != "settled" {
		t.Errorf("run detail state = %q, want settled", detail.State)
	}
	if detail.Thread == "" {
		t.Error("run detail carries no thread")
	}
	if !strings.HasPrefix(detail.Workspace, filepath.Join(stateDir, "workspaces")) {
		t.Errorf("run detail workspace = %q, want it under the smoke state dir", detail.Workspace)
	}

	// The finished run left the queue front; the off-lane row never
	// appeared on any observation (assertNoWrongLaneRow ran per read).
	uncached, err := stateapi.NewClient(ep, stateapi.WithCacheTTL(0))
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "finished run leaves the queue front", 30*time.Second, d.logs, func() (bool, error) {
		q, err := uncached.Queue(ctx)
		if err != nil {
			return false, err
		}
		for _, row := range q.Rows {
			assertNoWrongLaneRow(t, row, seeded)
			if row.ID == seeded.work.ID {
				return false, nil
			}
		}
		return true, nil
	})

	// The bearer token never appears in the daemon's output.
	if out := d.logs.String(); strings.Contains(out, ep.Token) {
		t.Error("bearer token leaked into daemon output")
	}

	// Token and daemon lifecycle: graceful shutdown removes the
	// descriptor, and a restart publishes a fresh one with a new
	// per-start token — without disturbing the finished run's history.
	d.stopTerm(t)
	if files := descriptorFiles(t, stateDir); len(files) != 0 {
		t.Errorf("descriptor survived graceful shutdown: %v", files)
	}

	restarted := startDaemon(t, lambeBin, configPath, stateDir)
	ep2 := resolveEndpoint(t, stateDir, restarted.logs)
	assertEndpointFacts(t, ep2, restarted.cmd.Process.Pid)
	if ep2.Token == ep.Token {
		t.Error("restart reused the previous start's bearer token")
	}
	client2, err := stateapi.NewClient(ep2, stateapi.WithCacheTTL(0))
	if err != nil {
		t.Fatalf("new client for restarted daemon: %v", err)
	}
	waitFor(t, "history survives the daemon restart", 45*time.Second, restarted.logs, func() (bool, error) {
		h, err := client2.History(ctx)
		if err != nil {
			return false, err
		}
		for _, run := range h.Runs {
			if run.Issue == seeded.work.ID && strings.HasPrefix(run.Phase, "stopped:") {
				return true, nil
			}
		}
		return false, nil
	})
	if _, err := client2.State(ctx); err != nil {
		t.Fatalf("restarted daemon does not serve state: %v", err)
	}

	restarted.stopTerm(t)
	if files := descriptorFiles(t, stateDir); len(files) != 0 {
		t.Errorf("restarted daemon left its descriptor behind: %v", files)
	}
}

// assertNoWrongLaneRow is the invariant every queue observation checks:
// only configured-lane rows are served — the off-lane seed, the
// unlabeled blocker, and the parked issue must never appear.
func assertNoWrongLaneRow(t *testing.T, row httpstate.QueueRowDTO, seeded *seededTracker) {
	t.Helper()
	switch row.ID {
	case seeded.offLane.ID:
		t.Errorf("off-lane row %s (%s) leaked into the queue projection", row.ID, row.Title)
	case seeded.blocker.ID:
		t.Errorf("unlabeled blocker %s leaked into the queue projection", row.ID)
	case seeded.parked.ID:
		t.Errorf("parked needs-human issue %s appeared as a queue row", row.ID)
	}
}
