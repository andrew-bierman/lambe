package tracker

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// TestShellTransportHungCommandTimesOut proves the per-call bound: a bd
// subprocess that never exits fails with a deadline-exceeded error within
// the configured CommandTimeout instead of wedging the caller forever.
func TestShellTransportHungCommandTimesOut(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("spawns a real sleeping subprocess")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "hung-bd")
	writeExecutable(t, script, "#!/bin/sh\nsleep 30\n")
	tr := NewShellTransport(script, dir, "lambe@test")
	tr.CommandTimeout = 250 * time.Millisecond

	done := make(chan error, 1)
	start := time.Now()
	go func() {
		_, err := tr.Version(context.Background())
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("hung bd invocation must fail")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("error = %v, want context.DeadlineExceeded", err)
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("call took %s, want it cut at the 250ms bound", elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("hung bd invocation wedged the caller past the bound")
	}
}

// TestShellTransportDefaultCommandTimeout pins the default: an unset or
// non-positive CommandTimeout resolves to DefaultCommandTimeout, and an
// explicit value is preserved.
func TestShellTransportDefaultCommandTimeout(t *testing.T) {
	t.Parallel()
	tr := NewShellTransport("bd", ".", "a")
	if got := tr.commandTimeout(); got != DefaultCommandTimeout {
		t.Errorf("default timeout = %v, want %v", got, DefaultCommandTimeout)
	}
	tr.CommandTimeout = -time.Second
	if got := tr.commandTimeout(); got != DefaultCommandTimeout {
		t.Errorf("negative timeout = %v, want default %v", got, DefaultCommandTimeout)
	}
	tr.CommandTimeout = 5 * time.Second
	if got := tr.commandTimeout(); got != 5*time.Second {
		t.Errorf("explicit timeout = %v, want 5s", got)
	}
}
