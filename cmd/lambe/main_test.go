package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andrew-bierman/lambe/internal/config"
	"github.com/andrew-bierman/lambe/internal/daemon"
	"github.com/andrew-bierman/lambe/internal/symphony"
	"github.com/andrew-bierman/lambe/internal/tracker"
)

func TestExitCodeFor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"nil", nil, 1},
		{"plain error", errors.New("boom"), 1},
		{"lock held", fmt.Errorf("wrap: %w", daemon.ErrLockHeld), 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := exitCodeFor(tt.err); got != tt.want {
				t.Errorf("exitCodeFor(%v) = %d, want %d", tt.err, got, tt.want)
			}
		})
	}
}

func TestRunDaemonWithoutConfig(t *testing.T) {
	isolatedConfigDir(t)
	t.Chdir(t.TempDir())
	if err := os.Mkdir(".beads", 0o750); err != nil {
		t.Fatal(err)
	}
	err := runDaemon(nil)
	if !strings.Contains(err.Error(), "lambe init") {
		t.Fatalf("missing config should point to lambe init: %v", err)
	}
}

func TestRunDaemonMissingWorkflow(t *testing.T) {
	isolatedConfigDir(t)
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o750); err != nil {
		t.Fatal(err)
	}
	configFile := filepath.Join(t.TempDir(), "lambe.yaml")
	if err := os.WriteFile(configFile, []byte("trackers:\n  - name: local\n    type: repo\n    path: "+dir+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	err := runDaemon([]string{"-config", configFile})
	if !errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), "create the agent workflow first") {
		t.Fatalf("missing workflow should explain setup: %v", err)
	}
}

func TestCommandsMissingTracker(t *testing.T) {
	for _, tt := range []struct {
		name string
		run  func([]string) error
		want string
	}{{"run", runDaemon, "lambe init"}, {"status", runStatus, "bd init"}, {"mcp", runMCP, "bd init"}} {
		t.Run(tt.name, func(t *testing.T) {
			isolatedConfigDir(t)
			t.Chdir(t.TempDir())
			err := tt.run(nil)
			if !errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("missing tracker should mention %q: %v", tt.want, err)
			}
			if _, err := os.Stat(".beads"); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("command must not initialize a stray tracker: %v", err)
			}
		})
	}
}

func TestInitNonInteractive(t *testing.T) {
	home := isolatedConfigDir(t)
	project := t.TempDir()
	if err := os.MkdirAll(filepath.Join(project, ".beads"), 0o750); err != nil {
		t.Fatal(err)
	}
	t.Chdir(project)

	var out bytes.Buffer
	if err := runInit([]string{"-non-interactive"}, strings.NewReader(""), &out); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	configPath := filepath.Join(home, "lambe", "config.yaml")
	if _, err := os.Stat(configPath); err != nil {
		t.Fatalf("config not written: %v", err)
	}
	workflow := filepath.Join(project, "WORKFLOW.md")
	if _, err := os.Stat(workflow); err != nil {
		t.Fatalf("WORKFLOW.md not written: %v", err)
	}
	formula := filepath.Join(project, ".beads", "formulas", "keep-tests-green.formula.toml")
	formulaBody, err := os.ReadFile(formula) //nolint:gosec // G304: path built from t.TempDir in this test.
	if err != nil {
		t.Fatalf("starter formula not written: %v", err)
	}
	for _, want := range []string{"[formula]", "[[steps]]", "[trigger]", "[gate]"} {
		if !strings.Contains(string(formulaBody), want) {
			t.Errorf("formula template should contain %q", want)
		}
	}
	if !strings.Contains(out.String(), "lambe run") || !strings.Contains(out.String(), "lambe status") {
		t.Errorf("init should print next steps, got:\n%s", out.String())
	}

	// The written config must load and resolve the workflow to the tracker.
	cfg, err := config.Load(config.Options{File: configPath})
	if err != nil {
		t.Fatalf("Load written config: %v", err)
	}
	if len(cfg.Trackers) != 1 || cfg.Trackers[0].Path != project {
		t.Errorf("trackers: got %+v, want path %s", cfg.Trackers, project)
	}
	if cfg.WorkflowPath != workflow {
		t.Errorf("workflow path: got %q, want %q", cfg.WorkflowPath, workflow)
	}

	// A second run must not clobber the existing config.
	if err := runInit([]string{"-non-interactive"}, strings.NewReader(""), &out); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("re-init should refuse to overwrite: %v", err)
	}
}

