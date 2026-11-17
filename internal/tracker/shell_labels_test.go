package tracker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestShellTransportListPassesLabelFilter verifies that the shell transport
// passes the Labels field of DispatchQuery as --label to bd list.
func TestShellTransportListPassesLabelFilter(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// A fake bd that writes its args to a file, then exits 0 with empty JSON.
	script := filepath.Join(dir, "bd")
	outFile := filepath.Join(dir, "args.txt")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho \"$@\" > "+outFile+"\necho '[]'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(script, 0o755); err != nil { //nolint:gosec // executable test fixture
		t.Fatal(err)
	}
	tr := NewShellTransport(script, dir, "lambe@test")

	_, err := tr.List(context.Background(), DispatchQuery{
		Statuses: []string{StatusReady},
		Labels:   []string{"lambe:babysit", "priority:high"},
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	raw, err := os.ReadFile(outFile) //nolint:gosec // test fixture path from t.TempDir()
	if err != nil {
		t.Fatal(err)
	}
	args := string(raw)
	if !strings.Contains(args, "--label") {
		t.Errorf("expected --label flag in args, got: %s", args)
	}
	if !strings.Contains(args, "lambe:babysit,priority:high") {
		t.Errorf("expected comma-joined labels in args, got: %s", args)
	}
}

// TestShellTransportListOmitsLabelWhenEmpty verifies that no --label flag is
// passed when Labels is empty, preserving the pre-filter behavior.
func TestShellTransportListOmitsLabelWhenEmpty(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	script := filepath.Join(dir, "bd")
	outFile := filepath.Join(dir, "args.txt")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho \"$@\" > "+outFile+"\necho '[]'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(script, 0o755); err != nil { //nolint:gosec // executable test fixture
		t.Fatal(err)
	}
	tr := NewShellTransport(script, dir, "lambe@test")

	_, err := tr.List(context.Background(), DispatchQuery{Statuses: []string{StatusReady}})
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	raw, err := os.ReadFile(outFile) //nolint:gosec // test fixture path from t.TempDir()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "--label") {
		t.Errorf("--label should not appear when Labels is empty, got: %s", raw)
	}
}
