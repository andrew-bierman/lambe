package symphony

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/andrew-bierman/lambe/internal/tracker"
)

// AgentRunner runs one agent turn in a workspace. Implementations own the
// agent process; the loop only sequences turns, prompts, and gates.
type AgentRunner interface {
	// RunTurn sends one prompt. A non-empty ThreadID continues the same
	// live thread; runners that support threading return their thread id.
	RunTurn(ctx context.Context, req TurnRequest) (TurnResult, error)
}

// TurnRequest is one turn of an attempt.
type TurnRequest struct {
	SessionID string
	ThreadID  string
	Issue     tracker.Issue
	Attempt   int
	Turn      int
	Prompt    string
	Workspace string
	Revision  Revision
}

// TurnExit classifies how a turn ended.
type TurnExit int

// Turn exit classifications.
const (
	TurnSuccess TurnExit = iota
	TurnFailure
	TurnTimeout
	TurnCanceled
	// TurnDeferred ends the attempt without consuming it: the runner
	// detected a condition (for example a suspected provider limit) whose
	// remedy is to park the issue until WakeAt and let the dispatch front
	// wake it, rather than burn retry attempts.
	TurnDeferred
)

// TurnResult reports the outcome of one turn. Settled carries the runner's
// completion signal (the agent declares the work finished); only settled
// turns are gated.
type TurnResult struct {
	ThreadID string
	Exit     TurnExit
	Output   string
	Settled  bool
	// WakeAt is the earliest re-dispatch time for TurnDeferred turns;
	// zero defers to the tracker's own default horizon.
	WakeAt time.Time
}

// GateEvaluator judges a settled turn. Green is the only path to done.
type GateEvaluator interface {
	Evaluate(ctx context.Context, req GateRequest) (GateOutcome, error)
}

// GateRequest is the gate input for one settled turn.
type GateRequest struct {
	Issue     tracker.Issue
	Attempt   int
	Workspace string
	Revision  Revision
	Output    string
}

// GateVerdict is the three-valued gate outcome.
type GateVerdict int

// Gate verdicts: green completes, red retries with the gate output as
// guidance and consumes the attempt, error retries the gate without
// consuming the attempt.
const (
	GateGreen GateVerdict = iota
	GateRed
	GateErr
)

// GateOutcome is one gate decision.
type GateOutcome struct {
	Verdict GateVerdict
	Output  string
}

// Formula is one responsibility definition read from a *.formula.toml file
// in the tracker's .beads/formulas/ directory.
type Formula struct {
	// Name is the formula file stem.
	Name string
	// Path is the formula file's absolute path.
	Path string
	// Command is the trigger: a shell command whose non-zero exit means
	// the trigger fired (for example a CI-status check).
	Command string
}

// formulaTOML mirrors the trigger table of a formula file.
type formulaTOML struct {
	Trigger struct {
		Command string `toml:"command"`
	} `toml:"trigger"`
}

// LoadFormulas reads every *.formula.toml under dir. An empty or missing
// directory yields no formulas.
func LoadFormulas(dir string) ([]Formula, error) {
	entries, err := os.ReadDir(dir) // #nosec G304 -- path is operator configuration
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read formulas: %w", err)
	}
	var formulas []Formula
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".formula.toml") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		var spec formulaTOML
		if _, err := toml.DecodeFile(path, &spec); err != nil { // #nosec G304 -- path built from the configured formulas directory
			return nil, fmt.Errorf("decode formula %s: %w", path, err)
		}
		if spec.Trigger.Command == "" {
			return nil, fmt.Errorf("formula %s: trigger.command is required", path)
		}
		formulas = append(formulas, Formula{
			Name:    strings.TrimSuffix(e.Name(), ".formula.toml"),
			Path:    path,
			Command: spec.Trigger.Command,
		})
	}
	return formulas, nil
}

// TriggerFired evaluates a formula trigger: the command runs under sh -lc
// with the formula's directory as cwd, and a non-zero exit means fired.
func TriggerFired(ctx context.Context, f Formula) (bool, error) {
	cmd := exec.CommandContext(ctx, "sh", "-lc", f.Command) // #nosec G204 -- trigger commands are operator-authored formula configuration
	cmd.Dir = filepath.Dir(f.Path)
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return true, nil
		}
		return false, fmt.Errorf("run trigger for %s: %w", f.Name, err)
	}
	return false, nil
}

