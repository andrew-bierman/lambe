package gate

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestBdHubReturnsWhenADescendantHoldsThePipe: a bd write whose descendant
// inherits stdout must return when its context expires, not block the gate
// settle (and the tick loop behind it) on the pipe.
func TestBdHubReturnsWhenADescendantHoldsThePipe(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bd")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nsleep 30 &\nexec sleep 30\n"), 0o700); err != nil { //nolint:gosec // G306: test fixture must be executable.
		t.Fatal(err)
	}
	hub, err := NewBdHub(bin, dir, "lambe@test")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- hub.Comment(ctx, "x-1", "hi") }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error from a killed command")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("BdHub.Comment blocked 5s past its context (descendant holds stdout)")
	}
}