func TestInitDetectsParentTracker(t *testing.T) {
	home := isolatedConfigDir(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".beads"), 0o750); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "sub", "deep")
	if err := os.MkdirAll(sub, 0o750); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sub)

	var out bytes.Buffer
	if err := runInit([]string{"-non-interactive"}, strings.NewReader(""), &out); err != nil {
		t.Fatalf("runInit: %v", err)
	}
	cfg, err := config.Load(config.Options{File: filepath.Join(home, "lambe", "config.yaml")})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Trackers) != 1 || cfg.Trackers[0].Path != root {
		t.Errorf("tracker path: got %+v, want %s", cfg.Trackers, root)
	}
	if _, err := os.Stat(filepath.Join(root, "WORKFLOW.md")); err != nil {
		t.Errorf("WORKFLOW.md should land next to the detected tracker: %v", err)
	}
}

func TestInitNonInteractiveWithoutTracker(t *testing.T) {
	home := isolatedConfigDir(t)
	project := t.TempDir()
	t.Chdir(project)

	var out bytes.Buffer
	if err := runInit([]string{"-non-interactive"}, strings.NewReader(""), &out); err != nil {
		t.Fatalf("runInit: %v", err)
	}
	cfg, err := config.Load(config.Options{File: filepath.Join(home, "lambe", "config.yaml")})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Trackers) != 1 || cfg.Trackers[0].Path != project {
		t.Errorf("tracker path should default to cwd: %+v", cfg.Trackers)
	}
	if !strings.Contains(out.String(), "bd init") {
		t.Errorf("output should tell the user to run bd init:\n%s", out.String())
	}
}

func TestDaemonOptionsGateClosesOwnClaim(t *testing.T) {
	if os.Getenv("LAMBE_TEST_BD") != "1" {
		t.Skip("set LAMBE_TEST_BD=1 for real tracker integration")
	}
	isolatedConfigDir(t)
	t.Setenv("LAMBE_STATE_DIR", t.TempDir())

	dir := t.TempDir()
	cmd := exec.Command("bd", "init", "--prefix", "smoke", "--non-interactive")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bd init: %v: %s", err, out)
	}
	formulas := filepath.Join(dir, ".beads", "formulas")
	if err := os.MkdirAll(formulas, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(formulas, "smoke.formula.toml"), []byte("[gate]\ntype = \"shell\"\ncommand = \"exit 0\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.Options{Env: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Trackers[0].Path = dir
	opts := daemonOptions(cfg, "")
	local, err := tracker.NewLocal("", dir, opts.Actor)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	issue, err := local.EnsureIssue(ctx, "smoke-1", tracker.IssueSpec{Title: "smoke test"})
	if err != nil {
		t.Fatal(err)
	}
	issue, err = local.Claim(ctx, issue.ID)
	if err != nil {
		t.Fatal(err)
	}
	var asm daemon.Assembly
	if err := opts.Wire(&asm); err != nil {
		t.Fatal(err)
	}
	outcome, err := asm.Gates.Evaluate(ctx, symphony.GateRequest{Issue: issue, Attempt: 1, Workspace: dir})
	if err != nil || outcome.Verdict != symphony.GateGreen {
		t.Fatalf("completion: %+v, error: %v", outcome, err)
	}
	closed, err := local.Dispatchable(ctx, tracker.DispatchQuery{IDs: []string{issue.ID}, Statuses: []string{tracker.StatusClosed}})
	if err != nil || len(closed) != 1 || closed[0].Status != tracker.StatusClosed {
		t.Fatalf("completed issue: %+v, error: %v", closed, err)
	}
	if err := local.Release(ctx, issue.ID); !errors.Is(err, tracker.ErrNotOwner) {
		t.Fatalf("closed issue has no claim to release: %v", err)
	}
}

// isolatedConfigDir keeps platform-specific defaults inside the test's temporary home.
func isolatedConfigDir(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("APPDATA", home)
	dir, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(home, dir)
	if err != nil || !filepath.IsLocal(rel) {
		t.Fatalf("user config directory %q escapes isolated home %q", dir, home)
	}
	return dir
}
