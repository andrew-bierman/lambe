package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/andrew-bierman/lambe/internal/config"
	"github.com/andrew-bierman/lambe/internal/daemon"
	"github.com/andrew-bierman/lambe/internal/proc"
	"github.com/andrew-bierman/lambe/internal/symphony"
)

// doctorFixture is a complete, healthy setup in temp dirs. Tests break one
// piece at a time.
type doctorFixture struct {
	env        doctorEnv
	configPath string
	tracker    string
	cfg        *config.Config
}

func newDoctorFixture(t *testing.T, extraConfig string) *doctorFixture {
	t.Helper()
	dir := t.TempDir()
	tracker := filepath.Join(dir, "proj")
	formulas := filepath.Join(tracker, ".beads", "formulas")
	if err := os.MkdirAll(formulas, 0o750); err != nil {
		t.Fatal(err)
	}
	gateTOML := "formula = \"g\"\nversion = 1\ntype = \"workflow\"\n[gate]\ntype = \"shell\"\ncommand = \"true\"\n"
	must(t, os.WriteFile(filepath.Join(formulas, "g.formula.toml"), []byte(gateTOML), 0o600))
	must(t, os.WriteFile(filepath.Join(tracker, "WORKFLOW.md"), []byte(workflowTemplate), 0o600))
	cfgPath := filepath.Join(dir, "config.yaml")
	yaml := "harness: acp\nacp:\n  command: [\"sh\"]\nworkflow_path: " + filepath.Join(tracker, "WORKFLOW.md") +
		"\ntrackers:\n  - name: local\n    type: repo\n    path: " + tracker + "\n" + extraConfig
	must(t, os.WriteFile(cfgPath, []byte(yaml), 0o600))
	cfg, err := config.Load(config.Options{File: cfgPath, Env: []string{}})
	must(t, err)

	fake := &fakeSys{reply: func(name string, _ []string) (string, error) {
		if name == "bd" || strings.HasSuffix(name, "/bd") {
			return "bd version 1.4.0 (abc)\n", nil
		}
		return "", errors.New("not loaded")
	}}
	return &doctorFixture{configPath: cfgPath, tracker: tracker, cfg: cfg, env: doctorEnv{
		sys:       fake.env("linux", filepath.Join(dir, "home")),
		stateDir:  filepath.Join(dir, "state"),
		now:       time.Now,
		lookPath:  func(string) (string, error) { return "/fake/bd", nil },
		getenv:    func(string) string { return "" },
		rexRunner: func(context.Context, string, ...string) (string, int, error) { return "", 4, errors.New("absent") },
	}}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func statuses(rs []checkResult) map[string]string {
	m := map[string]string{}
	for _, r := range rs {
		m[r.Name] = r.Status
	}
	return m
}

func TestDoctorMissingConfigFailsAndStops(t *testing.T) {
	t.Parallel()
	f := newDoctorFixture(t, "")
	var out bytes.Buffer
	err := runDoctor([]string{"-config", filepath.Join(t.TempDir(), "nope.yaml")}, f.env, &out)
	if !errors.Is(err, errDoctorFailed) {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(out.String(), "lambe init") {
		t.Errorf("no fix hint: %s", out.String())
	}
}

func TestDoctorHealthyStaticSetup(t *testing.T) {
	t.Parallel()
	f := newDoctorFixture(t, "")
	got := statuses(diagnose(context.Background(), f.env, f.configPath))
	for _, name := range []string{"config", "workflow", "tracker", "gates", "bd", "harness"} {
		if got[name] != statusOK {
			t.Errorf("%s = %q, want ok (all: %v)", name, got[name], got)
		}
	}
	if got["daemon"] != statusWarn {
		t.Errorf("never-run daemon = %q, want warn", got["daemon"])
	}
	if got["service"] != statusWarn {
		t.Errorf("uninstalled service = %q, want warn", got["service"])
	}
	if got["rex"] != statusSkip || got["stateapi"] != statusSkip {
		t.Errorf("optional checks should skip: %v", got)
	}
}

func TestDoctorFlagsBrokenPieces(t *testing.T) {
	t.Parallel()
	f := newDoctorFixture(t, "")
	must(t, os.WriteFile(f.cfg.WorkflowPath, []byte("---\nhooks: [unclosed\n---\nbody"), 0o600))
	must(t, os.Remove(filepath.Join(f.tracker, ".beads", "formulas", "g.formula.toml")))
	f.env.lookPath = func(string) (string, error) { return "", errors.New("not found") }
	got := statuses(diagnose(context.Background(), f.env, f.configPath))
	for _, name := range []string{"workflow", "gates", "bd"} {
		if got[name] != statusFail {
			t.Errorf("%s = %q, want fail (all: %v)", name, got[name], got)
		}
	}
}

// holdLock takes the daemon's exclusive state lock the way the daemon does.
func holdLock(t *testing.T, f *doctorFixture) {
	t.Helper()
	lock, err := daemon.AcquireStateLock(filepath.Join(f.env.stateDir, daemon.InstanceLockName(f.cfg)))
	must(t, err)
	t.Cleanup(func() { _ = lock.Release() })
}

func writeLive(t *testing.T, f *doctorFixture, pid int, identity string, runs []daemon.LiveRun) {
	t.Helper()
	path := daemon.LivePath(f.env.stateDir, f.cfg)
	must(t, os.MkdirAll(filepath.Dir(path), 0o700))
	data, err := json.Marshal(daemon.LiveSnapshot{PID: pid, Identity: identity, UpdatedAt: time.Now(), Runs: runs})
	must(t, err)
	must(t, os.WriteFile(path, data, 0o600))
}

func liveRun(id string, quietFor time.Duration) daemon.LiveRun {
	at := time.Now().Add(-quietFor)
	return daemon.LiveRun{Progress: symphony.Progress{IssueID: id}, UpdatedAt: at, LastOutputAt: at}
}

func TestDoctorLiveDaemonStallDetection(t *testing.T) {
	f := newDoctorFixture(t, "") // silence_timeout defaults to 10m
	identity, err := proc.ProcessIdentity(os.Getpid())
	must(t, err)
	holdLock(t, f)

	cases := []struct {
		name  string
		quiet time.Duration
		want  string
	}{
		{"fresh", 5 * time.Second, statusOK},
		{"half timeout", 6 * time.Minute, statusWarn},
		{"past timeout", 15 * time.Minute, statusFail},
	}
	for _, tc := range cases {
		writeLive(t, f, os.Getpid(), identity, []daemon.LiveRun{liveRun("x-1", tc.quiet)})
		got := statuses(diagnose(context.Background(), f.env, f.configPath))
		if got["daemon"] != statusOK {
			t.Fatalf("%s: daemon = %q", tc.name, got["daemon"])
		}
		if got["progress"] != tc.want {
			t.Errorf("%s: progress = %q, want %q", tc.name, got["progress"], tc.want)
		}
	}
}

func TestDoctorDeadDaemonWithUnfinishedRunsFails(t *testing.T) {
	t.Parallel()
	f := newDoctorFixture(t, "")
	writeLive(t, f, 999999, "gone", []daemon.LiveRun{liveRun("x-1", time.Minute)})
	if got := statuses(diagnose(context.Background(), f.env, f.configPath))["daemon"]; got != statusFail {
		t.Errorf("daemon = %q, want fail", got)
	}
}

func TestLockHeld(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "x.lock")
	if held, exists := lockHeld(path); held || exists {
		t.Errorf("missing file: held=%v exists=%v", held, exists)
	}
	f, err := os.Create(path) //nolint:gosec // G304: test temp path
	must(t, err)
	defer func() { _ = f.Close() }()
	if held, exists := lockHeld(path); held || !exists {
		t.Errorf("unlocked: held=%v exists=%v", held, exists)
	}
	must(t, syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB))
	if held, _ := lockHeld(path); !held {
		t.Error("lock not detected")
	}
}

