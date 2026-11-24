package acp

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/andrew-bierman/lambe/internal/proc"
)

// TestStartClientLoggerSetBeforeFirstRead pins that an agent talking the
// instant it spawns cannot race the SDK read loop against SetLogger: the
// connection's reader must not see a byte until the logger is installed.
// Run under -race; each iteration is one chance for the race to surface.
func TestStartClientLoggerSetBeforeFirstRead(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for range 40 {
		c, err := startClient(clientOptions{
			Argv:       []string{"sh", "-c", "echo not-json; echo '[]'; sleep 5"},
			Logger:     logger,
			OnActivity: func() {},
		}, &recordingSink{})
		if err != nil {
			t.Fatal(err)
		}
		// Let the read loop consume the garbage and hit the logger.
		time.Sleep(time.Millisecond)
		_ = c.Close(t.Context(), 10*time.Millisecond)
	}
}

// TestStdoutClosedPromptlyAfterCleanExit pins that a cleanly exiting agent does
// not make teardown sit out proc.DefaultWaitDelay: once the read loop has seen
// EOF, waitOwner closes the stdout read end at once. The bound is generous
// (two thirds of the delay) because the machine may be loaded.
func TestStdoutClosedPromptlyAfterCleanExit(t *testing.T) {
	c, err := startClient(clientOptions{Argv: []string{"sh", "-c", "exit 0"}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}, &recordingSink{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close(t.Context(), 10*time.Millisecond) }()
	deadline := time.Now().Add(proc.DefaultWaitDelay * 2 / 3)
	for {
		if _, err := c.proc.stdout.Stat(); errors.Is(err, os.ErrClosed) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("stdout still open %v after the agent exited cleanly", proc.DefaultWaitDelay*2/3)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
