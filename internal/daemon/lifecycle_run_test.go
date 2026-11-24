package daemon

// Characterization tests for Daemon.Run: startup order, shutdown order, and
// every early-return path, driven only through New/Run and the Wire seam.
// They pin the behavior the Run decomposition must preserve.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/andrew-bierman/lambe/internal/config"
	"github.com/andrew-bierman/lambe/internal/surface/rex"
	"github.com/andrew-bierman/lambe/internal/symphony"
	"github.com/andrew-bierman/lambe/internal/tracker"
)

// logRecords decodes the captured JSON log lines in emission order.
func logRecords(logs *syncBuffer) []map[string]any {
	var recs []map[string]any
	for _, line := range strings.Split(logs.String(), "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) == nil {
			recs = append(recs, rec)
		}
	}
	return recs
}

// logIndex returns the position of the first record with msg, or -1.
func logIndex(recs []map[string]any, msg string) int {
	for i, r := range recs {
		if r["msg"] == msg {
			return i
		}
	}
	return -1
}

// requireLogOrder fails unless every msg appears, in the given order.
func requireLogOrder(t *testing.T, logs *syncBuffer, msgs ...string) {
	t.Helper()
	recs := logRecords(logs)
	prev := -1
	for _, m := range msgs {
		i := logIndex(recs, m)
		if i < 0 {
			t.Fatalf("log %q missing; logs:\n%s", m, logs.String())
		}
		if i <= prev {
			t.Fatalf("log %q out of order; logs:\n%s", m, logs.String())
		}
		prev = i
	}
}

// waitRun waits for a Run result within the shutdown bound.
func waitRun(t *testing.T, runDone <-chan error) error {
	t.Helper()
	select {
	case err := <-runDone:
		return err
	case <-time.After(ShutdownBound + 10*time.Second):
		t.Fatal("daemon Run did not return")
		return nil
	}
}

// requireLockFree fails when the instance lock is still held.
func requireLockFree(t *testing.T, stateDir string, cfg *config.Config) {
	t.Helper()
	probe, err := AcquireStateLock(filepath.Join(stateDir, InstanceLockName(cfg)))
	if err != nil {
		t.Fatalf("lock not released: %v", err)
	}
	_ = probe.Release()
}

func writeWorkflow(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "WORKFLOW.md")
	if err := os.WriteFile(path, []byte("---\nagent:\n  max_concurrent_agents: 1\n---\nWork {{ issue }}"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunStartupAndShutdownOrder(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	cfg := stateAPIConfig(t.TempDir(), true)
	logs := &syncBuffer{}
	cancel, runDone := startTestDaemon(t, testDaemonOptions(stateDir, cfg, logs))
	waitForLog(t, logs, daemonRunningMsg)

	// The live projection is up while the daemon runs.
	if _, err := os.Stat(LivePath(stateDir, cfg)); err != nil {
		t.Errorf("live projection missing while running: %v", err)
	}
	cancel()
	if err := waitRun(t, runDone); err != nil {
		t.Fatalf("run: %v", err)
	}
	requireLogOrder(t, logs,
		"state lock acquired", "stateapi listening", daemonRunningMsg,
		"shutdown: forwarding kill ladder to children", "lambe daemon stopped")

	var running map[string]any
	for _, r := range logRecords(logs) {
		if r["msg"] == daemonRunningMsg {
			running = r
		}
	}
	if running["stateapi_addr"] != stateAPIListenAddr(t, logs) {
		t.Errorf("running record stateapi_addr = %v, want the listening addr", running["stateapi_addr"])
	}
	if running["trackers"] != float64(1) || running["machine"] != "testbox" || running["actor"] != "lambe@test" || running["tick_seconds"] != float64(10) {
		t.Errorf("running record attrs = %v", running)
	}
	if _, err := os.Stat(StateAPIPath(stateDir, cfg)); !os.IsNotExist(err) {
		t.Errorf("descriptor survived shutdown: %v", err)
	}
	requireLockFree(t, stateDir, cfg)
}

func TestRunLockHeldByAnotherHolderTouchesNothing(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	cfg := stateAPIConfig(t.TempDir(), true)
	holder, err := AcquireStateLock(filepath.Join(stateDir, InstanceLockName(cfg)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Release() }()

	logs := &syncBuffer{}
	d, err := New(testDaemonOptions(stateDir, cfg, logs))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Run(context.Background()); !errors.Is(err, ErrLockHeld) {
		t.Fatalf("run error = %v, want ErrLockHeld", err)
	}
	if got := logs.String(); got != "" {
		t.Errorf("a refused start must log nothing; got:\n%s", got)
	}
	for _, p := range []string{LivePath(stateDir, cfg), StateAPIPath(stateDir, cfg)} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s created by a refused start: %v", p, err)
		}
	}
	// The holder is unaffected: still locked, pid hint intact.
	if _, err := AcquireStateLock(filepath.Join(stateDir, InstanceLockName(cfg))); !errors.Is(err, ErrLockHeld) {
		t.Errorf("holder lost its lock: %v", err)
	}
	if _, err := os.Stat(holder.pidPath); err != nil {
		t.Errorf("holder pid hint removed: %v", err)
	}
}

func TestRunTrackerInitFailureReleasesLockBeforeSurfaces(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	cfg := stateAPIConfig(t.TempDir(), true)
	logs := &syncBuffer{}
	opts := testDaemonOptions(stateDir, cfg, logs)
	opts.Wire = nil
	opts.BdBin = filepath.Join(t.TempDir(), "no-such-bd")
	d, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	err = d.Run(context.Background())
	if err == nil || !strings.HasPrefix(err.Error(), "init tracker local: ") {
		t.Fatalf("run error = %v, want init tracker failure", err)
	}
	recs := logRecords(logs)
	if logIndex(recs, "state lock acquired") < 0 {
		t.Error("lock must be acquired before trackers open")
	}
	for _, m := range []string{"stateapi listening", daemonRunningMsg} {
		if logIndex(recs, m) >= 0 {
			t.Errorf("%q logged despite tracker failure", m)
		}
	}
	for _, p := range []string{LivePath(stateDir, cfg), StateAPIPath(stateDir, cfg)} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s created before trackers opened: %v", p, err)
		}
	}
	requireLockFree(t, stateDir, cfg)
}

