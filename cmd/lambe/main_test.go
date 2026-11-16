package main

import (
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

func TestRunDaemonMissingWorkflow(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.Mkdir(".beads", 0o750); err != nil {
		t.Fatal(err)
	}
	err := runDaemon(nil)
	if !errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), "create the agent workflow first") {
		t.Fatalf("missing workflow should explain setup: %v", err)
	}
}

func TestCommandsMissingTracker(t *testing.T) {
	for _, tt := range []struct {
		name string
		run  func([]string) error
	}{{"run", runDaemon}, {"status", runStatus}, {"mcp", runMCP}} {
		t.Run(tt.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			err := tt.run(nil)
			if !errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), "bd init") {
				t.Fatalf("missing tracker should explain setup: %v", err)
			}
			if _, err := os.Stat(".beads"); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("command must not initialize a stray tracker: %v", err)
			}
		})
	}
}

func TestDaemonOptionsGateClosesOwnClaim(t *testing.T) {
	if os.Getenv("LAMBE_TEST_BD") != "1" {
		t.Skip("set LAMBE_TEST_BD=1 for real tracker integration")
	}
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
}
