package symphony

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/andrew-bierman/lambe/internal/proc"
	"github.com/andrew-bierman/lambe/internal/tracker"
)

// ErrNoContinuation blocks recovery of a claim that carries no durable
// continuation state (a claim from an older installation, or one whose
// checkpoint was lost). Recovery keeps the claim, and the registry below
// tracks how long it has been blocked.
var ErrNoContinuation = errors.New("no durable continuation state; inspect any old agent before releasing its stale claim")

// OrphanClaim is one recovered-but-blocked claim: held by this daemon's
// actor, with nothing durable to resume from.
type OrphanClaim struct {
	Issue string
	// Owner is the assignee recorded when the claim was last seen.
	Owner string
	// Since is when the claim first blocked recovery; it survives restarts.
	Since time.Time
	// Reason is the last blocked reason.
	Reason string
	// Workspace is the expected workspace path, and WorkspacePresent
	// reports whether it exists on disk.
	Workspace        string
	WorkspacePresent bool
	// Lapsed reports that the daemon stopped heartbeating the claim
	// (supervisor.orphan_claim_ttl passed and no recorded agent is alive),
	// so its lease expires on its own. Daemon-local; not persisted.
	Lapsed bool
}

// OrphanRegistry is the daemon's concurrency-safe view of orphaned claims:
// the recovery loop writes it, the heartbeat loop reads it. A nil registry
// is valid and reports nothing.
type OrphanRegistry struct {
	mu sync.Mutex
	m  map[string]OrphanClaim
}

// Set records or refreshes a claim, keeping its lapsed state.
func (r *OrphanRegistry) Set(c OrphanClaim) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.m == nil {
		r.m = map[string]OrphanClaim{}
	}
	c.Lapsed = c.Lapsed || r.m[c.Issue].Lapsed
	r.m[c.Issue] = c
}

// Lapse marks a claim as no longer heartbeated.
func (r *OrphanRegistry) Lapse(id string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.m[id]; ok {
		c.Lapsed = true
		r.m[id] = c
	}
}

// Delete forgets a claim that recovery resolved.
func (r *OrphanRegistry) Delete(id string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.m, id)
}

// Get returns one claim.
func (r *OrphanRegistry) Get(id string) (OrphanClaim, bool) {
	if r == nil {
		return OrphanClaim{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.m[id]
	return c, ok
}

// List returns every tracked claim, oldest first.
func (r *OrphanRegistry) List() []OrphanClaim {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]OrphanClaim, 0, len(r.m))
	for _, c := range r.m {
		out = append(out, c)
	}
	sortOrphans(out)
	return out
}

func sortOrphans(c []OrphanClaim) {
	sort.Slice(c, func(i, j int) bool {
		if !c[i].Since.Equal(c[j].Since) {
			return c[i].Since.Before(c[j].Since)
		}
		return c[i].Issue < c[j].Issue
	})
}

// ScanOrphanClaims lists the blocked claims recorded in a checkpoint
// directory. It reads files only, so `lambe status` and `lambe claims` can
// answer while the daemon is down. A missing directory is not an error.
func ScanOrphanClaims(dir string) ([]OrphanClaim, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []OrphanClaim
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, e.Name())
		f, err := os.Open(path) // #nosec G304 -- private checkpoint dir under the state root
		if err != nil {
			continue
		}
		var saved checkpoint
		err = json.NewDecoder(io.LimitReader(f, 8<<20)).Decode(&saved)
		_ = f.Close()
		if err != nil || saved.Phase != checkpointLegacy || saved.Issue.ID == "" {
			continue
		}
		since := saved.BlockedSince
		if since.IsZero() {
			if info, err := e.Info(); err == nil {
				since = info.ModTime()
			}
		}
		c := OrphanClaim{Issue: saved.Issue.ID, Owner: saved.Issue.Assignee, Since: since, Reason: saved.Error, Workspace: saved.Workspace}
		if c.Reason == "" {
			c.Reason = ErrNoContinuation.Error()
		}
		if c.Workspace != "" {
			_, err := os.Stat(c.Workspace)
			c.WorkspacePresent = err == nil
		}
		out = append(out, c)
	}
	sortOrphans(out)
	return out, nil
}

// RemoveCheckpointFile deletes one issue's checkpoint from dir.
func RemoveCheckpointFile(dir, issue string) error {
	err := os.Remove(filepath.Join(dir, SanitizeKey(issue)+".json"))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// AgentGone reports whether the newest recorded agent of a session is
// provably dead. It never signals anything. An unreadable record, a live
// group, or an identity it cannot verify returns false with the reason: a
// caller must not treat "unknown" as "gone".
func AgentGone(ctx context.Context, tr tracker.Tracker, id string) (bool, string) {
	attempts, err := tr.Attempts(ctx, id)
	if err != nil {
		return false, fmt.Sprintf("attempt records unreadable: %v", err)
	}
	var latest tracker.Attempt
	for _, a := range attempts {
		if a.Number > latest.Number {
			latest = a
		}
	}
	if latest.PGID == 0 {
		return true, ""
	}
	if latest.PGID <= 1 {
		return false, fmt.Sprintf("invalid recorded process group %d", latest.PGID)
	}
	kerr := syscall.Kill(-latest.PGID, 0)
	if errors.Is(kerr, syscall.ESRCH) {
		return true, ""
	}
	identity, ierr := proc.ProcessIdentity(latest.PGID)
	if ierr == nil && latest.ProcessIdentity != "" && identity != latest.ProcessIdentity {
		return true, ""
	}
	if kerr != nil {
		return false, fmt.Sprintf("cannot verify recorded process group %d: %v", latest.PGID, kerr)
	}
	return false, fmt.Sprintf("recorded process group %d is still alive or unverifiable", latest.PGID)
}
