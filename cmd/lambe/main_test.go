package main

import (
	"bytes"
	"context"
	"encoding/json"
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
	for _, want := range []string{"formula = \"keep-tests-green\"", "[[steps]]", "[trigger]"} {
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

	// Starting from another project must not mix this tracker with its workflow.
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "WORKFLOW.md"), []byte("another project"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(other)
	cfg, err = config.Load(config.Options{File: configPath})
	if err != nil || cfg.WorkflowPath != workflow {
		t.Fatalf("configuration picked another project workflow: %+v %v", cfg, err)
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

func TestInitChoosesCodex(t *testing.T) {
	home := isolatedConfigDir(t)
	project := t.TempDir()
	t.Chdir(project)
	if err := os.Mkdir(".beads", 0o750); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runInit([]string{"-non-interactive", "-harness", "codex"}, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.Options{File: filepath.Join(home, "lambe", "config.yaml"), Env: []string{}})
	if err != nil || cfg.Harness != "codex" {
		t.Fatalf("Codex preference lost during setup: %+v, %v", cfg, err)
	}
}

func TestStarterFormulaCooksWithInstalledBeads(t *testing.T) {
	if os.Getenv("LAMBE_TEST_BD") != "1" {
		t.Skip("set LAMBE_TEST_BD=1 for installed Beads")
	}
	isolatedConfigDir(t)
	dir := t.TempDir()
	init := exec.Command("bd", "init", "--prefix", "starter", "--non-interactive")
	init.Dir = dir
	if out, err := init.CombinedOutput(); err != nil {
		t.Fatalf("bd init: %v: %s", err, out)
	}
	path := filepath.Join(dir, "keep-tests-green.formula.toml")
	if err := os.WriteFile(path, []byte(formulaTemplate), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bd", "cook", path, "--json") // #nosec G204 -- fixed executable and private test-owned formula
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("starter cannot be cooked: %v: %s", err, out)
	}
	var cooked struct {
		Steps []struct {
			ID string `json:"id"`
		} `json:"steps"`
	}
	if err := json.Unmarshal(out, &cooked); err != nil {
		t.Fatal(err)
	}
	if len(cooked.Steps) != 2 || cooked.Steps[0].ID != "diagnose" || cooked.Steps[1].ID != "fix" {
		t.Fatalf("starter lost its native steps: %s", out)
	}
	formulas, err := symphony.LoadFormulas(dir)
	if err != nil || len(formulas) != 1 {
		t.Fatalf("Lambe rejected native starter: %+v %v", formulas, err)
	}
	fired, err := symphony.TriggerFired(t.Context(), formulas[0])
	if err != nil || fired {
		t.Fatalf("starter created work before setup: fired=%t error=%v", fired, err)
	}
}

func TestStarterNeverBlocksAnIndependentCompletionGate(t *testing.T) {
	dir := t.TempDir()
	formulas := filepath.Join(dir, ".beads", "formulas")
	if err := os.MkdirAll(formulas, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(formulas, "keep-tests-green.formula.toml"), []byte(formulaTemplate), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.Options{Env: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Trackers[0].Path = dir
	if err := requireGates(cfg); err == nil || !strings.Contains(err.Error(), "before starting") {
		t.Fatalf("unsafe setup accepted: %v", err)
	}
	if err := os.WriteFile(filepath.Join(formulas, "verify.formula.toml"), []byte("[gate]\ntype = \"shell\"\ncommand = \"go test ./...\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := requireGates(cfg); err != nil {
		t.Fatalf("starter blocked a real completion gate: %v", err)
	}
	loaded, err := symphony.LoadFormulas(formulas)
	if err != nil || len(loaded) != 1 || loaded[0].Name != "keep-tests-green" {
		t.Fatalf("gate-only file disabled responsibilities: %+v %v", loaded, err)
	}
}

func TestGatePreflightRejectsEmptyTrackerConfiguration(t *testing.T) {
	if err := requireGates(&config.Config{}); err == nil {
		t.Fatal("missing tracker accepted")
	}
}

func TestBareCommandsLoadInitializedConfigurationFromAnotherDirectory(t *testing.T) {
	home := isolatedConfigDir(t)
	project := t.TempDir()
	if err := os.Mkdir(filepath.Join(project, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(project)
	var out bytes.Buffer
	if err := runInit([]string{"-non-interactive", "-harness", "codex"}, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	cfg, path, err := parseCommon(nil)
	if err != nil || path != filepath.Join(home, "lambe", "config.yaml") || cfg.Harness != "codex" || cfg.Trackers[0].Path != project || cfg.WorkflowPath != filepath.Join(project, "WORKFLOW.md") {
		t.Fatalf("bare command ignored initialized setup: path=%q cfg=%+v err=%v", path, cfg, err)
	}
}

// TestInitHelpIsSuccess pins the CLI contract that -h/--help exits cleanly:
// flag.ErrHelp is a successful outcome of the setup wizard, not a failure.
// (The flag package prints usage to its own stderr; runInit's writer only
// carries wizard output.)
func TestInitHelpIsSuccess(t *testing.T) {
	var out strings.Builder
	if err := runInit([]string{"-h"}, strings.NewReader(""), &out); err != nil {
		t.Fatalf("runInit(-h) = %v; want nil", err)
	}
}

// Hosts sharing one tracker need distinct actors; the daemon must take its
// identity from config (machine/actor, env included), not from the hostname
// alone.
func TestDaemonOptionsUseConfiguredIdentity(t *testing.T) {
	cfg, err := config.Load(config.Options{Env: []string{"LAMBE_MACHINE=studio", "LAMBE_ACTOR=lambe@studio-2"}})
	if err != nil {
		t.Fatal(err)
	}
	opts := daemonOptions(cfg, "")
	if opts.Machine != "studio" || opts.Actor != "lambe@studio-2" {
		t.Fatalf("identity = %q / %q, want studio / lambe@studio-2", opts.Machine, opts.Actor)
	}
	cfg, err = config.Load(config.Options{Env: []string{"LAMBE_MACHINE=studio"}})
	if err != nil {
		t.Fatal(err)
	}
	if opts := daemonOptions(cfg, ""); opts.Actor != "lambe@studio" {
		t.Fatalf("derived actor = %q, want lambe@studio", opts.Actor)
	}
}
