package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/andrew-bierman/lambe/internal/symphony"
	"github.com/andrew-bierman/lambe/internal/tracker"
)

// These tests drive the real lambe binary against an isolated Beads database,
// state directory, and config, with the in-tree fake ACP agent as the harness.
// Nothing here touches a production daemon: HOME, the state directory, the
// workspace root, and the tracker are all temporary.

var (
	buildOnce sync.Once
	lambeBin  string
	agentBin  string
	buildErr  error
)

// buildBinaries compiles lambe and the fake agent once per test run.
func buildBinaries(t *testing.T) {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "lambe-e2e-bin")
		if err != nil {
			buildErr = err
			return
		}
		lambeBin = filepath.Join(dir, "lambe")
		agentBin = filepath.Join(dir, "fakeagent")
		for out, pkg := range map[string]string{lambeBin: "./cmd/lambe", agentBin: "./internal/harness/acp/fakeagent"} {
			cmd := exec.Command("go", "build", "-o", out, pkg) // #nosec G204 -- fixed in-tree packages
			cmd.Dir = "../.."
			if b, err := cmd.CombinedOutput(); err != nil {
				buildErr = fmt.Errorf("build %s: %w: %s", pkg, err, b)
				return
			}
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
}

// supervisionRig is one isolated lambe deployment.
type supervisionRig struct {
	t       *testing.T
	root    string
	repo    string
	cfg     string
	env     []string
	local   *tracker.Local
	issueID string
}

// newSupervisionRig builds the temp tracker, config, workflow, and gate. The
// fake agent's behavior comes from extraEnv (LAMBE_FAKE_*).
func newSupervisionRig(t *testing.T, gate string, extraEnv ...string) *supervisionRig {
	t.Helper()
	if os.Getenv("LAMBE_TEST_BD") != "1" {
		t.Skip("set LAMBE_TEST_BD=1 for real tracker integration")
	}
	buildBinaries(t)
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	for _, d := range []string{repo, filepath.Join(root, "state"), filepath.Join(root, "ws"), filepath.Join(root, "fake"), filepath.Join(root, "home")} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("bd", "init", "--prefix", "sup", "--non-interactive")
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "HOME="+filepath.Join(root, "home"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bd init: %v: %s", err, out)
	}
	formulas := filepath.Join(repo, ".beads", "formulas")
	if err := os.MkdirAll(formulas, 0o750); err != nil {
		t.Fatal(err)
	}
	write := func(path, body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(formulas, "done.formula.toml"), "[gate]\ntype = \"shell\"\ncommand = "+strconv.Quote(gate)+"\n")
	write(filepath.Join(repo, "WORKFLOW.md"), "---\n---\nWork {{ issue }}")
	cfg := filepath.Join(root, "lambe.yaml")
	write(cfg, fmt.Sprintf(`harness: acp
acp:
  command: [%q, e2e]
workflow_path: %q
trackers:
  - name: local
    type: repo
    path: %q
supervisor:
  tick_seconds: 1
  silence_timeout: 1s
  turn_timeout: 1m
  heartbeat_interval: 30s
`, agentBin, filepath.Join(repo, "WORKFLOW.md"), repo))

	env := append(os.Environ(),
		"HOME="+filepath.Join(root, "home"),
		"XDG_CONFIG_HOME="+filepath.Join(root, "home"),
		"LAMBE_STATE_DIR="+filepath.Join(root, "state"),
		"LAMBE_WORKSPACE_ROOT="+filepath.Join(root, "ws"),
		"LAMBE_FAKE_DIR="+filepath.Join(root, "fake"),
	)
	env = append(env, extraEnv...)
	local, err := tracker.NewLocal("", repo, "lambe@e2e")
	if err != nil {
		t.Fatal(err)
	}
	issue, err := local.EnsureIssue(t.Context(), "sup-work", tracker.IssueSpec{Title: "produce done.txt", Priority: 2})
	if err != nil {
		t.Fatal(err)
	}
	return &supervisionRig{t: t, root: root, repo: repo, cfg: cfg, env: env, local: local, issueID: issue.ID}
}

