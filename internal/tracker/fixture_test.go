package tracker

import (
	"os"
	"syscall"
	"testing"
)

// writeExecutable writes a script fixture and makes it executable. It holds
// syscall.ForkLock for reading so no parallel test can fork while the write
// descriptor is open; a child that inherited it would make exec of the fresh
// file fail with ETXTBSY ("text file busy") on Linux.
func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	if err := os.WriteFile(path, []byte(content), 0o700); err != nil { // #nosec G306 -- private executable test fixture.
		t.Fatal(err)
	}
}
