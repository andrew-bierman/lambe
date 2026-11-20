package daemon

// This file drives the wired HTTP state surface through the assembled
// daemon — the dead-seam regression tests for the stateapi wiring. The
// DefaultWire variant below additionally constructs the daemon the way
// `lambe run` does, and cmd/lambe's test invokes the built binary; per the
// repo learning, the wiring unit's tests must drive the assembled path,
// not re-fixture the seam.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/andrew-bierman/lambe/internal/config"
	"github.com/andrew-bierman/lambe/internal/httpstate"
	"github.com/andrew-bierman/lambe/internal/proc"
	"github.com/andrew-bierman/lambe/internal/tracker"
)

// slogDiscard returns a quiet logger for daemon lives whose logs are not
// assertions.
func slogDiscard() *slog.Logger { return slog.New(slog.DiscardHandler) }

// slogJSON returns a logger capturing its JSON records into buf.
func slogJSON(buf *syncBuffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, nil))
}

// stateAPIConfig returns a minimal enabled-or-disabled stateapi config
// over one tracker directory.
func stateAPIConfig(trackerDir string, enable bool) *config.Config {
	return &config.Config{
		Supervisor: config.Supervisor{
			TickSeconds:       10,
			AttemptCap:        8,
			SilenceTimeout:    time.Minute,
			TurnTimeout:       2 * time.Minute,
			HeartbeatInterval: time.Hour,
		},
		Trackers: []config.Tracker{{Name: "local", Type: "repo", Path: trackerDir}},
		StateAPI: config.StateAPI{Enable: enable, Addr: "127.0.0.1:0"},
	}
}

// testDaemonOptions assembles a daemon over fake trackers the way tests
// elsewhere in the package do, with a capture logger for token-leak
// assertions.
func testDaemonOptions(stateDir string, cfg *config.Config, log *syncBuffer) Options {
	logger := slogDiscard()
	if log != nil {
		logger = slogJSON(log)
	}
	return Options{
		Config:       cfg,
		StateDir:     stateDir,
		Machine:      "testbox",
		Actor:        "lambe@test",
		LockName:     InstanceLockName(cfg),
		Version:      "test-version",
		Log:          logger,
		WorkflowPath: "", // tick cadence only; the surface reads registry/reports
		Wire: func(a *Assembly) error {
			a.Trackers = []tracker.Tracker{newFakeTracker()}
			return nil
		},
	}
}

