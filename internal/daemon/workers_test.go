package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/andrew-bierman/lambe/internal/config"
	"github.com/andrew-bierman/lambe/internal/symphony"
	"github.com/andrew-bierman/lambe/internal/tracker"
)

type daemonTurnFunc func(context.Context, symphony.TurnRequest) (symphony.TurnResult, error)

func (f daemonTurnFunc) RunTurn(ctx context.Context, req symphony.TurnRequest) (symphony.TurnResult, error) {
	return f(ctx, req)
}

func TestDaemonRefreshesWhileWorkersRunAndJoinsThemAtShutdown(t *testing.T) {
	dir := t.TempDir()
	workflow := filepath.Join(dir, "WORKFLOW.md")
	if err := os.WriteFile(workflow, []byte("---\nagent:\n  max_concurrent_agents: 2\n---\nWork {{ issue }}"), 0o600); err != nil {
		t.Fatal(err)
	}
	tr := newFakeTracker()
	for _, id := range []string{"first", "second"} {
		tr.issues[id] = tracker.Issue{ID: id, Title: id, Status: "ready"}
	}
	entered, stopped := make(chan string, 2), make(chan string, 2)
	reports := &ReportCache{}
	d, err := New(Options{
		Config:   &config.Config{Supervisor: config.Supervisor{TickSeconds: 1, HeartbeatInterval: time.Hour}},
		StateDir: filepath.Join(dir, "state"), Machine: "test", WorkflowPath: workflow,
		Wire: func(a *Assembly) error {
			a.Trackers = []tracker.Tracker{tr}
			a.Reports = reports
			a.Runner = daemonTurnFunc(func(ctx context.Context, req symphony.TurnRequest) (symphony.TurnResult, error) {
				entered <- req.Issue.ID
				<-ctx.Done()
				stopped <- req.Issue.ID
				return symphony.TurnResult{Exit: symphony.TurnCanceled}, ctx.Err()
			})
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("daemon shutdown: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("daemon failed to stop")
		}
	})
	for range 2 {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("assembled daemon did not start two workers")
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		rep, at := reports.Load()
		if !at.IsZero() && len(rep.Active) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("active workers blocked report refresh")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	for range 2 {
		select {
		case <-stopped:
		case <-time.After(3 * time.Second):
			t.Fatal("worker outlived daemon cancellation")
		}
	}
}

type closingTicker struct{ closed chan struct{} }

func (t closingTicker) Tick(context.Context) error  { return nil }
func (t closingTicker) Close(context.Context) error { close(t.closed); return nil }

func TestShutdownJoinsWorkerCoordinatorAfterInFlightCycle(t *testing.T) {
	d, err := New(Options{Config: &config.Config{}, StateDir: t.TempDir(), Machine: "test"})
	if err != nil {
		t.Fatal(err)
	}
	closed := make(chan struct{})
	d.loop = closingTicker{closed: closed}
	d.cycleMu.Lock()
	locked := true
	defer func() {
		if locked {
			d.cycleMu.Unlock()
		}
	}()
	ticksDone := make(chan struct{})
	close(ticksDone)
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- d.shutdown(ticksDone) }()
	select {
	case err := <-shutdownDone:
		t.Fatalf("shutdown skipped the in-flight coordinator: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	d.cycleMu.Unlock()
	locked = false
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not join workers after cycle exited")
	}
	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown did not return")
	}
}