// FormulaMaterializer materializes a formula's finite issues into the
// tracker (bd cook followed by bd mol pour) so they enter the same two-read
// dispatch as any other work.
type FormulaMaterializer interface {
	Materialize(ctx context.Context, f Formula) error
}

// BeadsMaterializer materializes through the bd CLI in the tracker
// directory. Protos are ephemeral by default; both cook and pour support
// --dry-run, which callers can enable for a rehearsal pass.
type BeadsMaterializer struct {
	Bin    string
	Dir    string
	DryRun bool
	Log    *slog.Logger
}

// Materialize implements FormulaMaterializer.
func (m *BeadsMaterializer) Materialize(ctx context.Context, f Formula) error {
	cook := []string{"cook", f.Name}
	pour := []string{"mol", "pour"}
	if m.DryRun {
		cook = append(cook, "--dry-run")
		pour = append(pour, "--dry-run")
	}
	for _, args := range [][]string{cook, pour} {
		cmd := exec.CommandContext(ctx, m.Bin, args...) // #nosec G204 -- fixed argv over a configured binary
		cmd.Dir = m.Dir
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("bd %s %s: %w: %s", args[0], f.Name, err, string(out))
		}
	}
	return nil
}

// session is one supervised issue: the pinned workflow revision, the
// attempt counter, the live thread, and the workspace key.
type session struct {
	issue    tracker.Issue
	wf       *Workflow
	attempt  int
	turn     int
	thread   string
	lastGate string
	key      string
}

// Loop is the tick orchestrator: formula triggers, the two-read dispatch,
// claims, workspaces, the turn loop, gates, retries, and reconciliation.
// It is single-threaded per Tick; concurrent Ticks are the caller's to
// serialize.
type Loop struct {
	Tracker          tracker.Tracker
	Loader           *Loader
	Runner           AgentRunner
	Gates            GateEvaluator
	Spaces           *WorkspaceManager
	Formulas         []Formula
	Materializer     FormulaMaterializer
	DispatchStatuses []string
	RetryCap         time.Duration
	// FailureDelay overrides the failure backoff computation; the default
	// wraps FailureBackoff with RetryCap.
	FailureDelay func(attempt int) time.Duration
	// ContinuationDelay overrides the fixed continuation retry delay.
	ContinuationDelay time.Duration
	Log               *slog.Logger

	sessions map[string]*session
	retries  *Queue
}

// NewLoop assembles the loop with spec defaults: dispatch reads the ready
// front and failure retries cap at 10 minutes.
func NewLoop(tr tracker.Tracker, loader *Loader, runner AgentRunner, gates GateEvaluator, spaces *WorkspaceManager, log *slog.Logger) *Loop {
	if log == nil {
		log = slog.Default()
	}
	return &Loop{
		Tracker:           tr,
		Loader:            loader,
		Runner:            runner,
		Gates:             gates,
		Spaces:            spaces,
		DispatchStatuses:  []string{"ready"},
		RetryCap:          DefaultRetryBackoff,
		ContinuationDelay: ContinuationRetry,
		FailureDelay:      nil,
		Log:               log,
		sessions:          map[string]*session{},
		retries:           &Queue{},
	}
}

// failureDelay resolves the configured or default failure backoff.
func (l *Loop) failureDelay(attempt int) time.Duration {
	if l.FailureDelay != nil {
		return l.FailureDelay(attempt)
	}
	return FailureBackoff(attempt, l.RetryCap)
}

// Tick advances the loop once: evaluate formula triggers and materialize
// fired responsibilities, service due retries, then two-read dispatch and
// run newly claimable issues.
func (l *Loop) Tick(ctx context.Context) error {
	l.runFormulas(ctx)
	if err := l.serviceRetries(ctx); err != nil {
		return err
	}
	return l.dispatch(ctx)
}

// runFormulas evaluates every configured trigger and materializes the
// fired ones so their finite issues flow into this tick's dispatch read.
func (l *Loop) runFormulas(ctx context.Context) {
	for _, f := range l.Formulas {
		if l.Materializer == nil {
			continue
		}
		fired, err := TriggerFired(ctx, f)
		if err != nil {
			l.Log.Warn("formula trigger errored", "formula", f.Name, "error", err)
			continue
		}
		if !fired {
			continue
		}
		if err := l.Materializer.Materialize(ctx, f); err != nil {
			l.Log.Error("formula materialization failed", "formula", f.Name, "error", err)
			continue
		}
		l.Log.Info("formula materialized", "formula", f.Name)
	}
}