// startTestDaemon runs a daemon life until the test's context is
// canceled, failing the test when the run returns early.
func startTestDaemon(t *testing.T, opts Options) (context.CancelFunc, <-chan error) {
	t.Helper()
	d, err := New(opts)
	if err != nil {
		t.Fatalf("new daemon: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- d.Run(ctx) }()
	t.Cleanup(cancel)
	return cancel, runDone
}

// waitForLock polls until the daemon's pid hint proves the state lock is
// held.
func waitForLock(t *testing.T, stateDir string, cfg *config.Config) {
	t.Helper()
	hint := filepath.Join(stateDir, InstanceLockName(cfg)) + ".pid"
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(hint); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("daemon never took the state lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitForDescriptor polls until the per-instance descriptor is published
// and returns it, decoded.
func waitForDescriptor(t *testing.T, path string, deadline time.Duration) StateAPIDescriptor {
	t.Helper()
	end := time.Now().Add(deadline)
	for {
		data, err := os.ReadFile(path) // #nosec G304 -- test-owned temporary path
		if err == nil {
			var got StateAPIDescriptor
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("decode descriptor: %v", err)
			}
			return got
		}
		if !os.IsNotExist(err) && time.Now().After(end) {
			t.Fatalf("descriptor at %s never published: %v", path, err)
		}
		if time.Now().After(end) {
			t.Fatalf("descriptor at %s never published (wiring regression?)", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// stateAPIRequest performs one request against the surface with an
// optional bearer token and Host override; empty means absent.
func stateAPIRequest(t *testing.T, method, url, token, host string) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if host != "" {
		req.Host = host
	}
	return (&http.Client{Timeout: 5 * time.Second}).Do(req)
}

// TestStateAPIDisabledByDefaultStartsNoListener pins the opt-in doctrine:
// without stateapi.enable nothing listens and nothing is advertised, and
// the daemon runs its normal life.
func TestStateAPIDisabledByDefaultStartsNoListener(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	cfg := stateAPIConfig(t.TempDir(), false)
	cancel, runDone := startTestDaemon(t, testDaemonOptions(stateDir, cfg, nil))
	waitForLock(t, stateDir, cfg)
	time.Sleep(300 * time.Millisecond)

	glob, err := filepath.Glob(filepath.Join(stateDir, "live", "stateapi-*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(glob) != 0 {
		t.Errorf("disabled surface published descriptors: %v", glob)
	}

	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("daemon run without the surface: %v", err)
		}
	case <-time.After(ShutdownBound + 5*time.Second):
		t.Fatal("daemon did not exit within the shutdown bound")
	}
}

// TestStateAPIServesBearerGuardedRoutes is the assembled-path test: an
// enabled daemon serves /api/v1 on an ephemeral loopback port, guards
// every route behind the per-start token, publishes a 0600 descriptor
// carrying addr + token + identity facts, removes it on graceful shutdown
// before the lock is released, and never leaks the token into logs.
func TestStateAPIServesBearerGuardedRoutes(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	cfg := stateAPIConfig(t.TempDir(), true)
	logs := &syncBuffer{}
	cancel, runDone := startTestDaemon(t, testDaemonOptions(stateDir, cfg, logs))

	lockPath := filepath.Join(stateDir, InstanceLockName(cfg))
	waitForLock(t, stateDir, cfg)
	descPath := StateAPIPath(stateDir, cfg)
	desc := waitForDescriptor(t, descPath, 15*time.Second)

	// The descriptor is written only after the lock: the pid hint already
	// proves the lock, and the descriptor mode carries the token at 0600.
	info, err := os.Stat(descPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("descriptor mode = %o, want 600", got)
	}
	if desc.SchemaVersion != StateAPIDescriptorSchemaVersion {
		t.Errorf("descriptor schema_version = %d, want %d", desc.SchemaVersion, StateAPIDescriptorSchemaVersion)
	}
	if desc.Instance != strings.TrimSuffix(InstanceLockName(cfg), ".lock") {
		t.Errorf("descriptor instance = %q, want the derived instance name", desc.Instance)
	}
	if desc.PID != os.Getpid() {
		t.Errorf("descriptor pid = %d, want %d", desc.PID, os.Getpid())
	}
	if desc.Version != "test-version" || desc.ConfigPath != "" {
		t.Errorf("descriptor identity facts = version %q config %q", desc.Version, desc.ConfigPath)
	}
	// Identity rides along wherever the platform can provide it.
	if _, idErr := proc.ProcessIdentity(os.Getpid()); idErr == nil && desc.Identity == "" {
		t.Errorf("descriptor identity empty although the platform provides one")
	}
	if _, err := time.Parse(time.RFC3339, desc.StartedAt); err != nil {
		t.Errorf("started_at %q: %v", desc.StartedAt, err)
	}
	if !strings.HasPrefix(desc.Addr, "127.0.0.1:") {
		t.Fatalf("descriptor addr = %q, want ephemeral loopback", desc.Addr)
	}

	base := "http://" + desc.Addr

	// With the descriptor's token, GET state answers schema_version 1.
	resp, err := stateAPIRequest(t, "GET", base+"/api/v1/state", desc.BearerToken, "")
	if err != nil {
		t.Fatalf("authenticated GET state: %v", err)
	}
	var snap httpstate.SnapshotResponse
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authenticated GET state: status %d body %s", resp.StatusCode, body)
	}
	if err := json.Unmarshal(body, &snap); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	if snap.SchemaVersion != 1 {
		t.Errorf("schema_version = %d, want 1", snap.SchemaVersion)
	}

	// Every route refuses missing and wrong tokens with the same 401
	// envelope — the future queue/history reads included, so no route
	// exception can slip in later.
	for _, rt := range []struct{ method, path string }{
		{"GET", "/api/v1/state"},
		{"GET", "/api/v1/unknown-issue"},
		{"POST", "/api/v1/refresh"},
		{"GET", "/api/v1/queue"},
		{"GET", "/api/v1/history"},
	} {
		for name, token := range map[string]string{"missing": "", "wrong": "not-the-token"} {
			resp, err := stateAPIRequest(t, rt.method, base+rt.path, token, "")
			if err != nil {
				t.Fatalf("%s %s with %s token: %v", rt.method, rt.path, name, err)
			}
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("%s %s with %s token: status %d, want 401", rt.method, rt.path, name, resp.StatusCode)
			}
			var got httpstate.ErrorResponse
			if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
				t.Fatalf("%s %s: decode 401 envelope: %v", rt.method, rt.path, err)
			}
			if got.SchemaVersion != 1 || got.Error != "unauthorized" || got.Message == "" {
				t.Errorf("%s %s with %s token: envelope %+v", rt.method, rt.path, name, got)
			}
		}
	}

	// A Host header other than the bound loopback host is rejected before
	// any handler, even with the correct token.
	resp, err = stateAPIRequest(t, "GET", base+"/api/v1/state", desc.BearerToken, "evil.example.com:80")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("foreign host: status %d, want 403", resp.StatusCode)
	}

	// The refresh POST answers an authenticated caller.
	resp, err = stateAPIRequest(t, "POST", base+"/api/v1/refresh", desc.BearerToken, "")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("authenticated refresh: status %d, want 202", resp.StatusCode)
	}

	// Graceful shutdown: the descriptor disappears before the lock is
	// released, and the listener closes.
	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("daemon run: %v", err)
		}
	case <-time.After(ShutdownBound + 5*time.Second):
		t.Fatal("daemon did not exit within the shutdown bound")
	}
	if _, err := os.Stat(descPath); !os.IsNotExist(err) {
		t.Errorf("descriptor survived shutdown: %v", err)
	}
	probe, probeErr := AcquireStateLock(lockPath)
	if probe != nil {
		_ = probe.Release()
	}
	if probeErr != nil {
		t.Fatalf("lock must be released after shutdown: %v", probeErr)
	}
	if _, statErr := os.Stat(descPath); statErr == nil {
		t.Error("descriptor reappeared after shutdown")
	}
	if _, err := stateAPIRequest(t, "GET", base+"/api/v1/state", desc.BearerToken, ""); err == nil {
		t.Error("listener still serving after shutdown")
	}

	// The bearer token never appears in the daemon's logs.
	if out := logs.String(); strings.Contains(out, desc.BearerToken) {
		t.Error("bearer token leaked into daemon logs")
	}
}