// start launches one daemon life and returns its command.
func (r *supervisionRig) start(life string) *exec.Cmd {
	r.t.Helper()
	logFile, err := os.Create(filepath.Join(r.root, life+".log")) // #nosec G304 -- temp dir
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { _ = logFile.Close() })
	cmd := exec.Command(lambeBin, "run", "-config", r.cfg) // #nosec G204 -- binary built by this test
	cmd.Env = r.env
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
	return cmd
}

// spawns reads the fake agent's per-prompt records: pid and start time.
func (r *supervisionRig) spawns() (pids []int, at []time.Time) {
	f, err := os.Open(filepath.Join(r.root, "fake", "spawns.log")) // #nosec G304 -- temp dir
	if err != nil {
		return nil, nil
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var pid int
		var ns int64
		if n, _ := fmt.Sscanf(sc.Text(), "%d %d", &pid, &ns); n == 2 {
			pids = append(pids, pid)
			at = append(at, time.Unix(0, ns))
		}
	}
	return pids, at
}

func (r *supervisionRig) waitSpawns(n int, within time.Duration) {
	r.t.Helper()
	r.waitFor(within, 100*time.Millisecond, fmt.Sprintf("%d agent prompts", n), func() bool { p, _ := r.spawns(); return len(p) >= n })
}

func (r *supervisionRig) waitClosed(within time.Duration) {
	r.t.Helper()
	// Poll the tracker slowly: bd takes an exclusive embedded-database lock,
	// and a tight poll starves the daemon under test.
	r.waitFor(within, 3*time.Second, "issue closed through its gate", func() bool {
		got, err := r.local.Dispatchable(context.Background(), tracker.DispatchQuery{IDs: []string{r.issueID}, Statuses: []string{tracker.StatusClosed}})
		return err == nil && len(got) == 1
	})
}

func (r *supervisionRig) waitFor(within, every time.Duration, what string, ok func() bool) {
	r.t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(every)
	}
	logs, _ := filepath.Glob(filepath.Join(r.root, "*.log"))
	var dump bytes.Buffer
	for _, l := range logs {
		b, _ := os.ReadFile(l) // #nosec G304 -- temp dir
		fmt.Fprintf(&dump, "--- %s\n%s\n", filepath.Base(l), b)
	}
	r.t.Fatalf("timed out after %s waiting for %s\n%s", within, what, dump.String())
}

func (r *supervisionRig) closedCount() int {
	got, _ := r.local.Dispatchable(context.Background(), tracker.DispatchQuery{Statuses: []string{tracker.StatusClosed}})
	n := 0
	for _, i := range got {
		if i.ID == r.issueID {
			n++
		}
	}
	return n
}

func (r *supervisionRig) logText() string {
	var all strings.Builder
	logs, _ := filepath.Glob(filepath.Join(r.root, "*.log"))
	for _, l := range logs {
		b, _ := os.ReadFile(l) // #nosec G304 -- temp dir
		all.Write(b)
	}
	return all.String()
}

func pidAlive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func stopDaemon(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	_ = cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done: //nolint:errcheck // exit status after SIGTERM is not under test
	case <-time.After(35 * time.Second):
		t.Fatal("daemon ignored SIGTERM")
	}
}

// A tool call that is quiet for three silence windows is alive, not stalled:
// the run is not killed and the issue closes through its shell gate.
func TestE2EQuietLongToolCallCompletesThroughGate(t *testing.T) {
	t.Parallel()
	rig := newSupervisionRig(t, "test -f done.txt", "LAMBE_FAKE_QUIET=3500ms")
	daemonCmd := rig.start("only")
	rig.waitClosed(300 * time.Second)
	stopDaemon(t, daemonCmd)
	if pids, _ := rig.spawns(); len(pids) != 1 {
		t.Fatalf("quiet tool call was killed and re-dispatched: %d prompts", len(pids))
	}
	if strings.Contains(rig.logText(), "watchdog") {
		t.Fatalf("watchdog fired on a healthy quiet tool:\n%s", rig.logText())
	}
}

