package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/andrew-bierman/lambe/internal/proc"
	"github.com/andrew-bierman/lambe/internal/symphony"
	"github.com/andrew-bierman/lambe/internal/tracker"
)

// Startup recovery bounds: parallel ownership reads, each time-boxed, and an
// overall budget so a saturated tracker cannot hold up the daemon.
const (
	recoverConcurrency = 4
	recoverLookupBound = 15 * time.Second
	recoverBudget      = 60 * time.Second
)

// Orphan is one persisted process-group binding discovered at startup: the
// pgid recorded through the proc Persist seam and the session (issue id) it
// was spawned for.
type Orphan struct {
	PGID      int
	SessionID string
	// StartTime is the group leader's /proc/<pid>/stat field 22 recorded
	// at spawn time. It is the identity check that makes the recorded
	// pgid safe to kill: pids are recycled, so a stale pgid without a
	// matching starttime may point at an unrelated process. Zero means no
	// starttime was recorded, in which case recovery cannot verify the
	// group and stands down without killing.
	StartTime uint64
	// ProcessIdentity is the native kernel identity recorded at spawn.
	ProcessIdentity string
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
	// Ownership reads are independent and read-only: run them with bounded
	// parallelism, each time-boxed, then act on the results in order. A
	// read that did not finish is an unverified ownership (stand down).
	lookCtx, cancel := context.WithTimeout(ctx, recoverBudget)
	defer cancel()
	looks := make([]struct {
		issue tracker.Issue
		ok    bool
		err   error
	}, len(orphans))
	sem := make(chan struct{}, recoverConcurrency)
	var wg sync.WaitGroup
	for i, o := range orphans {
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			rctx, rcancel := context.WithTimeout(lookCtx, recoverLookupBound)
			defer rcancel()
			looks[i].issue, looks[i].ok, looks[i].err = lookupIssue(rctx, tr, o.SessionID)
		}()
	}
	wg.Wait()
	var adopted []Adoption
	for i, o := range orphans {
		issue, ok, err := looks[i].issue, looks[i].ok, looks[i].err
		if err != nil {
			// Unverified ownership: never kill, never adopt.
			log.Error("orphan ownership unverified, standing down",
				"pgid", o.PGID, "session", o.SessionID,
				"category", CategoryUnavailable, "error", err)
			continue
		}
		if ok && issue.Assignee != "" && issue.Assignee != actor {
			log.Info("orphan owned by another actor; preserving group", "session", o.SessionID)
			continue
		}
		if !ok || !symphony.StatusActive(issue.Status) {
			// Identity check before any kill: the recorded pgid is only
			// safe to signal when the group leader's starttime still
			// matches the one recorded at spawn. Anything unverifiable —
			// no recorded starttime (non-Linux persist, older records), a
			// gone or unreadable leader, a recycled pid — is a stand-down.
			if !orphanIdentityHolds(log, o) {
				continue
			}
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

// orphanIdentityHolds reports whether the orphan's recorded group identity
// still matches the live kernel state: the group leader's /proc starttime
// equals the recorded one. On any mismatch or unverifiable reading it logs
// a stand-down and returns false — the daemon never kills a group it
// cannot prove is the one it spawned. A leader that is simply gone is
// also a stand-down (the ladder is unnecessary but unprovable; a later
// reap or the next boot clears it).
func orphanIdentityHolds(log *slog.Logger, o Orphan) bool {
	if o.ProcessIdentity != "" {
		live, err := proc.ProcessIdentity(o.PGID)
		return err == nil && live == o.ProcessIdentity
	}
	if o.StartTime == 0 {
		log.Warn("orphan kill refused: no recorded starttime to verify pgid against, standing down",
			"pgid", o.PGID, "session", o.SessionID)
		return false
	}
	live, ok := procStartTime(o.PGID)
	if !ok {
		log.Warn("orphan kill refused: group leader starttime unreadable, standing down",
			"pgid", o.PGID, "session", o.SessionID)
		return false
	}
	if live != o.StartTime {
		log.Warn("orphan kill refused: pgid starttime mismatch (recycled pid?), standing down",
			"pgid", o.PGID, "session", o.SessionID,
			"recorded_starttime", o.StartTime, "live_starttime", live)
		return false
	}
	return true
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
// session that carries a persisted pgid. One session's failing Attempts
// read is logged and skipped; it never aborts the recovery of the others.
type TrackerOrphans struct {
	Tracker tracker.Tracker
	// Log receives per-session skip warnings; nil uses the default logger.
	Log *slog.Logger
}

// Orphans implements OrphanSource.
func (t TrackerOrphans) Orphans(ctx context.Context) ([]Orphan, error) {
	log := t.Log
	if log == nil {
		log = slog.Default()
	}
	issues, err := t.Tracker.Dispatchable(ctx, tracker.DispatchQuery{Statuses: []string{tracker.StatusInProgress}})
	if err != nil {
		return nil, fmt.Errorf("read in-progress sessions: %w", err)
	}
	if len(issues) == 0 {
		return nil, nil
	}
	records, bulkErr := t.Tracker.ListByMetadata(ctx, tracker.MetaKind, tracker.KindAttempt)
	grouped := tracker.GroupAttempts(records)
	var orphans []Orphan
	for _, issue := range issues {
		attempts := grouped[issue.ID]
		var err error
		if bulkErr != nil {
			attempts, err = t.Tracker.Attempts(ctx, issue.ID)
		}
		if err != nil {
			log.Warn("orphan scan: attempts read failed, skipping session",
				"session", issue.ID, "category", ErrorCategory(err), "error", err)
			continue
		}
		var latest tracker.Attempt
		for _, attempt := range attempts {
			if attempt.Number > latest.Number {
				latest = attempt
			}
		}
		if latest.PGID > 0 {
			orphans = append(orphans, Orphan{PGID: latest.PGID, SessionID: issue.ID, ProcessIdentity: latest.ProcessIdentity})
		}
	}
	return orphans, nil
}
