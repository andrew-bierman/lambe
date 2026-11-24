package daemon

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/andrew-bierman/lambe/internal/config"
	"github.com/andrew-bierman/lambe/internal/symphony"
	"github.com/andrew-bierman/lambe/internal/tracker"
)

// CheckpointDir is where one configured daemon keeps its session
// checkpoints under stateDir.
func CheckpointDir(stateDir string, cfg *config.Config) string {
	return filepath.Join(stateDir, "sessions", strings.TrimSuffix(InstanceLockName(cfg), ".lock"))
}

// ListOrphanClaims returns the blocked claims recorded on disk that the
// tracker still shows in progress, oldest first. It is read-only and does
// not need a running daemon.
func ListOrphanClaims(ctx context.Context, tr tracker.Tracker, stateDir string, cfg *config.Config) ([]symphony.OrphanClaim, error) {
	recorded, err := symphony.ScanOrphanClaims(CheckpointDir(stateDir, cfg))
	if err != nil || len(recorded) == 0 {
		return nil, err
	}
	active, err := tr.Dispatchable(ctx, tracker.DispatchQuery{Statuses: []tracker.Status{tracker.StatusInProgress}})
	if err != nil {
		return nil, fmt.Errorf("read in-progress claims: %w", err)
	}
	held := make(map[string]bool, len(active))
	for _, issue := range active {
		held[issue.ID] = true
	}
	var out []symphony.OrphanClaim
	for _, c := range recorded {
		if held[c.Issue] {
			out = append(out, c)
		}
	}
	return out, nil
}

// ReleaseOrphanClaim gives one orphaned claim back to the open pool. It is
// the only way lambe ever releases such a claim, and only on explicit
// operator request. Unless force is set it refuses when the issue is not a
// recorded orphan, when its workspace still exists, or when its recorded
// agent is not provably dead. The release itself is guarded by tr's actor,
// so a claim held by anyone else is never touched.
func ReleaseOrphanClaim(ctx context.Context, tr tracker.Tracker, stateDir string, cfg *config.Config, issue string, force bool) error {
	if !force {
		claims, err := ListOrphanClaims(ctx, tr, stateDir, cfg)
		if err != nil {
			return err
		}
		var c *symphony.OrphanClaim
		for i := range claims {
			if claims[i].Issue == issue {
				c = &claims[i]
			}
		}
		switch {
		case c == nil:
			return fmt.Errorf("%s is not a recorded orphaned claim (see `lambe claims list`); use --force to release any claim held by this actor", issue)
		case c.WorkspacePresent:
			return fmt.Errorf("%s still has a workspace at %s; inspect it, or use --force", issue, c.Workspace)
		}
		if gone, why := symphony.AgentGone(ctx, tr, issue); !gone {
			return fmt.Errorf("%s: %s; stop that agent first, or use --force", issue, why)
		}
	}
	if err := tr.Release(ctx, issue); err != nil {
		if errors.Is(err, tracker.ErrNotOwner) {
			return fmt.Errorf("%s is not claimed by this actor: %w", issue, err)
		}
		return fmt.Errorf("release %s: %w", issue, err)
	}
	return symphony.RemoveCheckpointFile(CheckpointDir(stateDir, cfg), issue)
}