func TestDoctorServiceChecks(t *testing.T) {
	t.Parallel()
	f := newDoctorFixture(t, "")
	unitPath := f.env.sys.unitPath("systemd", "")
	must(t, os.MkdirAll(filepath.Dir(unitPath), 0o750))
	must(t, os.WriteFile(unitPath, []byte("ExecStart=lambe run --config "+f.configPath), 0o600))

	state := "inactive"
	f.env.sys.Run = func(_ context.Context, name string, _ ...string) (string, error) {
		if name == "systemctl" {
			return state + "\n", errors.New("exit")
		}
		return "bd version 1.4.0\n", nil
	}
	if got := checkService(context.Background(), f.env, f.configPath); got.Status != statusFail {
		t.Errorf("inactive unit = %q, want fail", got.Status)
	}
	state = "active"
	if got := checkService(context.Background(), f.env, f.configPath); got.Status != statusOK {
		t.Errorf("active unit = %+v, want ok", got)
	}
	if got := checkService(context.Background(), f.env, "/some/other.yaml"); got.Status != statusWarn {
		t.Errorf("config mismatch = %q, want warn", got.Status)
	}
}

func TestDoctorStateAPI(t *testing.T) {
	t.Parallel()
	f := newDoctorFixture(t, "stateapi:\n  enable: true\n")
	if got := checkStateAPI(context.Background(), f.env, f.cfg); got.Status != statusWarn {
		t.Errorf("no descriptor = %q, want warn", got.Status)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()
	desc := daemon.StateAPIDescriptor{SchemaVersion: 1, Addr: strings.TrimPrefix(srv.URL, "http://"), BearerToken: "tok", PID: os.Getpid()}
	must(t, daemon.WriteStateAPIDescriptor(daemon.StateAPIPath(f.env.stateDir, f.cfg), desc))
	if got := checkStateAPI(context.Background(), f.env, f.cfg); got.Status != statusOK {
		t.Errorf("live API = %+v, want ok", got)
	}
	desc.BearerToken = "wrong"
	must(t, daemon.WriteStateAPIDescriptor(daemon.StateAPIPath(f.env.stateDir, f.cfg), desc))
	if got := checkStateAPI(context.Background(), f.env, f.cfg); got.Status != statusFail {
		t.Errorf("rejected token = %q, want fail", got.Status)
	}
	desc.Addr = "10.0.0.1:80"
	must(t, daemon.WriteStateAPIDescriptor(daemon.StateAPIPath(f.env.stateDir, f.cfg), desc))
	if got := checkStateAPI(context.Background(), f.env, f.cfg); got.Status != statusFail || !strings.Contains(got.Detail, "loopback") {
		t.Errorf("non-loopback = %+v", got)
	}
}

func TestDoctorJSONOutputAndRex(t *testing.T) {
	t.Parallel()
	f := newDoctorFixture(t, "")
	f.env.getenv = func(k string) string {
		if k == "REX_SESSION" {
			return "s1"
		}
		return ""
	}
	var out bytes.Buffer
	_ = runDoctor([]string{"-json", "-config", f.configPath}, f.env, &out)
	var rs []checkResult
	must(t, json.Unmarshal(out.Bytes(), &rs))
	if got := statuses(rs)["rex"]; got != statusOK {
		t.Errorf("rex = %q, want ok", got)
	}
}

func doctorResult(t *testing.T, rs []checkResult, name string) checkResult {
	t.Helper()
	for _, r := range rs {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("no %q check in %+v", name, rs)
	return checkResult{}
}

func TestDoctorIdentityPrintsActorAndWarnsOnGenericHostnames(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		host       string
		extra      string
		wantStatus string
		wantActor  string
	}{
		{"studio-east.local", "", statusOK, "lambe@studio-east"},
		{"localhost", "", statusWarn, "lambe@localhost"},
		{"MacBook-Pro.local", "", statusWarn, "lambe@MacBook-Pro"},
		{"macbook", "", statusWarn, "lambe@macbook"},
		{"localhost", "machine: east-1\n", statusOK, "lambe@east-1"},
		{"localhost", "actor: lambe@desk\n", statusOK, "lambe@desk"},
	} {
		f := newDoctorFixture(t, tc.extra)
		f.env.hostname = func() (string, error) { return tc.host, nil }
		r := doctorResult(t, diagnose(context.Background(), f.env, f.configPath), "identity")
		if r.Status != tc.wantStatus || !strings.Contains(r.Detail, "actor "+tc.wantActor) {
			t.Errorf("host %q extra %q: identity = %+v, want %s with actor %s", tc.host, tc.extra, r, tc.wantStatus, tc.wantActor)
		}
		if tc.wantStatus == statusWarn && !strings.Contains(r.Fix, "machine") {
			t.Errorf("host %q: warning needs a machine fix, got %q", tc.host, r.Fix)
		}
	}
}

func TestDoctorWarnsOnNodeIDAgainstSharedServer(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		mode   string
		nodeID string
		want   string
	}{
		{"embedded", "east-1", statusOK},
		{"server", "", statusOK},
		{"server", "east-1", statusWarn},
	} {
		f := newDoctorFixture(t, "")
		f.env.hostname = func() (string, error) { return "studio-east", nil }
		must(t, os.WriteFile(filepath.Join(f.tracker, ".beads", "metadata.json"), []byte(`{"dolt_mode":"`+tc.mode+`"}`), 0o600))
		f.env.getenv = func(k string) string {
			if k == "BEADS_NODE_ID" {
				return tc.nodeID
			}
			return ""
		}
		r := doctorResult(t, diagnose(context.Background(), f.env, f.configPath), "identity")
		if r.Status != tc.want {
			t.Errorf("mode %s node_id %q: identity = %+v, want %s", tc.mode, tc.nodeID, r, tc.want)
		}
		if tc.want == statusWarn && !strings.Contains(r.Detail, "node_id") {
			t.Errorf("warning must name node_id: %+v", r)
		}
	}
}

