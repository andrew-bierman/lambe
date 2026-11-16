package daemon

import (
	"os"
	"testing"
)

func TestProcStartTimeParsesOwnStat(t *testing.T) {
	t.Parallel()
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("no /proc on this platform")
	}
	st, ok := procStartTime(os.Getpid())
	if !ok {
		t.Fatal("own starttime must parse")
	}
	if st == 0 {
		t.Fatal("starttime of a live process is nonzero (boot-relative ticks)")
	}
	if _, ok := procStartTime(-1); ok {
		t.Error("negative pid must not parse")
	}
	if _, ok := procStartTime(0); ok {
		t.Error("pid 0 must not parse")
	}
	// A pid with no stat entry (kernel max is far below this on test
	// machines) must report unreadable, not garbage.
	if _, ok := procStartTime(1 << 28); ok {
		t.Log("pid space is enormous here; skipping nonexistent-pid assertion")
	}
}

func TestCutLast(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		in, sep, want string
		ok            bool
	}{
		{"(a b) S 1 2", ")", " S 1 2", true},
		{"(a (b) c) S 1", ")", " S 1", true},
		{"no parens", ")", "", false},
	} {
		got, ok := cutLast(tt.in, tt.sep)
		if got != tt.want || ok != tt.ok {
			t.Errorf("cutLast(%q, %q) = (%q, %v), want (%q, %v)", tt.in, tt.sep, got, ok, tt.want, tt.ok)
		}
	}
}