// TestStateAPIShutdownRemovesDescriptorBeforeLockRelease pins the
// ordering a client depends on: whoever can acquire the instance lock
// observes no live descriptor. A competing acquirer only ever wins the
// lock after Run released it, so the descriptor must be gone by then.
func TestStateAPIShutdownRemovesDescriptorBeforeLockRelease(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	cfg := stateAPIConfig(t.TempDir(), true)
	cancel, runDone := startTestDaemon(t, testDaemonOptions(stateDir, cfg, nil))
	descPath := StateAPIPath(stateDir, cfg)
	waitForDescriptor(t, descPath, 15*time.Second)

	lockPath := filepath.Join(stateDir, InstanceLockName(cfg))
	lockFree := make(chan struct{})
	go func() {
		defer close(lockFree)
		for {
			probe, err := AcquireStateLock(lockPath)
			if err == nil {
				_ = probe.Release()
				if _, statErr := os.Stat(descPath); statErr == nil {
					t.Errorf("descriptor still advertised after the lock was released")
				}
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()

	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("daemon run: %v", err)
		}
	case <-time.After(ShutdownBound + 5*time.Second):
		t.Fatal("daemon did not exit within the shutdown bound")
	}
	select {
	case <-lockFree:
	case <-time.After(5 * time.Second):
		t.Fatal("competing acquirer never won the released lock")
	}
}

// TestStateAPIBindFailureLogsAndContinues pins the viewer-surface
// doctrine: an occupied bind address is logged and the daemon carries on
// with its normal life, publishing no descriptor.
func TestStateAPIBindFailureLogsAndContinues(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	cfg := stateAPIConfig(t.TempDir(), true)
	occupied, err := net.Listen("tcp", "127.0.0.1:0") // a non-daemon squatter
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = occupied.Close() })
	cfg.StateAPI.Addr = occupied.Addr().String()

	logs := &syncBuffer{}
	cancel, runDone := startTestDaemon(t, testDaemonOptions(stateDir, cfg, logs))
	waitForLock(t, stateDir, cfg)
	time.Sleep(300 * time.Millisecond)

	glob, _ := filepath.Glob(filepath.Join(stateDir, "live", "stateapi-*.json"))
	if len(glob) != 0 {
		t.Errorf("failed bind published descriptors: %v", glob)
	}
	// The daemon continues: the lock is still held by this life.
	lockPath := filepath.Join(stateDir, InstanceLockName(cfg))
	probe, probeErr := AcquireStateLock(lockPath)
	if probe != nil {
		_ = probe.Release()
	}
	if probeErr == nil {
		t.Fatal("daemon gave up its lock after a bind failure")
	}
	if !strings.Contains(logs.String(), "stateapi bind failed") {
		t.Errorf("bind failure not logged; logs:\n%s", logs.String())
	}

	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("daemon run after bind failure: %v", err)
		}
	case <-time.After(ShutdownBound + 5*time.Second):
		t.Fatal("daemon did not exit within the shutdown bound")
	}
}