// serviceRetries reconciles due retry entries by ID: terminal issues get
// terminal cleanup and release; live ones re-enter the run path.
func (l *Loop) serviceRetries(ctx context.Context) error {
	for _, r := range l.retries.Due(time.Now()) {
		s, ok := l.sessions[r.IssueID]
		if !ok {
			continue
		}
		stop, err := Reconcile(ctx, l.Tracker, []string{r.IssueID})
		if err != nil {
			l.Log.Error("retry reconcile failed", "issue", r.IssueID, "error", err)
			continue
		}
		if len(stop) > 0 {
			l.terminal(ctx, s, "retry found terminal state")
			continue
		}
		if r.Kind == RetryFailure {
			l.runAttempt(ctx, s, true)
			continue
		}
		l.runAttempt(ctx, s, false)
	}
	return nil
}

// dispatch performs the state-list read and starts runs for every issue
// whose claim wins.
func (l *Loop) dispatch(ctx context.Context) error {
	issues, err := l.Tracker.Dispatchable(ctx, tracker.DispatchQuery{Statuses: l.DispatchStatuses})
	if err != nil {
		return fmt.Errorf("dispatch read: %w", err)
	}
	for _, issue := range issues {
		if _, busy := l.sessions[issue.ID]; busy {
			continue
		}
		claimed, err := l.Tracker.Claim(ctx, issue.ID)
		if err != nil {
			if errors.Is(err, tracker.ErrLostRace) || errors.Is(err, tracker.ErrFrozen) {
				continue
			}
			return fmt.Errorf("claim %s: %w", issue.ID, err)
		}
		wf := l.Loader.Current()
		s := &session{issue: claimed, wf: wf, key: claimed.ID, turn: 0, attempt: 0}
		l.sessions[claimed.ID] = s
		l.Log.Info("issue claimed", "issue", claimed.ID, "revision", wf.Revision)
		l.runAttempt(ctx, s, true)
	}
	return nil
}

// runAttempt executes one attempt (a failure retry increments the attempt
// number; a continuation reuses it) as a bounded turn loop: prompt, turn,
// reconcile-or-gate, continuation guidance on the same thread.
func (l *Loop) runAttempt(ctx context.Context, s *session, nextAttempt bool) {
	log := l.Log.With(slog.String("issue", s.issue.ID))
	if nextAttempt {
		s.attempt++
		s.turn = 0
	}
	wf := s.wf

	ws, err := l.Spaces.Ensure(ctx, s.key, wf.Hooks)
	if err != nil {
		// after_create failure is fatal to creation: never invoke the
		// runner, retry creation later under the failure backoff.
		log.Error("workspace creation failed, retrying later", "error", err)
		l.schedule(s, RetryFailure, l.failureDelay(s.attempt))
		return
	}

	if err := l.Spaces.RunBeforeRun(ctx, ws.Dir, wf.Hooks); err != nil {
		log.Error("before_run failed, attempt aborted", "error", err)
		l.Spaces.RunAfterRun(ctx, ws.Dir, wf.Hooks)
		l.schedule(s, RetryFailure, l.failureDelay(s.attempt))
		return
	}

	if err := l.Tracker.PutAttempt(ctx, tracker.Attempt{
		SessionID: s.issue.ID,
		Number:    s.attempt,
	}); err != nil {
		log.Error("record attempt failed", "error", err)
	}

	for s.turn < wf.MaxTurns {
		s.turn++
		if l.runTurn(ctx, s, ws, log) {
			return
		}
	}

	// Turn cap reached without settling: treat as a failed attempt.
	l.schedule(s, RetryFailure, l.failureDelay(s.attempt))
}

