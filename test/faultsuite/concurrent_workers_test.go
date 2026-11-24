package faultsuite_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/andrew-bierman/lambe/internal/gate"
	"github.com/andrew-bierman/lambe/internal/tracker"
)

func TestFault_ConcurrentWorkersWithLiveTrackerPolling(t *testing.T) {
	const actor = "lambe@faults-concurrent"
	env := newTracker(t, actor)
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	ids := []string{
		seedWork(t, env, "concurrent-first", "first concurrent task"),
		seedWork(t, env, "concurrent-second", "second concurrent task"),
	}
	for _, id := range ids {
		if err := env.PutAttempt(ctx, tracker.Attempt{SessionID: id, Number: 1}); err != nil {
			t.Fatal(err)
		}
	}
	quarantineRecords(ctx, t, env.Local, ids...)
	root := t.TempDir()
	formulas := filepath.Join(root, "formulas")
	if err := os.MkdirAll(formulas, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(formulas, "check.formula.toml"), []byte("[gate]\ntype = \"shell\"\ncommand = \"true\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	hub, err := gate.NewBdHub(env.bin, env.dir, actor)
	if err != nil {
		t.Fatal(err)
	}
	gates := gate.NewRunner(formulas, hub, gate.NewEscalator(hub), actor, quietLog())
	runner := newBlockingTurn()
	loop := newLoop(env, newLoader(t, root, "work {{ issue }}"), runner, gates, root).Loop
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCancel()
		if err := loop.Close(stopCtx); err != nil {
			t.Errorf("worker cleanup: %v", err)
		}
	})
	if err := loop.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "two real-tracker workers", 15*time.Second, func() (bool, error) { return runner.count() == 2, nil })
	for range 2 {
		if err := loop.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	close(runner.release)
	if err := loop.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		issues, err := env.Dispatchable(ctx, tracker.DispatchQuery{IDs: ids, Statuses: []tracker.Status{tracker.StatusClosed}})
		if err != nil {
			t.Fatal(err)
		}
		complete := len(issues) == 2
		for _, issue := range issues {
			complete = complete && issue.Status == tracker.StatusClosed && issue.Metadata[gate.MetaTombstone] == "1"
		}
		if complete {
			break
		}
		if err := loop.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		if err := loop.Wait(ctx); err != nil {
			t.Fatal(err)
		}
	}
	issues, err := env.Dispatchable(ctx, tracker.DispatchQuery{IDs: ids, Statuses: []tracker.Status{tracker.StatusClosed}})
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 2 {
		t.Fatalf("issues after concurrent completion = %+v", issues)
	}
	for _, issue := range issues {
		if issue.Status != tracker.StatusClosed || issue.Metadata[gate.MetaTombstone] != "1" {
			t.Fatalf("incomplete concurrent task: %+v", issue)
		}
		attempts, err := env.Attempts(ctx, issue.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(attempts) != 1 || attempts[0].Number != 1 {
			t.Fatalf("concurrent attempt records = %+v", attempts)
		}
	}
}
