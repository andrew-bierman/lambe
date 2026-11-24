// SPDX-License-Identifier: GPL-3.0-or-later

package proc_test

import (
	"os"
	"runtime"
	"testing"

	"github.com/andrew-bierman/lambe/internal/proc"
)

func TestProcessIdentityIsStableForLiveProcess(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("unsupported platform")
	}
	first, err := proc.ProcessIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	again, err := proc.ProcessIdentity(os.Getpid())
	if err != nil || first == "" || again != first {
		t.Fatalf("process identity changed: %q, %q, %v", first, again, err)
	}
	for _, pid := range []int{-1, 0, 1, 1 << 28} {
		if _, err := proc.ProcessIdentity(pid); err == nil {
			t.Errorf("invalid/missing process %d had an identity", pid)
		}
	}
}