func TestRunNoTrackersConfigured(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	cfg := stateAPIConfig(t.TempDir(), false)
	cfg.Trackers = nil
	opts := testDaemonOptions(stateDir, cfg, nil)
	opts.Wire = nil
	d, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Run(context.Background()); err == nil || err.Error() != "no trackers configured" {
		t.Fatalf("run error = %v", err)
	}
	requireLockFree(t, stateDir, cfg)
}

func TestNewRejectsInvalidOptions(t *testing.T) {
	t.Parallel()
	cfg := stateAPIConfig(t.TempDir(), false)
	for _, tc := range []struct {
		name string
		opts Options
		want string
	}{
		{"no config", Options{StateDir: "s", Machine: "m"}, "daemon: config is required"},
		{"no state dir", Options{Config: cfg, Machine: "m"}, "daemon: state dir is required"},
		{"no machine", Options{Config: cfg, StateDir: "s"}, "daemon: machine identity is required"},
		{"wire fails", Options{Config: cfg, StateDir: "s", Machine: "m", Wire: func(*Assembly) error { return errors.New("boom") }}, "wire assembly: boom"},
	} {
		if _, err := New(tc.opts); err == nil || err.Error() != tc.want {
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
}

func TestRunWithPreCanceledContextStopsCleanly(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	cfg := stateAPIConfig(t.TempDir(), true)
	logs := &syncBuffer{}
	d, err := New(testDaemonOptions(stateDir, cfg, logs))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- d.Run(ctx) }()
	if err := waitRun(t, runDone); err != nil {
		t.Fatalf("run: %v", err)
	}
	requireLogOrder(t, logs, "state lock acquired", daemonRunningMsg, "lambe daemon stopped")
	if _, err := os.Stat(StateAPIPath(stateDir, cfg)); !os.IsNotExist(err) {
		t.Errorf("descriptor survived: %v", err)
	}
	requireLockFree(t, stateDir, cfg)
}

func TestRunStateAPIDisabledStartsNothingAndOmitsAddr(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	cfg := stateAPIConfig(t.TempDir(), false)
	logs := &syncBuffer{}
	cancel, runDone := startTestDaemon(t, testDaemonOptions(stateDir, cfg, logs))
	waitForLog(t, logs, daemonRunningMsg)
	cancel()
	if err := waitRun(t, runDone); err != nil {
		t.Fatal(err)
	}
	recs := logRecords(logs)
	if logIndex(recs, "stateapi listening") >= 0 {
		t.Error("disabled surface logged listening")
	}
	for _, r := range recs {
		if r["msg"] == daemonRunningMsg {
			if _, ok := r["stateapi_addr"]; ok {
				t.Error("running record advertises an addr with the surface disabled")
			}
		}
	}
}

func TestRunWorkflowRouteLoadFailureTearsDownSurface(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	cfg := stateAPIConfig(t.TempDir(), true)
	cfg.Workflows = []config.NamedWorkflow{{Name: "docs", Label: "docs", Path: filepath.Join(t.TempDir(), "missing.md")}}
	opts := testDaemonOptions(stateDir, cfg, nil)
	opts.WorkflowPath = writeWorkflow(t, t.TempDir())
	d, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	err = d.Run(context.Background())
	if err == nil || !strings.HasPrefix(err.Error(), "load workflow docs: ") {
		t.Fatalf("run error = %v, want route load failure", err)
	}
	if _, err := os.Stat(StateAPIPath(stateDir, cfg)); !os.IsNotExist(err) {
		t.Errorf("descriptor survived: %v", err)
	}
	requireLockFree(t, stateDir, cfg)
}

func TestRunWorkspaceRootChangeIsFatalAndTearsDown(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	cfg := stateAPIConfig(t.TempDir(), true)
	cfg.WorkspaceRoot = filepath.Join(t.TempDir(), "new-root")
	opts := testDaemonOptions(stateDir, cfg, nil)
	opts.WorkflowPath = writeWorkflow(t, t.TempDir())

	issue, err := json.Marshal(tracker.Issue{ID: "i-1"})
	if err != nil {
		t.Fatal(err)
	}
	wf, err := json.Marshal(symphony.Workflow{MaxTurns: 1})
	if err != nil {
		t.Fatal(err)
	}
	saved := `{"version":1,"phase":"running","issue":` + string(issue) + `,"workflow":` + string(wf) +
		`,"key":"i-1","workspace":"/elsewhere/i-1"}`
	dir := filepath.Join(stateDir, "sessions", strings.TrimSuffix(InstanceLockName(cfg), ".lock"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "i-1.json"), []byte(saved), 0o600); err != nil {
		t.Fatal(err)
	}

	d, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Run(context.Background()); !errors.Is(err, symphony.ErrWorkspaceRootChanged) {
		t.Fatalf("run error = %v, want ErrWorkspaceRootChanged", err)
	}
	if _, err := os.Stat(StateAPIPath(stateDir, cfg)); !os.IsNotExist(err) {
		t.Errorf("descriptor survived: %v", err)
	}
	requireLockFree(t, stateDir, cfg)
}

func TestRunMalformedFormulaWarnsAndContinues(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	trackerDir := t.TempDir()
	formulas := filepath.Join(trackerDir, ".beads", "formulas")
	if err := os.MkdirAll(formulas, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(formulas, "bad.formula.toml"), []byte("[trigger\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := stateAPIConfig(trackerDir, false)
	logs := &syncBuffer{}
	opts := testDaemonOptions(stateDir, cfg, logs)
	opts.WorkflowPath = writeWorkflow(t, t.TempDir())
	cancel, runDone := startTestDaemon(t, opts)
	waitForLog(t, logs, daemonRunningMsg)
	cancel()
	if err := waitRun(t, runDone); err != nil {
		t.Fatal(err)
	}
	requireLogOrder(t, logs, "formula loading failed; running without responsibilities", daemonRunningMsg, "lambe daemon stopped")
}

func TestRunAutoReclaimIsLoggedBeforeRunning(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	cfg := stateAPIConfig(t.TempDir(), false)
	cfg.Supervisor.AutoReclaim = true
	cfg.Supervisor.ReclaimGrace = 90 * time.Second
	logs := &syncBuffer{}
	cancel, runDone := startTestDaemon(t, testDaemonOptions(stateDir, cfg, logs))
	waitForLog(t, logs, daemonRunningMsg)
	cancel()
	if err := waitRun(t, runDone); err != nil {
		t.Fatal(err)
	}
	requireLogOrder(t, logs, "auto reclaim enabled", daemonRunningMsg)
	for _, r := range logRecords(logs) {
		if r["msg"] == "auto reclaim enabled" && r["grace"] != "1m30s" {
			t.Errorf("grace = %v", r["grace"])
		}
	}
}

// The signal tests send process-wide signals, so they stay serial: parallel
// daemons only start after every serial test has finished.

func TestRunSIGINTRunsShutdown(t *testing.T) {
	stateDir := t.TempDir()
	cfg := stateAPIConfig(t.TempDir(), true)
	logs := &syncBuffer{}
	_, runDone := startTestDaemon(t, testDaemonOptions(stateDir, cfg, logs))
	waitForLog(t, logs, daemonRunningMsg)
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	if err := waitRun(t, runDone); err != nil {
		t.Fatalf("run: %v", err)
	}
	requireLogOrder(t, logs, daemonRunningMsg, "shutdown: forwarding kill ladder to children", "lambe daemon stopped")
	if _, err := os.Stat(StateAPIPath(stateDir, cfg)); !os.IsNotExist(err) {
		t.Errorf("descriptor survived: %v", err)
	}
	requireLockFree(t, stateDir, cfg)
}

func TestRunSIGHUPKeepsConfigOnInvalidReload(t *testing.T) {
	stateDir := t.TempDir()
	trackerDir := t.TempDir()
	cfgFile := filepath.Join(t.TempDir(), "lambe.yaml")
	write := func(content string) {
		t.Helper()
		if err := os.WriteFile(cfgFile, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("trackers:\n  - name: main\n    type: repo\n    path: " + trackerDir + "\n")
	cfg, err := config.Load(config.Options{File: cfgFile})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Supervisor.HeartbeatInterval = time.Hour
	logs := &syncBuffer{}
	opts := testDaemonOptions(stateDir, cfg, logs)
	opts.ConfigPath = cfgFile
	cancel, runDone := startTestDaemon(t, opts)
	waitForLog(t, logs, daemonRunningMsg)

	hup := func(wantMsg string) {
		t.Helper()
		if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
			t.Fatal(err)
		}
		waitForLog(t, logs, wantMsg)
	}
	// A valid reload is deliberately not driven: reload swaps the running config
	// while the heartbeat goroutine reads it, a known data race under -race.
	write("trackers: [unterminated\n")
	hup("config reload rejected, keeping running config")

	select {
	case err := <-runDone:
		t.Fatalf("SIGHUP must not stop the daemon: %v", err)
	default:
	}
	cancel()
	if err := waitRun(t, runDone); err != nil {
		t.Fatal(err)
	}
	requireLogOrder(t, logs, "reload requested", "config reload rejected, keeping running config", "lambe daemon stopped")
}

// clearProbe is a Rex output writer that, at the moment the shutdown clear
// sequence is written, records the surrounding teardown state.
type clearProbe struct {
	stateDir string
	cfg      *config.Config
	mu       sync.Mutex
	cleared  bool
	descGone bool
	lockHeld bool
}

func (p *clearProbe) Write(b []byte) (int, error) {
	if strings.Contains(string(b), "state=clear") {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.cleared = true
		_, err := os.Stat(StateAPIPath(p.stateDir, p.cfg))
		p.descGone = os.IsNotExist(err)
		probe, lockErr := AcquireStateLock(filepath.Join(p.stateDir, InstanceLockName(p.cfg)))
		if probe != nil {
			_ = probe.Release()
		}
		p.lockHeld = errors.Is(lockErr, ErrLockHeld)
	}
	return len(b), nil
}

// TestRunShutdownClearsRexSurfaceAfterStateAPIBeforeLockRelease pins the
// teardown order: the state surface descriptor is removed first, then the Rex
// badges are cleared and only then does the
// instance lock return.
func TestRunShutdownClearsRexSurfaceAfterStateAPIBeforeLockRelease(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	cfg := stateAPIConfig(t.TempDir(), true)
	probe := &clearProbe{stateDir: stateDir, cfg: cfg}
	opts := testDaemonOptions(stateDir, cfg, nil)
	opts.Wire = func(a *Assembly) error {
		a.Trackers = []tracker.Tracker{newFakeTracker()}
		a.Surface = rex.NewAdapter(rex.Availability{Available: true}, &fakeRexClient{}, probe)
		return nil
	}
	cancel, runDone := startTestDaemon(t, opts)
	waitForDescriptor(t, StateAPIPath(stateDir, cfg), 15*time.Second)
	cancel()
	if err := waitRun(t, runDone); err != nil {
		t.Fatal(err)
	}
	probe.mu.Lock()
	defer probe.mu.Unlock()
	if !probe.cleared {
		t.Fatal("shutdown never cleared the Rex surface")
	}
	if !probe.descGone {
		t.Error("clear ran while the state descriptor was still advertised")
	}
	if !probe.lockHeld {
		t.Error("clear ran after the instance lock was released")
	}
	requireLockFree(t, stateDir, cfg)
}

// TestReloadValidConfigRacesWithBackgroundReaders drives a valid reload
// repeatedly while the heartbeat goroutine re-reads the interval; under
// -race it fails if reload publishes the new config unsynchronized.
func TestReloadValidConfigRacesWithBackgroundReaders(t *testing.T) {
	stateDir := t.TempDir()
	trackerDir := t.TempDir()
	cfgFile := filepath.Join(t.TempDir(), "lambe.yaml")
	body := "supervisor:\n  heartbeat_interval: 1ms\ntrackers:\n  - name: main\n    type: repo\n    path: " + trackerDir + "\n"
	if err := os.WriteFile(cfgFile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.Options{File: cfgFile})
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	opts := testDaemonOptions(stateDir, cfg, logs)
	opts.ConfigPath = cfgFile
	d, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- d.Run(ctx) }()
	waitForLog(t, logs, daemonRunningMsg)

	for range 200 {
		d.reload()
		_ = d.StateSource() // reads the config on the caller's goroutine too
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := waitRun(t, runDone); err != nil {
		t.Fatal(err)
	}
	if got := d.config(); got == cfg {
		t.Error("reload did not publish the new config")
	}
}
