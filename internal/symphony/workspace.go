package symphony

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/andrew-bierman/lambe/internal/proc"
)

// SanitizeKey reduces an arbitrary issue identifier to a workspace-safe key
// over [A-Za-z0-9._-]: disallowed runes become '-' and runs collapse to one
// dash. When sanitization changed the identifier, a 64-bit hash suffix of
// the original key is appended so two different identifiers that sanitize
// identically never share a workspace.
func SanitizeKey(key string) string {
	var b strings.Builder
	lastDash := false
	changed := false
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			b.WriteRune(r)
			lastDash = r == '-'
			continue
		default:
			changed = true
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	sanitized := strings.Trim(b.String(), "-")
	if sanitized == "" {
		sanitized = "issue"
		changed = true
	}
	if sanitized != key {
		changed = true
	}
	if !changed {
		return sanitized
	}
	sum := sha256.Sum256([]byte(key))
	return sanitized + "-" + hex.EncodeToString(sum[:8])
}

// Workspace is one per-issue working directory under the manager's root.
type Workspace struct {
	// Key is the sanitized workspace key.
	Key string
	// Dir is the absolute workspace path, always under the root.
	Dir string
	// IsNew reports whether this Ensure created the directory.
	IsNew bool
}

// WorkspaceManager creates, removes, and sweeps per-issue workspaces under
// Root, running the four lifecycle hooks with the workspace as cwd.
type WorkspaceManager struct {
	Root        string
	HookTimeout time.Duration
	Log         *slog.Logger
}

// NewWorkspaceManager returns a manager rooted at root. A nil logger uses
// slog.Default.
func NewWorkspaceManager(root string, log *slog.Logger) *WorkspaceManager {
	if log == nil {
		log = slog.Default()
	}
	return &WorkspaceManager{Root: root, HookTimeout: DefaultHookTimeout, Log: log}
}

// dirFor returns the workspace directory for a sanitized key, refusing
// paths that escape the root.
func (m *WorkspaceManager) dirFor(key string) (string, error) {
	dir := filepath.Join(m.Root, key)
	rel, err := filepath.Rel(m.Root, dir)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("workspace path %s escapes root %s", dir, m.Root)
	}
	return dir, nil
}

// Ensure returns the workspace for key, creating it when absent. On a new
// directory the after_create hook runs with the workspace as cwd; its
// failure is fatal to creation: the directory is removed and the error
// returned, so creation can be retried on a later tick. Existing
// directories are reused across runs and never re-run after_create.
func (m *WorkspaceManager) Ensure(ctx context.Context, key string, hooks map[HookName]string) (*Workspace, error) {
	dir, err := m.dirFor(SanitizeKey(key))
	if err != nil {
		return nil, err
	}
	existed := true
	if _, statErr := os.Stat(dir); statErr != nil {
		if !os.IsNotExist(statErr) {
			return nil, fmt.Errorf("stat workspace: %w", statErr)
		}
		existed = false
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("create workspace: %w", err)
	}
	if existed {
		return &Workspace{Key: SanitizeKey(key), Dir: dir, IsNew: false}, nil
	}
	if cmd := hooks[HookAfterCreate]; cmd != "" {
		if hookErr := m.RunHook(ctx, HookAfterCreate, dir, cmd, m.hookTimeout()); hookErr != nil {
			m.Log.Error("after_create hook failed, removing workspace", "dir", dir, "error", hookErr)
			if rmErr := os.RemoveAll(dir); rmErr != nil {
				m.Log.Error("remove failed workspace", "dir", dir, "error", rmErr)
			}
			return nil, fmt.Errorf("after_create hook: %w", hookErr)
		}
	}
	return &Workspace{Key: SanitizeKey(key), Dir: dir, IsNew: true}, nil
}

// Remove runs before_remove (failures logged and ignored — cleanup
// proceeds) and then deletes the workspace directory.
func (m *WorkspaceManager) Remove(ctx context.Context, key string, hooks map[HookName]string) error {
	dir, err := m.dirFor(SanitizeKey(key))
	if err != nil {
		return err
	}
	if cmd := hooks[HookBeforeRemove]; cmd != "" {
		if hookErr := m.RunHook(ctx, HookBeforeRemove, dir, cmd, m.hookTimeout()); hookErr != nil {
			m.Log.Warn("before_remove hook failed, removing anyway", "dir", dir, "error", hookErr)
		}
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("remove workspace: %w", err)
	}
	return nil
}

// RunBeforeRun runs the before_run hook in dir. A failure aborts the
// attempt.
func (m *WorkspaceManager) RunBeforeRun(ctx context.Context, dir string, hooks map[HookName]string) error {
	cmd := hooks[HookBeforeRun]
	if cmd == "" {
		return nil
	}
	return m.RunHook(ctx, HookBeforeRun, dir, cmd, m.hookTimeout())
}

// RunAfterRun runs the after_run hook in dir after every attempt,
// including timed-out or canceled ones. Failures are logged and ignored.
func (m *WorkspaceManager) RunAfterRun(ctx context.Context, dir string, hooks map[HookName]string) {
	cmd := hooks[HookAfterRun]
	if cmd == "" {
		return
	}
	if err := m.RunHook(ctx, HookAfterRun, dir, cmd, m.hookTimeout()); err != nil {
		m.Log.Warn("after_run hook failed, ignored", "dir", dir, "error", err)
	}
}

// RunHook executes one hook command with sh -lc, the workspace as cwd, and
// the configured timeout.
func (m *WorkspaceManager) RunHook(ctx context.Context, name HookName, dir, command string, timeout time.Duration) error {
	if command == "" {
		return nil
	}
	hookCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := proc.RunCommand(hookCtx, dir, "sh", "-lc", command)
	if hookCtx.Err() != nil {
		return fmt.Errorf("hook %s timed out after %s", name, timeout)
	}
	if err != nil {
		return fmt.Errorf("hook %s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Sweep deletes every workspace directory under Root for which keep returns
// false. It is the startup sweep: workspaces are reused across runs and
// deleted only here or on terminal-state transitions.
func (m *WorkspaceManager) Sweep(keep func(dirName string) bool) error {
	entries, err := os.ReadDir(m.Root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("sweep workspaces: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if keep(e.Name()) {
			continue
		}
		m.Log.Info("startup sweep removing stale workspace", "dir", e.Name())
		if err := os.RemoveAll(filepath.Join(m.Root, e.Name())); err != nil {
			return fmt.Errorf("sweep remove %s: %w", e.Name(), err)
		}
	}
	return nil
}

func (m *WorkspaceManager) hookTimeout() time.Duration {
	if m.HookTimeout <= 0 {
		return DefaultHookTimeout
	}
	return m.HookTimeout
}
