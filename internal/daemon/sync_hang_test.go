package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestBdSyncerReturnsWhenADescendantHoldsThePipe reproduces a silent wedge: a
// hub pull whose bd spawns a descendant that inherits stdout (as a dolt
// server does) must still return once the call's context expires. Without
// WaitDelay, Wait blocks on the pipe and the whole tick cycle (cycleMu) hangs.
func TestBdSyncerReturnsWhenADescendantHoldsThePipe(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bd")
	script := "#!/bin/sh\nsleep 30 &\nexec sleep 30\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil { //nolint:gosec // G306: test fixture must be executable.
		t.Fatal(err)
	}
	cctx, stop := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer stop()

	done := make(chan error, 1)
	go func() { done <- (&BdSyncer{Bin: bin, Dir: dir}).Pull(cctx) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("pull of a killed command reported success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("BdSyncer.Pull still blocked 5s after its 300ms context expired (descendant holds stdout)")
	}
}