// TestStateAPIDescriptorsAreDistinctPerInstance pins enumeration over a
// shared state dir: two daemons with disjoint tracker paths publish two
// distinct descriptor files with distinct addrs and tokens — never a
// merged document.
func TestStateAPIDescriptorsAreDistinctPerInstance(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	cfgA := stateAPIConfig(t.TempDir(), true)
	cfgB := stateAPIConfig(t.TempDir(), true)
	if InstanceLockName(cfgA) == InstanceLockName(cfgB) {
		t.Fatal("disjoint tracker paths must derive distinct instances")
	}
	cancelA, doneA := startTestDaemon(t, testDaemonOptions(stateDir, cfgA, nil))
	cancelB, doneB := startTestDaemon(t, testDaemonOptions(stateDir, cfgB, nil))
	waitForLock(t, stateDir, cfgA)
	waitForLock(t, stateDir, cfgB)

	descA := waitForDescriptor(t, StateAPIPath(stateDir, cfgA), 15*time.Second)
	descB := waitForDescriptor(t, StateAPIPath(stateDir, cfgB), 15*time.Second)
	if descA.Instance == descB.Instance || descA.Addr == descB.Addr || descA.BearerToken == descB.BearerToken {
		t.Fatalf("instances share identity: %+v vs %+v", descA, descB)
	}

	glob, err := filepath.Glob(filepath.Join(stateDir, "live", "stateapi-*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(glob) != 2 {
		t.Errorf("descriptor files = %v, want exactly two", glob)
	}

	for _, desc := range []StateAPIDescriptor{descA, descB} {
		resp, err := stateAPIRequest(t, "GET", "http://"+desc.Addr+"/api/v1/state", desc.BearerToken, "")
		if err != nil {
			t.Fatalf("GET on %s: %v", desc.Addr, err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET on %s: status %d", desc.Addr, resp.StatusCode)
		}
		_ = resp.Body.Close()
	}

	cancelA()
	cancelB()
	for name, done := range map[string]<-chan error{"A": doneA, "B": doneB} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("daemon %s run: %v", name, err)
			}
		case <-time.After(ShutdownBound + 5*time.Second):
			t.Fatalf("daemon %s did not exit within the shutdown bound", name)
		}
	}
}