// runTurn executes one turn of an attempt and reports whether the attempt
// is finished (terminal transition or retry scheduled). Every completed
// turn finishes the attempt in this design: continuation turns arrive on
// the next tick through the continuation retry, on the same live thread.
func (l *Loop) runTurn(ctx context.Context, s *session, ws *Workspace, log *slog.Logger) bool {
	wf := s.wf
	prompt, err := l.turnPrompt(s)
	if err != nil {
		log.Error("prompt render failed", "error", err)
		l.Spaces.RunAfterRun(ctx, ws.Dir, wf.Hooks)
		l.schedule(s, RetryFailure, l.failureDelay(s.attempt))
		return true
	}
	res, err := l.Runner.RunTurn(ctx, TurnRequest{
		SessionID: s.issue.ID,
		ThreadID:  s.thread,
		Issue:     s.issue,
		Attempt:   s.attempt,
		Turn:      s.turn,
		Prompt:    prompt,
		Workspace: ws.Dir,
		Revision:  wf.Revision,
	})
	if res.ThreadID != "" {
		s.thread = res.ThreadID
	}
	l.Spaces.RunAfterRun(ctx, ws.Dir, wf.Hooks)

	if res.Exit == TurnDeferred {
		until := res.WakeAt
		if until.IsZero() {
			until = time.Now().Add(5 * time.Minute)
		}
		if err := l.Tracker.Defer(ctx, s.issue.ID, until, "runner deferred turn: "+res.Output); err != nil {
			log.Error("defer failed, falling back to failure retry", "error", err)
			l.schedule(s, RetryFailure, l.failureDelay(s.attempt))
			return true
		}
		log.Info("turn deferred, attempt preserved", "turn", s.turn, "wake_at", until.UTC().Format(time.RFC3339))
		delete(l.sessions, s.issue.ID)
		return true
	}

	if err != nil || res.Exit != TurnSuccess {
		log.Info("turn failed, scheduling failure retry", "turn", s.turn, "error", err)
		l.schedule(s, RetryFailure, l.failureDelay(s.attempt))
		return true
	}

	if res.Settled {
		outcome, gerr := l.Gates.Evaluate(ctx, GateRequest{
			Issue:     s.issue,
			Attempt:   s.attempt,
			Workspace: ws.Dir,
			Revision:  wf.Revision,
			Output:    res.Output,
		})
		if gerr != nil || outcome.Verdict == GateErr {
			log.Info("gate error, retrying gate without consuming attempt", "error", gerr)
			l.schedule(s, RetryContinuation, l.ContinuationDelay)
			return true
		}
		if outcome.Verdict == GateRed {
			log.Info("gate red, scheduling failure retry with gate output")
			s.lastGate = outcome.Output
			l.schedule(s, RetryFailure, l.failureDelay(s.attempt))
			return true
		}
		l.terminal(ctx, s, "gate green")
		return true
	}

	// Normal exit without settling: refresh the issue by ID; if it is
	// still active, continuation guidance goes out on the same live
	// thread on the next tick.
	stop, err := Reconcile(ctx, l.Tracker, []string{s.issue.ID})
	if err != nil {
		log.Error("mid-run reconcile failed", "error", err)
		l.schedule(s, RetryContinuation, l.ContinuationDelay)
		return true
	}
	if len(stop) > 0 {
		l.terminal(ctx, s, "issue left active state mid-run")
		return true
	}
	l.schedule(s, RetryContinuation, l.ContinuationDelay)
	return true
}

// turnPrompt renders the first-turn full prompt or the between-turns
// continuation guidance. A prior attempt's gate rejection is written back
// into the next first-turn prompt so the reconstructed context explains the
// rejection.
func (l *Loop) turnPrompt(s *session) (string, error) {
	if s.turn > 1 {
		return ContinuationGuidance(s.issue.Title, s.attempt, s.lastGate)
	}
	prompt, err := RenderPrompt(s.wf.PromptBody, s.issue.Title, s.attempt)
	if err != nil {
		return "", err
	}
	if s.lastGate != "" {
		prompt += "\n\nPrevious attempt was rejected by the gate. Gate output:\n" + s.lastGate
	}
	return prompt, nil
}

// terminal completes a run: terminal-state workspace cleanup, claim
// release, session teardown. Closing the bead itself belongs to the gate
// layer.
func (l *Loop) terminal(ctx context.Context, s *session, reason string) {
	log := l.Log.With(slog.String("issue", s.issue.ID))
	log.Info("run terminal", "reason", reason)
	if err := l.Spaces.Remove(ctx, s.key, s.wf.Hooks); err != nil {
		log.Error("terminal workspace cleanup failed", "error", err)
	}
	if err := l.Tracker.Release(ctx, s.issue.ID); err != nil && !errors.Is(err, tracker.ErrNotOwner) {
		log.Error("release failed", "error", err)
	}
	delete(l.sessions, s.issue.ID)
}

// schedule arms the retry queue for a session's issue.
func (l *Loop) schedule(s *session, kind RetryKind, delay time.Duration) {
	l.retries.Add(Retry{IssueID: s.issue.ID, Kind: kind, At: time.Now().Add(delay)})
}
