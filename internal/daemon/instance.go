package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/andrew-bierman/lambe/internal/config"
)

// InstanceLockName derives the daemon's single-instance lock file name from
// the configuration's tracker paths. Instances driving the same trackers
// serialize against the same lock; instances with disjoint trackers run in
// parallel (each is single-instance for its own responsibility set).
//
// Empty-name fallback keeps the pre-existing machine-global default for
// configs with no trackers.
func InstanceLockName(cfg *config.Config) string {
	paths := make([]string, 0, len(cfg.Trackers))
	for _, tr := range cfg.Trackers {
		abs, err := filepath.Abs(tr.Path)
		if err != nil {
			abs = tr.Path
		}
		paths = append(paths, abs)
	}
	if len(paths) == 0 {
		return "lambe.lock"
	}
	sort.Strings(paths)
	sum := sha256.Sum256([]byte(strings.Join(paths, "\x00")))
	return fmt.Sprintf("lambe-%s.lock", hex.EncodeToString(sum[:6]))
}

// ResolveWorkflowPath defaults the WORKFLOW.md location to the primary
// tracker directory (where examples/WORKFLOWS/default.md tells operators to
// put it), falling back to the historical cwd-relative "WORKFLOW.md" when the
// tracker copy does not exist. An explicit cfg override is future work; the
// returned path is always absolute when tracker-derived.
func ResolveWorkflowPath(cfg *config.Config) string {
	if len(cfg.Trackers) == 0 {
		return "WORKFLOW.md"
	}
	candidate := filepath.Join(cfg.Trackers[0].Path, "WORKFLOW.md")
	if abs, err := filepath.Abs(candidate); err == nil {
		candidate = abs
	}
	if _, err := os.Stat(candidate); err == nil {
		return candidate
	}
	return "WORKFLOW.md"
}
