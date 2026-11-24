package acp

import (
	"io"
	"log/slog"
	"testing"
	"time"
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
