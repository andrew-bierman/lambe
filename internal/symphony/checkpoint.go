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

// ErrWorkspaceRootChanged prevents restarting saved work under a different root.
var ErrWorkspaceRootChanged = errors.New("workspace root differs from saved checkpoint")

type checkpointPhase string

const (
	checkpointClaiming    checkpointPhase = "claiming"
	checkpointRunning     checkpointPhase = "running"
	checkpointPreparing   checkpointPhase = "preparing"
	checkpointRetry       checkpointPhase = "retry"
	checkpointGate        checkpointPhase = "gating"
	checkpointDeferred    checkpointPhase = "deferred"
	checkpointInterrupted checkpointPhase = "interrupted"
	checkpointLegacy      checkpointPhase = "legacy-blocked"
)

type checkpoint struct {
	Version       int             `json:"version"`
	Phase         checkpointPhase `json:"phase"`
	Issue         tracker.Issue   `json:"issue"`
	Workflow      *Workflow       `json:"workflow"`
	Attempt       int             `json:"attempt"`
	Turn          int             `json:"turn"`
	Thread        string          `json:"thread,omitempty"`
	ThreadHarness string          `json:"thread_harness,omitempty"`
	LastGate      string          `json:"last_gate,omitempty"`
	GateOutput    string          `json:"gate_output,omitempty"`
	Key           string          `json:"key"`
	Workspace     string          `json:"workspace,omitempty"`
	Retry         *Retry          `json:"retry,omitempty"`
	Error         string          `json:"error,omitempty"`
	// BlockedSince is when recovery first found no durable continuation
	// for this claim. It is persisted so the orphan clock survives daemon
	// restarts.
	BlockedSince time.Time `json:"blocked_since,omitempty"`
}

func (l *Loop) checkpointPath(id string) string {
	return filepath.Join(l.CheckpointDir, SanitizeKey(id)+".json")
}

func (l *Loop) saveCheckpoint(s *session, phase checkpointPhase, retry *Retry) error {
	l.progress(s, string(phase), retry)
	if l.CheckpointDir == "" {
		return nil
	}
	return l.writeCheckpoint(checkpoint{Version: 1, Phase: phase, Issue: s.issue, Workflow: s.wf, Attempt: s.attempt, Turn: s.turn, Thread: s.thread, ThreadHarness: s.threadHarness, LastGate: s.lastGate, GateOutput: s.gateOutput, Key: s.key, Workspace: s.workspace, Retry: retry})
}

