package acp

import (
	"io"
	"os"
	"testing"
	"time"
)

// A child with no descendants leaves the pipe drained as soon as it exits;
// waitOwner must not sleep the whole WaitDelay before closing the read end.
// With the unconditional sleep this test fails after its two-second budget.
func TestWaitOwnerClosesThePipeWhenItIsAlreadyDrained(t *testing.T) {
	p, err := spawnAgent(clientOptions{Argv: []string{"/bin/sh", "-c", "printf hi"}, Env: os.Environ()})
	if err != nil {
		t.Fatal(err)
	}
	read := make(chan struct{})
	go func() {
		defer close(read)
		_, _ = io.Copy(io.Discard, p.reader)
	}()

	<-p.WaitDone()
	<-read
	select {
	case <-p.PipeClosed():
	case <-time.After(2 * time.Second):
		t.Fatal("the read end stayed open for the full WaitDelay after the pipe drained")
	}
}
