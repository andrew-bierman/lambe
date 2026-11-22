package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/andrew-bierman/lambe/internal/config"
	"github.com/andrew-bierman/lambe/internal/daemon"
	"github.com/andrew-bierman/lambe/internal/symphony"
	"github.com/andrew-bierman/lambe/internal/tracker"
)

func TestClaimsUsageErrors(t *testing.T) {
	for _, args := range [][]string{nil, {"nope"}, {"list", "x"}, {"list", "--force"}, {"release"}, {"release", "a", "b"}} {
		err := runClaims(args, &bytes.Buffer{})
		if got := readExitCode(err); got != 2 {
			t.Errorf("runClaims(%v) exit = %d (%v), want 2", args, got, err)
		}
	}
}

// TestClaimsAgainstRealBeads lists and releases an orphaned claim in a
// throwaway bd database under an isolated HOME and state dir.
func TestClaimsAgainstRealBeads(t *testing.T) {
	if os.Getenv("LAMBE_TEST_BD") != "1" {
		t.Skip("set LAMBE_TEST_BD=1 for real tracker integration")
	}
	isolatedConfigDir(t)
	state := t.TempDir()
	t.Setenv("LAMBE_STATE_DIR", state)

	dir := t.TempDir()
	init := exec.Command("bd", "init", "--prefix", "orph", "--non-interactive")
	init.Dir = dir
	if out, err := init.CombinedOutput(); err != nil {
		t.Fatalf("bd init: %v: %s", err, out)
	}
	cfgFile := filepath.Join(t.TempDir(), "lambe.yaml")
	if err := os.WriteFile(cfgFile, []byte("trackers:\n  - name: local\n    type: repo\n    path: "+dir+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.Options{File: cfgFile, Env: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	_, actor := hostIdentity(cfg)
	local, err := tracker.NewLocal("", dir, actor)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	var ids []string
	for i := range 2 {
		issue, err := local.CreateTask(ctx, fmt.Sprintf("stale %d", i), nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := local.Claim(ctx, issue.ID); err != nil {
			t.Fatalf("claim: %v", err)
		}
		ids = append(ids, issue.ID)
		body := fmt.Sprintf(`{"version":1,"phase":"legacy-blocked","issue":{"ID":%q,"Assignee":%q},"workflow":{"MaxTurns":1},"key":%q}`, issue.ID, actor, issue.ID)
		ckpt := daemon.CheckpointDir(state, cfg)
		if err := os.MkdirAll(ckpt, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(ckpt, symphony.SanitizeKey(issue.ID)+".json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	var list bytes.Buffer
	if err := runClaims([]string{"list", "-config", cfgFile}, &list); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if !strings.Contains(list.String(), id) || !strings.Contains(list.String(), "absent") {
			t.Fatalf("list missing %s:\n%s", id, list.String())
		}
	}
	var status bytes.Buffer
	if err := runStatus([]string{"-config", cfgFile}, &status); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(status.String(), "orphaned claims") || !strings.Contains(status.String(), ids[0]) {
		t.Fatalf("status lacks the orphaned claims section:\n%s", status.String())
	}

	if err := runClaims([]string{"release", "-config", cfgFile, ids[0]}, &bytes.Buffer{}); err != nil {
		t.Fatalf("release: %v", err)
	}
	got, err := local.Dispatchable(ctx, tracker.DispatchQuery{IDs: []string{ids[0]}})
	if err != nil || len(got) != 1 || got[0].Assignee != "" || got[0].Status == tracker.StatusInProgress {
		t.Fatalf("released issue still claimed: %+v, err %v", got, err)
	}
	other, err := local.Dispatchable(ctx, tracker.DispatchQuery{IDs: []string{ids[1]}})
	if err != nil || len(other) != 1 || other[0].Assignee != actor {
		t.Fatalf("release touched another claim: %+v, err %v", other, err)
	}
	list.Reset()
	if err := runClaims([]string{"list", "-config", cfgFile}, &list); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(list.String(), ids[0]) || !strings.Contains(list.String(), ids[1]) {
		t.Fatalf("list after release:\n%s", list.String())
	}
}

func TestDoctorWarnsAboutOrphanedClaims(t *testing.T) {
	state := t.TempDir()
	cfg := &config.Config{Trackers: []config.Tracker{{Name: "t", Path: t.TempDir()}}}
	e := doctorEnv{stateDir: state, now: func() time.Time { return time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC) }}
	if got := checkOrphanClaims(e, cfg); len(got) != 0 {
		t.Fatalf("no orphans should add no check: %+v", got)
	}
	ckpt := daemon.CheckpointDir(state, cfg)
	if err := os.MkdirAll(ckpt, 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"version":1,"phase":"legacy-blocked","issue":{"ID":"old-1"},"workflow":{"MaxTurns":1},"key":"old-1","blocked_since":"2026-11-20T00:00:00Z"}`
	if err := os.WriteFile(filepath.Join(ckpt, "old-1.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got := checkOrphanClaims(e, cfg)
	if len(got) != 1 || got[0].Status != statusWarn || !strings.Contains(got[0].Detail, "1 orphaned claim") || !strings.Contains(got[0].Detail, "48h0m0s") {
		t.Fatalf("check = %+v", got)
	}
}