func (l *Loop) writeCheckpoint(saved checkpoint) error {
	if err := os.MkdirAll(l.CheckpointDir, 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(saved)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(l.CheckpointDir, ".checkpoint-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(file.Name()) }()
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(file.Name(), l.checkpointPath(saved.Issue.ID)); err != nil {
		return err
	}
	dir, err := os.Open(l.CheckpointDir)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}

func (l *Loop) readCheckpoint(path string) (checkpoint, error) {
	file, err := os.Open(path) // #nosec G304 -- caller selects a private checkpoint under the configured state root
	if err != nil {
		return checkpoint{}, err
	}
	defer func() { _ = file.Close() }()
	var saved checkpoint
	if err := json.NewDecoder(io.LimitReader(file, 8<<20)).Decode(&saved); err != nil {
		return saved, err
	}
	if saved.Version != 1 || saved.Issue.ID == "" || saved.Key != saved.Issue.ID || saved.Workflow == nil || saved.Workflow.MaxTurns < 1 || saved.Attempt < 0 || saved.Turn < 0 {
		return saved, fmt.Errorf("invalid session checkpoint %s", path)
	}
	if saved.Retry != nil && (saved.Retry.IssueID != saved.Issue.ID || saved.Retry.Kind < RetryFailure || saved.Retry.Kind > RetryGate || saved.Retry.At.IsZero()) {
		return saved, fmt.Errorf("invalid checkpoint retry")
	}
	switch saved.Phase {
	case checkpointClaiming, checkpointPreparing, checkpointRunning, checkpointRetry, checkpointGate, checkpointDeferred, checkpointInterrupted, checkpointLegacy:
	default:
		return saved, fmt.Errorf("unknown checkpoint phase %q", saved.Phase)
	}
	if filepath.Base(path) != SanitizeKey(saved.Issue.ID)+".json" {
		return saved, fmt.Errorf("checkpoint id does not match its filename")
	}
	expected, err := l.Spaces.dirFor(SanitizeKey(saved.Key))
	if err != nil {
		return saved, err
	}
	if saved.Workspace != "" && saved.Workspace != expected {
		return saved, fmt.Errorf("%w: %s saved workspace %q, configured workspace %q; restore the previous root or migrate the checkpoint and workspace before restarting", ErrWorkspaceRootChanged, path, saved.Workspace, expected)
	}
	return saved, nil
}

func checkpointSession(saved checkpoint) *session {
	return &session{issue: saved.Issue, wf: saved.Workflow, attempt: saved.Attempt, turn: saved.Turn, thread: saved.Thread, threadHarness: saved.ThreadHarness, lastGate: saved.LastGate, gateOutput: saved.GateOutput, key: saved.Key, workspace: saved.Workspace}
}

func (l *Loop) removeCheckpoint(id string) {
	if l.CheckpointDir == "" {
		return
	}
	if err := os.Remove(l.checkpointPath(id)); err != nil && !os.IsNotExist(err) {
		l.Log.Error("remove completed checkpoint", "issue", id, "error", err)
	}
}

// Restore loads this instance's private continuation state. Ownership is
// refreshed from the tracker before any process is stopped or work resumed.
// Transient failures remain pending for later ticks; healthy sessions still recover.
func (l *Loop) Restore(ctx context.Context, actor string) error {
	if l.CheckpointDir == "" {
		return nil
	}
	l.recoveryActor = actor
	entries, err := os.ReadDir(l.CheckpointDir)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	var failures []error
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		saved, err := l.readCheckpoint(filepath.Join(l.CheckpointDir, entry.Name()))
		if err != nil {
			failures = append(failures, err)
			continue
		}
		l.recovery[saved.Issue.ID] = saved
	}
	failure := errors.Join(failures...)
	if errors.Is(failure, ErrWorkspaceRootChanged) {
		return failure
	}
	l.recoveryScan = true
	l.recoverPending(ctx)
	return failure
}

func (l *Loop) discoverLegacy(ctx context.Context) error {
	issues, err := l.Tracker.Dispatchable(ctx, tracker.DispatchQuery{Statuses: []string{tracker.StatusInProgress}})
	if err != nil {
		return err
	}
	for _, issue := range issues {
		if issue.Assignee != l.recoveryActor || issue.Metadata[tracker.MetaKind] != "" {
			continue
		}
		if _, exists := l.recovery[issue.ID]; exists {
			continue
		}
		// An older installation cannot safely reconstruct a missing conversation.
		saved := checkpoint{Version: 1, Phase: checkpointLegacy, Issue: issue, Workflow: l.Loader.Current(), Key: issue.ID, BlockedSince: time.Now()}
		l.recovery[issue.ID] = saved
		if _, err := os.Stat(l.checkpointPath(issue.ID)); os.IsNotExist(err) {
			if err := l.writeCheckpoint(saved); err != nil {
				return err
			}
		}
	}
	l.recoveryScan = false
	return nil
}

// Recovery defaults. A pass is time-boxed so a slow tracker cannot hold up
// dispatch, and a blocked claim is re-inspected only occasionally.
const (
	defaultRecoveryBudget      = 15 * time.Second
	defaultRecoveryInspect     = 10 * time.Second
	defaultRecoveryConcurrency = 4
	defaultRecoveryRecheck     = 5 * time.Minute
)

// lookup is the prefetched tracker read for one pending recovery.
type lookup struct {
	issues []tracker.Issue
	err    error
}

func (l *Loop) readIssue(ctx context.Context, id string) ([]tracker.Issue, error) {
	issues, err := l.Tracker.Dispatchable(ctx, tracker.DispatchQuery{IDs: []string{id}})
	if err != nil {
		return nil, err
	}
	if len(issues) == 0 {
		// Closed issues are omitted from bd's default list view.
		return l.Tracker.Dispatchable(ctx, tracker.DispatchQuery{IDs: []string{id}, Statuses: []string{tracker.StatusClosed}})
	}
	return issues, nil
}

// inspectAll reads the tracker state of every id with bounded parallelism,
// each read time-boxed. Ids not reached before ctx ends are absent.
func (l *Loop) inspectAll(ctx context.Context, ids []string) map[string]lookup {
	workers := l.RecoveryConcurrency
	if workers <= 0 {
		workers = defaultRecoveryConcurrency
	}
	box := l.RecoveryInspect
	if box <= 0 {
		box = defaultRecoveryInspect
	}
	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		out = make(map[string]lookup, len(ids))
		sem = make(chan struct{}, workers)
	)
	for _, id := range ids {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return out
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			ictx, cancel := context.WithTimeout(ctx, box)
			defer cancel()
			issues, err := l.readIssue(ictx, id)
			mu.Lock()
			out[id] = lookup{issues, err}
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}

func (l *Loop) recoverPending(ctx context.Context) {
	ctx, cancel := l.controlContext(ctx)
	defer cancel()
	budget := l.RecoveryBudget
	if budget <= 0 {
		budget = defaultRecoveryBudget
	}
	ctx, cancelBudget := context.WithTimeout(ctx, budget)
	defer cancelBudget()
	if l.recoveryScan {
		if err := l.discoverLegacy(ctx); err != nil {
			l.Log.Warn("legacy claim inventory unavailable; retrying next tick", "error", err)
		}
	}
	now := time.Now()
	var due []string
	for id, saved := range l.recovery {
		if saved.Phase == checkpointDeferred && saved.Retry != nil && saved.Retry.At.After(now) {
			continue
		}
		if l.sessions[id] != nil || now.Before(l.nextCheck[id]) {
			continue
		}
		due = append(due, id)
	}
	sort.Strings(due)
	looks := l.inspectAll(ctx, due)
	recheck := l.RecoveryRecheck
	if recheck <= 0 {
		recheck = defaultRecoveryRecheck
	}
	for _, id := range due {
		if ctx.Err() != nil {
			return
		}
		look, ok := looks[id]
		if !ok {
			continue
		}
		saved := l.recovery[id]
		resumed, err := l.resumeCheckpoint(ctx, saved, look)
		if err != nil {
			// Only a claim with no continuation state is parked; anything else
			// (a transient tracker error, a failed finalizer) retries next tick.
			if errors.Is(err, ErrNoContinuation) {
				l.nextCheck[id] = time.Now().Add(recheck)
			}
			l.noteBlocked(ctx, &saved, err)
			l.recovery[id] = saved
			continue
		}
		if resumed {
			delete(l.recovery, id)
			delete(l.nextCheck, id)
			l.Orphans.Delete(id)
		}
	}
}

// noteBlocked records why a claim cannot be recovered. A claim blocked for
// lack of continuation state is an orphan: its clock is persisted, and once
// it exceeds OrphanTTL (and no recorded agent can be alive) the daemon stops
// heartbeating it so the lease expires. The claim itself is never released.
func (l *Loop) noteBlocked(ctx context.Context, saved *checkpoint, err error) {
	id := saved.Issue.ID
	orphan := errors.Is(err, ErrNoContinuation)
	changed := saved.Error != err.Error()
	if orphan && saved.BlockedSince.IsZero() {
		saved.BlockedSince = time.Now()
		changed = true
	}
	saved.Error = err.Error()
	if changed {
		_ = l.writeCheckpoint(*saved)
		l.Log.Error("session recovery blocked; preserving claim and workspace", "issue", id, "error", err)
	}
	if !orphan {
		return
	}
	claim := OrphanClaim{Issue: id, Owner: saved.Issue.Assignee, Since: saved.BlockedSince, Reason: saved.Error, Workspace: saved.Workspace}
	if dir, derr := l.Spaces.dirFor(SanitizeKey(saved.Key)); derr == nil {
		claim.Workspace = dir
		_, serr := os.Stat(dir)
		claim.WorkspacePresent = serr == nil
	}
	l.Orphans.Set(claim)
	if prev, _ := l.Orphans.Get(id); prev.Lapsed || l.OrphanTTL <= 0 || time.Since(saved.BlockedSince) < l.OrphanTTL {
		return
	}
	if gone, why := AgentGone(ctx, l.Tracker, id); !gone {
		l.Log.Warn("orphaned claim past ttl keeps heartbeating", "issue", id, "reason", why)
		return
	}
	l.Orphans.Lapse(id)
	l.Log.Warn("orphaned claim past ttl; heartbeats stopped, lease will expire (release it with `lambe claims release`)",
		"issue", id, "since", saved.BlockedSince, "ttl", l.OrphanTTL)
}

func (l *Loop) resumeCheckpoint(ctx context.Context, saved checkpoint, look lookup) (bool, error) {
	issues, err := look.issues, look.err
	if err != nil {
		return false, err
	}
	if len(issues) == 0 {
		l.removeCheckpoint(saved.Issue.ID)
		return true, nil
	}
	issue := issues[0]
	if issue.Assignee != l.recoveryActor {
		// Relinquish foreign continuation state without touching its workspace or agent.
		if issue.Assignee != "" || issue.Status == tracker.StatusClosed {
			l.removeCheckpoint(issue.ID)
		}
		// Unclaimed ready work goes through the normal guarded claim path.
		return true, nil
	}
	if issue.Status == tracker.StatusClosed {
		if err := l.stopRecordedAgent(ctx, issue.ID); err != nil {
			return false, err
		}
		if saved.Phase == checkpointGate {
			if finalizer, ok := l.Gates.(CompletionFinalizer); ok {
				if err := finalizer.FinalizeCompletion(ctx, issue.ID); err != nil {
					return false, err
				}
			}
		}
		l.cleanup(ctx, checkpointSession(saved), "recovered closed issue")
		l.removeCheckpoint(issue.ID)
		return true, nil
	}
	if !StatusActive(issue.Status) {
		// Paused work keeps its workspace and continuation; never reopen it.
		return false, l.stopRecordedAgent(ctx, issue.ID)
	}
	if saved.Phase == checkpointLegacy {
		return false, ErrNoContinuation
	}
	if err := l.Tracker.Heartbeat(ctx, issue.ID); err != nil {
		return false, err
	}
	if err := l.stopRecordedAgent(ctx, issue.ID); err != nil {
		return false, err
	}
	s := checkpointSession(saved)
	s.issue = issue
	retry := Retry{IssueID: issue.ID, Kind: RetryContinuation, At: time.Now()}
	switch saved.Phase {
	case checkpointRunning, checkpointInterrupted:
		if s.turn > 0 {
			s.turn--
		}
		if s.attempt == 0 {
			retry.Kind = RetryFailure
		}
	case checkpointGate:
		retry.Kind = RetryGate
	default:
		if saved.Retry != nil {
			retry = *saved.Retry
		} else if s.attempt == 0 {
			retry.Kind = RetryFailure
		}
	}
	if err := l.saveCheckpoint(s, checkpointRetry, &retry); err != nil {
		return false, err
	}
	l.sessions[issue.ID] = s
	l.retries.Add(retry)
	l.Log.Info("session recovered", "issue", issue.ID, "attempt", s.attempt, "turn", s.turn, "thread", s.thread)
	return true, nil
}

func (l *Loop) stopRecordedAgent(ctx context.Context, id string) error {
	attempts, err := l.Tracker.Attempts(ctx, id)
	if err != nil {
		return err
	}
	var latest tracker.Attempt
	for _, attempt := range attempts {
		if attempt.Number > latest.Number {
			latest = attempt
		}
	}
	if latest.PGID == 0 {
		return nil
	}
	if latest.PGID <= 1 {
		return fmt.Errorf("invalid recorded process group %d", latest.PGID)
	}
	err = syscall.Kill(-latest.PGID, 0)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	identity, identityErr := proc.ProcessIdentity(latest.PGID)
	if identityErr == nil && latest.ProcessIdentity != "" && identity != latest.ProcessIdentity {
		// A different boot/process incarnation proves the recorded local agent is gone.
		return nil
	}
	if err != nil {
		return fmt.Errorf("cannot verify recorded process group: %w", err)
	}
	if identityErr != nil || latest.ProcessIdentity == "" {
		return fmt.Errorf("recorded process identity cannot be verified for group %d", latest.PGID)
	}
	stopCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return proc.TerminateGroup(stopCtx, latest.PGID, time.Second)
}