// A silent agent with no tool in flight is killed and the issue is retried
// after the failure backoff; the retry then completes through the gate.
func TestE2EStalledRunKilledAndRetriedWithBackoff(t *testing.T) {
	t.Parallel()
	rig := newSupervisionRig(t, "test -f done.txt", "LAMBE_FAKE_STALL_FIRST=1", "LAMBE_FAKE_QUIET=0s")
	daemonCmd := rig.start("only")
	rig.waitClosed(300 * time.Second)
	stopDaemon(t, daemonCmd)
	pids, at := rig.spawns()
	if len(pids) != 2 {
		t.Fatalf("want one stalled run and one retry, got %d prompts", len(pids))
	}
	if pidAlive(pids[0]) {
		t.Fatalf("stalled agent %d survived the kill ladder", pids[0])
	}
	if gap := at[1].Sub(at[0]); gap < symphony.FailureRetryBase {
		t.Fatalf("retry after %s, want at least the %s failure backoff", gap, symphony.FailureRetryBase)
	}
	attempts, err := rig.local.Attempts(t.Context(), rig.issueID)
	if err != nil || len(attempts) < 2 {
		t.Fatalf("history must keep both attempts: %+v err=%v", attempts, err)
	}
}

// A daemon killed mid-run, by SIGTERM or SIGKILL, comes back to the same
// work: the old agent is gone before the next prompt, attempt history is
// kept, and the issue is dispatched once more rather than twice.
func TestE2EDaemonRestartPreservesHistoryWithoutDuplicateDispatch(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
		t.Run(sig.String(), func(t *testing.T) {
			t.Parallel()
			rig := newSupervisionRig(t, "test -f done.txt", "LAMBE_FAKE_QUIET=8s", "LAMBE_FAKE_SURVIVE=1")
			first := rig.start("first")
			rig.waitSpawns(1, 300*time.Second)
			pids, _ := rig.spawns()
			if sig == syscall.SIGKILL {
				_ = first.Process.Kill()
				_ = first.Wait()
				// Lambe sets no parent-death signal: an orphaned agent outlives its
				// daemon and the next life must reap it. The fake agent ignores
				// SIGPIPE so this holds on macOS and Linux alike.
				time.Sleep(300 * time.Millisecond) // an agent that dies of the broken pipe does so within milliseconds
				if !pidAlive(pids[0]) {
					t.Fatalf("agent %d died with the daemon; orphan reaping is untested", pids[0])
				}
			} else {
				stopDaemon(t, first)
			}
			before, err := rig.local.Attempts(t.Context(), rig.issueID)
			if err != nil || len(before) == 0 {
				t.Fatalf("no attempt history survived the %s: %+v err=%v", sig, before, err)
			}
			second := rig.start("second")
			rig.waitSpawns(2, 300*time.Second)
			if pidAlive(pids[0]) {
				t.Fatalf("recovery prompted while the old agent %d was still alive", pids[0])
			}
			rig.waitClosed(300 * time.Second)
			stopDaemon(t, second)
			if all, _ := rig.spawns(); len(all) != 2 {
				t.Fatalf("duplicate dispatch: %d prompts, want 2 (one per daemon life)", len(all))
			}
			after, err := rig.local.Attempts(t.Context(), rig.issueID)
			if err != nil || len(after) < len(before) || after[0].Number != before[0].Number {
				t.Fatalf("history rewritten by restart: before=%+v after=%+v err=%v", before, after, err)
			}
			if rig.closedCount() != 1 {
				t.Fatal("issue not closed exactly once")
			}
			out, err := exec.Command(lambeBin, "status", "-config", rig.cfg).CombinedOutput() // #nosec G204 -- test binary
			if err != nil {
				t.Fatalf("lambe status after restart: %v: %s", err, out)
			}
		})
	}
}
