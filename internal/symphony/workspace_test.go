package symphony

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSanitizeKey(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		key   string
		want  string
		exact bool
	}{
		{name: "allowed characters unchanged", key: "issue-42_v3.1", want: "issue-42_v3.1", exact: true},
		{name: "slashes and spaces become dashes plus hash", key: "feat/build the UI", want: "feat-build-the-UI-", exact: false},
		{name: "unicode is replaced", key: "héllo", want: "h-llo-", exact: false},
		{name: "traversal is neutralized", key: "../../etc/passwd", want: "..-..-etc-passwd-", exact: false},
		{name: "empty key falls back", key: "///", want: "issue-", exact: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := SanitizeKey(tt.key)
			if tt.exact {
				if got != tt.want {
					t.Errorf("SanitizeKey(%q) = %q, want %q", tt.key, got, tt.want)
				}
				return
			}
			if !strings.HasPrefix(got, tt.want) {
				t.Errorf("SanitizeKey(%q) = %q, want prefix %q", tt.key, got, tt.want)
			}
			suffix := strings.TrimPrefix(got, tt.want)
			if len(suffix) < 16 { // 64-bit hash = 16 hex characters
				t.Errorf("SanitizeKey(%q) suffix %q shorter than 64 bits", tt.key, suffix)
			}
		})
	}
}

func TestSanitizeKeyCollisionDisambiguation(t *testing.T) {
	t.Parallel()
	// Both keys sanitize to "a-b" but must not share a workspace.
	a := SanitizeKey("a b")
	b := SanitizeKey("a/b")
	if a == b {
		t.Fatalf("collision not disambiguated: %q == %q (from %q and %q)", a, b, "a b", "a/b")
	}
	if !strings.HasPrefix(a, "a-b-") || !strings.HasPrefix(b, "a-b-") {
		t.Errorf("unexpected sanitized forms: %q, %q", a, b)
	}
	if SanitizeKey("a b") != a || SanitizeKey("a/b") != b {
		t.Error("sanitization is not deterministic")
	}
}

func TestWorkspaceEnsureAfterCreateOnlyOnNewDir(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	m := NewWorkspaceManager(root, nil)
	hooks := map[HookName]string{HookAfterCreate: "echo created >> .marker"}

	ws, err := m.Ensure(context.Background(), "issue/1", hooks)
	if err != nil {
		t.Fatal(err)
	}
	if !ws.IsNew {
		t.Error("first Ensure should report IsNew")
	}
	again, err := m.Ensure(context.Background(), "issue/1", hooks)
	if err != nil {
		t.Fatal(err)
	}
	if again.IsNew {
		t.Error("second Ensure should reuse the existing workspace")
	}
	data, err := os.ReadFile(filepath.Join(ws.Dir, ".marker"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), "created") != 1 {
		t.Errorf("after_create ran %d times, want 1", strings.Count(string(data), "created"))
	}
}

func TestWorkspaceAfterCreateFailureRetriedLater(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	m := NewWorkspaceManager(root, nil)
	gate := filepath.Join(root, "fail-create")
	if err := os.WriteFile(gate, []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	hooks := map[HookName]string{HookAfterCreate: "test ! -e " + gate}

	if _, err := m.Ensure(context.Background(), "issue/2", hooks); err == nil {
		t.Fatal("after_create failure should be fatal to creation")
	}
	// The failed directory was removed so a later retry re-runs after_create.
	if _, err := os.Stat(filepath.Join(root, SanitizeKey("issue/2"))); !os.IsNotExist(err) {
		t.Errorf("failed workspace directory still present: %v", err)
	}
	if err := os.Remove(gate); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Ensure(context.Background(), "issue/2", hooks); err != nil {
		t.Fatalf("retry after fixing the hook: %v", err)
	}
}

func TestWorkspaceRemoveIgnoresBeforeRemoveFailure(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	m := NewWorkspaceManager(root, nil)
	hooks := map[HookName]string{HookBeforeRemove: "exit 1"}
	if _, err := m.Ensure(context.Background(), "issue/3", nil); err != nil {
		t.Fatal(err)
	}
	if err := m.Remove(context.Background(), "issue/3", hooks); err != nil {
		t.Errorf("Remove should ignore before_remove failure: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, SanitizeKey("issue/3"))); !os.IsNotExist(err) {
		t.Error("workspace not removed")
	}
}

func TestWorkspaceHookTimeout(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	m := NewWorkspaceManager(root, nil)
	m.HookTimeout = 50 * time.Millisecond
	dir := filepath.Join(root, "ws")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := m.RunHook(context.Background(), HookBeforeRun, dir, "sleep 5", m.HookTimeout); err == nil {
		t.Fatal("expected hook timeout error")
	}
}

func TestWorkspaceSweep(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	m := NewWorkspaceManager(root, nil)
	stale := filepath.Join(root, "stale-ws")
	live := filepath.Join(root, "live-ws")
	for _, dir := range []string{stale, live} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Sweep(func(dirName string) bool { return dirName == "live-ws" }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("stale workspace survived the sweep")
	}
	if _, err := os.Stat(live); err != nil {
		t.Error("live workspace was swept")
	}
}
