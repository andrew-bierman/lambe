package proc_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/andrew-bierman/lambe/internal/proc"
)

func TestRunCommandCleansDescendants(t *testing.T) {
	for _, stop := range []string{"cancel", "exit"} {
		t.Run(stop, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
			defer cancel()
			script := "(sleep 2; echo leaked > leaked) & echo ready > ready; echo passed; "
			if stop == "cancel" {
				script += "wait"
			} else {
				script += "exit 0"
			}
			type result struct {
				out string
				err error
			}
			done := make(chan result, 1)
			go func() { out, err := proc.RunCommand(ctx, dir, "sh", "-c", script); done <- result{out, err} }()
			deadline := time.Now().Add(3 * time.Second)
			for {
				if _, err := os.Stat(filepath.Join(dir, "ready")); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("command did not start")
				}
				time.Sleep(2 * time.Millisecond)
			}
			if stop == "cancel" {
				cancel()
			}
			var res result
			select {
			case res = <-done:
			case <-time.After(4 * time.Second):
				t.Fatal("command did not return")
			}
			if stop == "cancel" {
				if !errors.Is(res.err, context.Canceled) {
					t.Fatalf("cancel error = %v", res.err)
				}
			} else if res.err != nil || !strings.Contains(res.out, "passed") {
				t.Fatalf("clean exit = %q, %v", res.out, res.err)
			}
			time.Sleep(2200 * time.Millisecond)
			if _, err := os.Stat(filepath.Join(dir, "leaked")); !os.IsNotExist(err) {
				t.Fatal("descendant survived completed command")
			}
		})
	}
}

func TestRunCommandIdentifiesACommandThatNeverStarted(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := proc.RunCommand(ctx, t.TempDir(), "sh", "-c", "exit 0"); !errors.Is(err, proc.ErrCommandNotStarted) || !errors.Is(err, context.Canceled) {
		t.Fatalf("unstarted cancellation lost identity: %v", err)
	}
	if _, err := proc.RunCommand(t.Context(), t.TempDir(), filepath.Join(t.TempDir(), "missing")); !errors.Is(err, proc.ErrCommandNotStarted) {
		t.Fatalf("missing binary classified as started: %v", err)
	}
	if _, err := proc.RunCommand(t.Context(), t.TempDir(), "sh", "-c", "exit 1"); err == nil || errors.Is(err, proc.ErrCommandNotStarted) {
		t.Fatalf("executed failure classified as unstarted: %v", err)
	}
}