func TestDoctorFlagsStaleLoopTickOnLiveDaemon(t *testing.T) {
	f := newDoctorFixture(t, "") // tick 10s, silence 10m => cycle bound 5m
	identity, err := proc.ProcessIdentity(os.Getpid())
	must(t, err)
	holdLock(t, f)
	for _, tc := range []struct {
		name string
		age  time.Duration
		want string
	}{
		{"fresh", 5 * time.Second, statusOK},
		{"wedged", 30 * time.Minute, statusFail},
		{"legacy snapshot without tick_at", -1, statusSkip},
	} {
		snap := daemon.LiveSnapshot{PID: os.Getpid(), Identity: identity, UpdatedAt: time.Now()}
		if tc.age >= 0 {
			snap.TickAt = time.Now().Add(-tc.age)
		}
		path := daemon.LivePath(f.env.stateDir, f.cfg)
		must(t, os.MkdirAll(filepath.Dir(path), 0o700))
		data, err := json.Marshal(snap)
		must(t, err)
		must(t, os.WriteFile(path, data, 0o600))
		r := doctorResult(t, diagnose(context.Background(), f.env, f.configPath), "loop")
		if r.Status != tc.want {
			t.Errorf("%s: loop = %+v, want %s", tc.name, r, tc.want)
		}
	}
}

func TestDoctorProgressUsesConfiguredStallAfter(t *testing.T) {
	f := newDoctorFixture(t, "supervisor:\n  stall_after: 1m\n")
	identity, err := proc.ProcessIdentity(os.Getpid())
	must(t, err)
	holdLock(t, f)
	writeLive(t, f, os.Getpid(), identity, []daemon.LiveRun{liveRun("x-1", 2*time.Minute)})
	if got := statuses(diagnose(context.Background(), f.env, f.configPath))["progress"]; got != statusWarn {
		t.Errorf("progress at 2m quiet with stall_after 1m = %q, want warn", got)
	}
}
