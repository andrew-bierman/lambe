package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/andrew-bierman/lambe/internal/proc"
	"github.com/andrew-bierman/lambe/internal/symphony"
	"github.com/andrew-bierman/lambe/internal/tracker"
)

// Orphan is one persisted process-group binding discovered at startup: the
// pgid recorded through the proc Persist seam and the session (issue id) it
// was spawned for.
type Orphan struct {
	PGID      int
	SessionID string
}

// OrphanSource yields the persisted pgid/session bindings left behind by a
// previous daemon life.
type OrphanSource interface {
	Orphans(ctx context.Context) ([]Orphan, error)
}

// Adoption is one orphan the daemon re-took ownership of at startup; its
// process group stays running and its claim stays heartbeated.
type Adoption struct {
	Orphan
	Issue tracker.Issue
}

// Recover decides the fate of every orphan left by a previous daemon life.
//
// For each orphan the daemon re-verifies lease ownership before acting: if
// the heartbeat confirms this daemon's actor still holds the claim and the
// issue is still active, the orphan is adopted (recorded for resume). If the
// assignee changed or the claim was lost, the daemon stands down without
// killing — the work is someone else's now. If the issue is gone or
// terminal, the orphaned process group is killed with the bounded ladder.
// An ownership that cannot be verified (tracker error) is never killed.
func Recover(ctx context.Context, src OrphanSource, tr tracker.Tracker, actor string, grace time.Duration, log *slog.Logger) ([]Adoption, error) {
	if log == nil {
		log = slog.Default()
	}
	orphans, err := src.Orphans(ctx)
	if err != nil {
		return nil, fmt.Errorf("read persisted orphans: %w", err)
	}
	var adopted []Adoption
	for _, o := range orphans {
		issue, ok, err := lookupIssue(ctx, tr, o.SessionID)
		if err != nil {
			// Unverified ownership: never kill, never adopt.
			log.Error("orphan ownership unverified, standing down",
				"pgid", o.PGID, "session", o.SessionID,
				"category", CategoryUnavailable, "error", err)
			continue
		}
		if !ok || !symphony.StatusActive(issue.Status) {
			log.Info("orphan issue gone or terminal, killing group", "pgid", o.PGID, "session", o.SessionID)
			killCtx, cancel := context.WithTimeout(ctx, grace)
			kerr := proc.TerminateGroup(killCtx, o.PGID, grace)
			cancel()
			if kerr != nil {
				log.Error("orphan group kill failed", "pgid", o.PGID, "category", CategoryUnknown, "error", kerr)
			}
			continue
		}
		if issue.Assignee == "" || issue.Assignee != actor {
			log.Info("orphan claim held by another assignee, standing down",
				"pgid", o.PGID, "session", o.SessionID, "assignee", issue.Assignee)
			continue
		}
		if err := tr.Heartbeat(ctx, o.SessionID); err != nil {
			if errors.Is(err, tracker.ErrNotOwner) {
				log.Info("orphan claim lost, standing down without killing",
					"pgid", o.PGID, "session", o.SessionID)
				continue
			}
			log.Error("orphan lease check failed, standing down",
				"pgid", o.PGID, "session", o.SessionID,
				"category", CategoryUnavailable, "error", err)
			continue
		}
		log.Info("orphan adopted for resume", "pgid", o.PGID, "session", o.SessionID)
		adopted = append(adopted, Adoption{Orphan: o, Issue: issue})
	}
	return adopted, nil
}

// lookupIssue reads one issue by id, reporting whether it still exists in
// an answerable state.
func lookupIssue(ctx context.Context, tr tracker.Tracker, id string) (tracker.Issue, bool, error) {
	issues, err := tr.Dispatchable(ctx, tracker.DispatchQuery{IDs: []string{id}})
	if err != nil {
		return tracker.Issue{}, false, err
	}
	for _, issue := range issues {
		if issue.ID == id {
			return issue, true, nil
		}
	}
	return tracker.Issue{}, false, nil
}

// TrackerOrphans is the default OrphanSource: it derives orphans from the
// tracker's own attempt records — the newest attempt of every in-progress
// session that carries a persisted pgid.
type TrackerOrphans struct {
	Tracker tracker.Tracker
}

// Orphans implements OrphanSource.
func (t TrackerOrphans) Orphans(ctx context.Context) ([]Orphan, error) {
	issues, err := t.Tracker.Dispatchable(ctx, tracker.DispatchQuery{Statuses: []string{tracker.StatusInProgress}})
	if err != nil {
		return nil, fmt.Errorf("read in-progress sessions: %w", err)
	}
	var orphans []Orphan
	for _, issue := range issues {
		attempts, err := t.Tracker.Attempts(ctx, issue.ID)
		if err != nil {
			return nil, fmt.Errorf("read attempts for %s: %w", issue.ID, err)
		}
		for i := len(attempts) - 1; i >= 0; i-- {
			if attempts[i].PGID > 0 {
				orphans = append(orphans, Orphan{PGID: attempts[i].PGID, SessionID: issue.ID})
				break
			}
		}
	}
	return orphans, nil
}
