package symphony

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/andrew-bierman/lambe/internal/proc"
	"github.com/andrew-bierman/lambe/internal/tracker"
)

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
	Version    int             `json:"version"`
	Phase      checkpointPhase `json:"phase"`
	Issue      tracker.Issue   `json:"issue"`
	Workflow   *Workflow       `json:"workflow"`
	Attempt    int             `json:"attempt"`
	Turn       int             `json:"turn"`
	Thread     string          `json:"thread,omitempty"`
	LastGate   string          `json:"last_gate,omitempty"`
	GateOutput string          `json:"gate_output,omitempty"`
	Key        string          `json:"key"`
	Workspace  string          `json:"workspace,omitempty"`
	Retry      *Retry          `json:"retry,omitempty"`
	Error      string          `json:"error,omitempty"`
}

func (l *Loop) checkpointPath(id string) string {
	return filepath.Join(l.CheckpointDir, SanitizeKey(id)+".json")
}

func (l *Loop) saveCheckpoint(s *session, phase checkpointPhase, retry *Retry) error {
	if l.CheckpointDir == "" {
		return nil
	}
	return l.writeCheckpoint(checkpoint{Version: 1, Phase: phase, Issue: s.issue, Workflow: s.wf, Attempt: s.attempt, Turn: s.turn, Thread: s.thread, LastGate: s.lastGate, GateOutput: s.gateOutput, Key: s.key, Workspace: s.workspace, Retry: retry})
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
	if err != nil || (saved.Workspace != "" && saved.Workspace != expected) {
		return saved, fmt.Errorf("checkpoint workspace escapes its manager")
	}
	return saved, nil
}

func checkpointSession(saved checkpoint) *session {
	return &session{issue: saved.Issue, wf: saved.Workflow, attempt: saved.Attempt, turn: saved.Turn, thread: saved.Thread, lastGate: saved.LastGate, gateOutput: saved.GateOutput, key: saved.Key, workspace: saved.Workspace}
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
	l.recoveryScan = true
	l.recoverPending(ctx)
	return errors.Join(failures...)
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
		saved := checkpoint{Version: 1, Phase: checkpointLegacy, Issue: issue, Workflow: l.Loader.Current(), Key: issue.ID}
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

func (l *Loop) recoverPending(ctx context.Context) {
	ctx, cancel := l.controlContext(ctx)
	defer cancel()
	if l.recoveryScan {
		if err := l.discoverLegacy(ctx); err != nil {
			l.Log.Warn("legacy claim inventory unavailable; retrying next tick", "error", err)
		}
	}
	for id, saved := range l.recovery {
		if ctx.Err() != nil {
			return
		}
		if saved.Phase == checkpointDeferred && saved.Retry != nil && saved.Retry.At.After(time.Now()) {
			continue
		}
		if l.sessions[id] != nil {
			continue
		}
		resumed, err := l.resumeCheckpoint(ctx, saved)
		if err != nil {
			if saved.Error != err.Error() {
				saved.Error = err.Error()
				l.recovery[id] = saved
				if saved.Phase != checkpointLegacy {
					_ = l.writeCheckpoint(saved)
				}
				l.Log.Error("session recovery blocked; preserving claim and workspace", "issue", id, "error", err)
			}
			continue
		}
		if resumed {
			delete(l.recovery, id)
		}
	}
}

func (l *Loop) resumeCheckpoint(ctx context.Context, saved checkpoint) (bool, error) {
	issues, err := l.Tracker.Dispatchable(ctx, tracker.DispatchQuery{IDs: []string{saved.Issue.ID}})
	if err != nil {
		return false, err
	}
	if len(issues) == 0 {
		// Closed issues are omitted from bd's default list view.
		issues, err = l.Tracker.Dispatchable(ctx, tracker.DispatchQuery{IDs: []string{saved.Issue.ID}, Statuses: []string{tracker.StatusClosed}})
		if err != nil {
			return false, err
		}
	}
	if len(issues) == 0 {
		l.removeCheckpoint(saved.Issue.ID)
		return true, nil
	}
	issue := issues[0]
	if issue.Assignee != l.recoveryActor {
		// Relinquish foreign continuation state without touching its workspace or agent.
		if issue.Assignee != "" {
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
		return false, fmt.Errorf("no durable continuation state; inspect any old agent before releasing its stale claim")
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