// TestAssembledDefaultWireServesStateAPI constructs the daemon exactly the
// way `lambe run` does — daemon.New with DefaultWire over a real
// disposable bd tracker — and proves an authenticated /api/v1/state
// answers and the descriptor exists. This is the dead-seam regression
// test for the wiring itself; the binary-level proof lives in cmd/lambe.
func TestAssembledDefaultWireServesStateAPI(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping bd-backed assembled test in -short mode")
	}
	if os.Getenv("LAMBE_TEST_BD") != "1" {
		t.Skip("set LAMBE_TEST_BD=1 for real tracker integration")
	}
	if _, err := exec.LookPath("bd"); err != nil {
		t.Skip("bd binary not found")
	}

	trackerDir := t.TempDir()
	init := exec.Command("bd", "init", "--prefix", "asm1", "--non-interactive")
	init.Dir = trackerDir
	if out, err := init.CombinedOutput(); err != nil {
		t.Fatalf("bd init: %v: %s", err, out)
	}
	workflowPath := filepath.Join(trackerDir, "WORKFLOW.md")
	if err := os.WriteFile(workflowPath, []byte("---\n---\nWork on {{ issue }} attempt {{ attempt }}.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	formulas := filepath.Join(trackerDir, ".beads", "formulas")
	if err := os.MkdirAll(formulas, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(formulas, "verify.formula.toml"), []byte("[gate]\ntype = \"shell\"\ncommand = \"exit 0\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "lambe.yaml")
	cfgYAML := fmt.Sprintf("harness: acp\nacp:\n  command: [\"true\"]\nworkflow_path: %q\ntrackers:\n  - name: local\n    type: repo\n    path: %q\nstateapi:\n  enable: true\n  addr: 127.0.0.1:0\n", workflowPath, trackerDir)
	if err := os.WriteFile(configPath, []byte(cfgYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.Options{File: configPath, Env: []string{}})
	if err != nil {
		t.Fatalf("load fixture config: %v", err)
	}

	stateDir := t.TempDir()
	opts := Options{
		Config:       cfg,
		ConfigPath:   configPath,
		StateDir:     stateDir,
		Machine:      "testbox",
		Actor:        "lambe@test",
		WorkflowPath: cfg.WorkflowPath,
		LockName:     InstanceLockName(cfg),
		Version:      "assembled-test",
		Log:          slogDiscard(),
		Wire:         DefaultWire(cfg, "", "lambe@test"),
	}
	d, err := New(opts)
	if err != nil {
		t.Fatalf("assemble daemon the way run does: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- d.Run(ctx) }()
	t.Cleanup(cancel)

	descPath := StateAPIPath(stateDir, cfg)
	desc := waitForDescriptor(t, descPath, 30*time.Second)

	base := "http://" + desc.Addr
	resp, err := stateAPIRequest(t, "GET", base+"/api/v1/state", desc.BearerToken, "")
	if err != nil {
		t.Fatalf("authenticated GET through the assembled wiring: %v", err)
	}
	var snap httpstate.SnapshotResponse
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	if resp.StatusCode != http.StatusOK || snap.SchemaVersion != 1 {
		t.Errorf("assembled GET = %d schema %d, want 200/1", resp.StatusCode, snap.SchemaVersion)
	}
	resp, err = stateAPIRequest(t, "GET", base+"/api/v1/state", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated GET through the assembled wiring: status %d, want 401", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("daemon run: %v", err)
		}
	case <-time.After(ShutdownBound + 5*time.Second):
		t.Fatal("daemon did not exit within the shutdown bound")
	}
	if _, err := os.Stat(descPath); !os.IsNotExist(err) {
		t.Errorf("descriptor survived shutdown: %v", err)
	}
}
