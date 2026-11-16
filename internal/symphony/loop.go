package symphony

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/andrew-bierman/lambe/internal/logging"
	"github.com/andrew-bierman/lambe/internal/proc"
	"github.com/andrew-bierman/lambe/internal/tracker"
)

// AgentRunner runs one agent turn in a workspace. Implementations own the
// agent process; the loop only sequences turns, prompts, and gates.
type AgentRunner interface {
	// RunTurn sends one prompt. A non-empty ThreadID continues the same
	// live thread; runners that support threading return their thread id.
	RunTurn(ctx context.Context, req TurnRequest) (TurnResult, error)
}

// SessionCloser is the optional runner teardown seam: runners that hold
// per-session resources (agent processes) implement it so the loop can
// release them when supervision of a session pauses (deferred turn, gate
// retry) or ends (terminal transition). The next turn for the issue
// reconstructs its resources from the session's durable state.
type SessionCloser interface {
	// CloseSession releases every resource the runner holds for the
	// session. It must be a no-op for a session with no live resources
	// and must not block unboundedly.
	CloseSession(ctx context.Context, sessionID string)
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
	// zero substitutes DefaultDeferHorizon.
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
// Exit codes 126 and 127 are infrastructure failures (the command cannot
// be executed or found — a broken PATH, a missing gh), not a fired
// trigger: they return an error so the caller logs the trigger
// unavailable instead of materializing work on every tick.
func TriggerFired(ctx context.Context, f Formula) (bool, error) {
	if _, err := proc.RunCommand(ctx, filepath.Dir(f.Path), "sh", "-lc", f.Command); err != nil {
		if ctx.Err() != nil {
			return false, fmt.Errorf("trigger %s unavailable: %w", f.Name, ctx.Err())
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			switch exitErr.ExitCode() {
			case 126, 127:
				return false, fmt.Errorf("trigger %s exited %d (command not executable or not found): %w", f.Name, exitErr.ExitCode(), err)
			}
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
// directory. A durable ledger (.beads/lambe-materialized.json) records the
// poured root issue id per formula: bd mol pour is not deduped by bd itself
// (each invocation materializes a fresh issue set — issue #6), so the
// materializer skips a pour while the recorded root mol is still open and
// re-allowed once it closes. Protos are ephemeral by default; both cook and
// pour support --dry-run, which callers can enable for a rehearsal pass.
type BeadsMaterializer struct {
	Bin    string
	Dir    string
	DryRun bool
	Log    *slog.Logger
}

// materializedEntry is one ledger record: the poured root mol and when.
type materializedEntry struct {
	RootID   string `json:"root_id"`
	PouredAt string `json:"poured_at"`
}

func (m *BeadsMaterializer) ledgerPath() string {
	return filepath.Join(m.Dir, ".beads", "lambe-materialized.json")
}

func (m *BeadsMaterializer) loadLedger() map[string]materializedEntry {
	out := map[string]materializedEntry{}
	raw, err := os.ReadFile(m.ledgerPath()) // #nosec G304 -- path is derived from the configured tracker dir
	if err != nil {
		return out
	}
	_ = json.Unmarshal(raw, &out)
	return out
}

func (m *BeadsMaterializer) saveLedger(entries map[string]materializedEntry) {
	raw, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return
	}
	path := m.ledgerPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return
	}
	_ = os.WriteFile(path, raw, 0o600) // #nosec G304 -- operator-owned tracker dir
}

// pouredRootOpen reports the ledger's root issue id for name and whether it
// is still live (not closed). A tracker read failure counts as live: a
// flaky read must never cause a duplicate pour, and retries converge.
func (m *BeadsMaterializer) pouredRootOpen(ctx context.Context, name string) (string, bool) {
	entry, ok := m.loadLedger()[name]
	if !ok || entry.RootID == "" {
		return "", false
	}
	res, err := m.run(ctx, "show", entry.RootID, "--json")
	if err != nil {
		return entry.RootID, true
	}
	var payload struct {
		Status string `json:"status"`
	}
	if json.Unmarshal([]byte(res), &payload) != nil {
		return entry.RootID, true
	}
	return entry.RootID, payload.Status != "closed"
}

// run executes the bd CLI in the tracker directory. The first argv element
// is the bd subcommand; the binary path is prepended here so callers pass
// logical arguments only.
func (m *BeadsMaterializer) run(ctx context.Context, argv ...string) (string, error) {
	return proc.RunCommand(ctx, m.Dir, append([]string{m.Bin}, argv...)...)
}

// Materialize implements FormulaMaterializer. The formula name doubles
// as the proto id for bd mol pour, which resolves the formula file from
// Dir/.beads/formulas and creates the step issues in one command. bd
// prints "Root issue: <id>" for the poured mol root; that id anchors the
// dedup ledger.
func (m *BeadsMaterializer) Materialize(ctx context.Context, f Formula) error {
	if m.DryRun {
		// Rehearsal pass: surface the pour command without side effects.
		_, _ = m.run(ctx, "mol", "pour", f.Name, "--dry-run")
		return nil
	}
	if _, live := m.pouredRootOpen(ctx, f.Name); live {
		m.Log.Info("formula already materialized; root mol still open", "formula", f.Name)
		return nil
	}
	out, err := m.run(ctx, "mol", "pour", f.Name)
	if err != nil {
		return fmt.Errorf("bd mol pour %s: %w: %s", f.Name, err, out)
	}
	for _, line := range strings.Split(out, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "Root issue: ")
		if !ok {
			continue
		}
		rootID := strings.Trim(rest, "`")
		entries := m.loadLedger()
		entries[f.Name] = materializedEntry{RootID: rootID, PouredAt: time.Now().UTC().Format(time.RFC3339)}
		m.saveLedger(entries)
		break
	}
	return nil
}

// session is one supervised issue: the pinned workflow revision, the
// attempt counter, the live thread, and the workspace key.
type session struct {
	issue     tracker.Issue
	wf        *Workflow
	attempt   int
	turn      int
	thread    string
	lastGate  string
	key       string
	workspace string
	// gateOutput holds the settled output between a gate error and its
	// gate-only re-evaluation, so the agent is never re-prompted while the
	// gate retries on the same output.
	gateOutput string
}

// Loop is the tick orchestrator: formula triggers, the two-read dispatch,
// claims, workspaces, the turn loop, gates, retries, and reconciliation.
// Tick coordinates independent workers without waiting for agent turns.
// Coordinator operations (Tick, Wait, Close) must be serialized by the caller.
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
	// ControlTimeout bounds each formula trigger and materialization.
	// Non-positive values use the tracker command timeout; agent turns
	// retain their own runner deadlines.
	ControlTimeout time.Duration
	// FailureDelay overrides the failure backoff computation; the default
	// wraps FailureBackoff with RetryCap.
	FailureDelay func(attempt int) time.Duration
	// ContinuationDelay overrides the fixed continuation retry delay.
	ContinuationDelay time.Duration
	Log               *slog.Logger

	sessions map[string]*session
	retries  *Queue
	active   map[string]*worker
	// materialized records formula names already materialized by this
	// process: the durable guard is the materializer's ledger, this map is
	// the fast path so a fired trigger pours at most once per daemon run.
	materialized map[string]bool
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
		DispatchStatuses:  []string{tracker.StatusReady},
		RetryCap:          DefaultRetryBackoff,
		ContinuationDelay: ContinuationRetry,
		FailureDelay:      nil,
		Log:               log,
		sessions:          map[string]*session{},
		active:            map[string]*worker{},
		retries:           &Queue{},
		materialized:      map[string]bool{},
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
	if err := ctx.Err(); err != nil {
		return err
	}
	l.collect(ctx)
	l.reconcileWorkers(ctx)
	l.runFormulas(ctx)
	if err := l.serviceRetries(ctx); err != nil {
		return err
	}
	return l.dispatch(ctx)
}

// runFormulas evaluates every configured trigger and materializes the
// fired ones so their finite issues flow into this tick's dispatch read.
func (l *Loop) runFormulas(ctx context.Context) {
	if len(l.Formulas) == 0 {
		l.Log.Debug("no formulas configured")
		return
	}
	if l.Materializer == nil {
		l.Log.Warn("formulas configured but materializer is nil")
		return
	}
	for _, f := range l.Formulas {
		// One active mol per formula: pours are not deduped by bd (each
		// pour materializes a fresh issue set — issue #6), so a firing
		// trigger would otherwise re-pour on every tick. The gate or a
		// human closes the mol's issues; when that happens the ledger
		// clears and the next firing materializes a fresh cycle.
		if l.materialized[f.Name] {
			l.Log.Debug("formula already materialized", "formula", f.Name)
			continue
		}
		if err := l.runFormula(ctx, f); err != nil {
			l.Log.Error("formula run failed", "formula", f.Name, "error", err)
			continue
		}
		if fired := l.formulaFired(ctx, f); fired {
			l.materialized[f.Name] = true
			l.Log.Info("formula materialized", "formula", f.Name)
		}
	}
}

// controlContext bounds each formula trigger and materialization to the
// loop's ControlTimeout (agent turns carry their own runner deadlines).
func (l *Loop) controlContext(ctx context.Context) (context.Context, context.CancelFunc) {
	bound := l.ControlTimeout
	if bound <= 0 {
		bound = tracker.DefaultCommandTimeout
	}
	return context.WithTimeout(ctx, bound)
}

// formulaFired reports whether f's trigger currently fires. The caller
// dedupes fired formulas via l.materialized (issue #6); unfired triggers
// stay unmarked so a later firing can still materialize.
func (l *Loop) formulaFired(ctx context.Context, f Formula) bool {
	ctx, cancel := l.controlContext(ctx)
	defer cancel()
	fired, err := TriggerFired(ctx, f)
	if err != nil {
		l.Log.Warn("formula trigger errored", "formula", f.Name, "error", err)
		return false
	}
	return fired
}

// runFormula evaluates one fired responsibility under the loop's control
// timeout and materializes its finite issues into the dispatch front.
func (l *Loop) runFormula(ctx context.Context, f Formula) error {
	ctx, cancel := l.controlContext(ctx)
	defer cancel()
	fired, err := TriggerFired(ctx, f)
	if err != nil {
		return fmt.Errorf("formula trigger errored: %w", err)
	}
	l.Log.Debug("formula trigger evaluated", "formula", f.Name, "fired", fired)
	if !fired {
		return nil
	}
	if err := l.Materializer.Materialize(ctx, f); err != nil {
		return fmt.Errorf("formula materialization failed: %w", err)
	}
	l.Log.Info("formula materialized", "formula", f.Name)
	return nil
}

// isTemplateID reports whether the issue id looks like a cook --persist
// template bead ("<formula-name>.<step-id>"), which bd holds read-only.
func (l *Loop) isTemplateID(id string) bool {
	for _, f := range l.Formulas {
		if strings.HasPrefix(id, f.Name+".") {
			return true
		}
	}
	return false
}

// serviceRetries reconciles due retry entries by ID: terminal issues get
// terminal cleanup and release; live ones re-enter the run path.
func (l *Loop) serviceRetries(ctx context.Context) error {
	for _, r := range l.retries.Due(time.Now()) {
		if len(l.active) >= l.capacity() || l.active[r.IssueID] != nil {
			l.retries.Add(r)
			continue
		}
		s, ok := l.sessions[r.IssueID]
		if !ok {
			continue
		}
		stop, err := Reconcile(ctx, l.Tracker, []string{r.IssueID})
		if err != nil {
			l.Log.Error("retry reconcile failed", "issue", r.IssueID, "error", err)
			l.retries.Add(r)
			continue
		}
		if len(stop) > 0 {
			l.terminal(ctx, s, "retry found terminal state")
			continue
		}
		if err := l.Tracker.Heartbeat(ctx, r.IssueID); err != nil {
			if errors.Is(err, tracker.ErrNotOwner) {
				l.Log.Info("retry claim lost, standing down", "issue", r.IssueID)
				l.closeRunnerSession(ctx, r.IssueID)
				delete(l.sessions, r.IssueID)
			} else {
				l.Log.Error("retry ownership check failed", "issue", r.IssueID, "error", err)
				l.retries.Add(r)
			}
			continue
		}
		if r.Kind == RetryGate {
			logging.WithIssue(l.Log, s.issue.ID).Info("gate retry, re-evaluating on settled output")
			l.start(ctx, s, RetryGate)
			continue
		}
		l.start(ctx, s, r.Kind)
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
		// Template beads from `bd cook --persist` (ids "<formula>.<step>")
		// are read-only specifications; pours create tracker-mol-* work
		// items (issue #7). Skip them before the claim attempt: bd rejects
		// the claim and the error would repeat every tick.
		if l.isTemplateID(issue.ID) {
			logging.WithIssue(l.Log, issue.ID).Debug("dispatch skip: formula template bead")
			continue
		}
		if _, busy := l.sessions[issue.ID]; busy {
			continue
		}
		if len(l.active) >= l.capacity() {
			break
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		claimed, err := l.Tracker.Claim(ctx, issue.ID)
		if err != nil {
			if errors.Is(err, tracker.ErrLostRace) || errors.Is(err, tracker.ErrFrozen) {
				continue
			}
			// One poisoned issue must not starve the rest of the
			// dispatch front: log it and move on. Only a failure of
			// the Dispatchable read itself aborts the tick.
			logging.WithIssue(l.Log, issue.ID).Error("claim failed, continuing with remaining issues", "category", "dispatch", "error", err)
			continue
		}
		wf := l.Loader.Current()
		s := &session{issue: claimed, wf: wf, key: claimed.ID, turn: 0, attempt: 0}
		l.sessions[claimed.ID] = s
		l.Log.Info("issue claimed", "issue", claimed.ID, "revision", wf.Revision)
		l.start(ctx, s, RetryFailure)
	}
	return nil
}

// runAttempt executes one attempt (a failure retry increments the attempt
// number; a continuation reuses it) as a bounded turn loop: prompt, turn,
// reconcile-or-gate, continuation guidance on the same thread.
func (w *worker) runAttempt(ctx context.Context, s *session, nextAttempt bool) {
	l := w.loop
	log := logging.WithIssue(l.Log, s.issue.ID)
	if nextAttempt {
		s.attempt++
		s.turn = 0
	}
	wf := s.wf

	ws, err := l.Spaces.Ensure(ctx, s.key, wf.Hooks)
	if err != nil {
		log.Error("workspace creation failed, retrying later", "error", err)
		w.schedule(s, RetryFailure, l.failureDelay(s.attempt))
		return
	}
	s.workspace = ws.Dir

	if err := l.Spaces.RunBeforeRun(ctx, ws.Dir, wf.Hooks); err != nil {
		log.Error("before_run failed, attempt aborted", "error", err)
		l.Spaces.RunAfterRun(ctx, ws.Dir, wf.Hooks)
		w.schedule(s, RetryFailure, l.failureDelay(s.attempt))
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
		if w.runTurn(ctx, s, ws, log) {
			return
		}
	}

	w.schedule(s, RetryFailure, l.failureDelay(s.attempt))
}

// runTurn executes one turn of an attempt and reports whether the attempt
// is finished (terminal transition or retry scheduled). Every completed
// turn finishes the attempt in this design: continuation turns arrive on
// the next tick through the continuation retry, on the same live thread.
func (w *worker) runTurn(ctx context.Context, s *session, ws *Workspace, log *slog.Logger) bool {
	l := w.loop
	wf := s.wf
	prompt, err := l.turnPrompt(s)
	if err != nil {
		log.Error("prompt render failed", "error", err)
		l.Spaces.RunAfterRun(ctx, ws.Dir, wf.Hooks)
		w.schedule(s, RetryFailure, l.failureDelay(s.attempt))
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
	if err == nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	if res.ThreadID != "" {
		s.thread = res.ThreadID
	}
	l.Spaces.RunAfterRun(ctx, ws.Dir, wf.Hooks)

	if res.Exit == TurnDeferred {
		until := res.WakeAt
		if until.IsZero() {
			until = time.Now().Add(DefaultDeferHorizon)
		}
		if err := l.Tracker.Defer(ctx, s.issue.ID, until, "runner deferred turn: "+res.Output); err != nil {
			log.Error("defer failed, falling back to failure retry", "error", err)
			w.schedule(s, RetryFailure, l.failureDelay(s.attempt))
			return true
		}
		log.Info("turn deferred, attempt preserved", "turn", s.turn, "wake_at", until.UTC().Format(time.RFC3339))
		l.closeRunnerSession(ctx, s.issue.ID)
		w.finished = true
		return true
	}

	if err != nil || res.Exit != TurnSuccess {
		log.Info("turn failed, scheduling failure retry", "turn", s.turn, "error", err)
		w.schedule(s, RetryFailure, l.failureDelay(s.attempt))
		return true
	}

	if res.Settled {
		return w.evaluateGate(ctx, s, res.Output, log)
	}

	stop, err := Reconcile(ctx, l.Tracker, []string{s.issue.ID})
	if err != nil {
		log.Error("mid-run reconcile failed", "error", err)
		w.schedule(s, RetryContinuation, l.ContinuationDelay)
		return true
	}
	if len(stop) > 0 {
		w.terminal(ctx, s, "issue left active state mid-run")
		return true
	}
	w.schedule(s, RetryContinuation, l.ContinuationDelay)
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
	l.cleanup(ctx, s, reason)
	delete(l.sessions, s.issue.ID)
}

func (l *Loop) cleanup(ctx context.Context, s *session, reason string) {
	log := logging.WithIssue(l.Log, s.issue.ID)
	log.Info("run terminal", "reason", reason)
	l.closeRunnerSession(ctx, s.issue.ID)
	if err := l.Spaces.Remove(ctx, s.key, s.wf.Hooks); err != nil {
		log.Error("terminal workspace cleanup failed", "error", err)
	}
	if err := l.Tracker.Release(ctx, s.issue.ID); err != nil && !errors.Is(err, tracker.ErrNotOwner) {
		log.Error("release failed", "error", err)
	}
}

// schedule arms the retry queue for a session's issue.
func (w *worker) schedule(s *session, kind RetryKind, delay time.Duration) {
	w.retry = &Retry{IssueID: s.issue.ID, Kind: kind, At: time.Now().Add(delay)}
}

// closeRunnerSession releases the runner's per-session resources (agent
// processes) when the runner implements the optional SessionCloser seam.
// It is a no-op for runners that hold nothing per session.
// evaluateGate judges a settled attempt's output and routes the verdict:
// green terminates, red retries with the gate output as guidance, and a
// gate error schedules a gate-only re-evaluation on the SAME settled
// output — the agent is never re-prompted for a gate that errored, and
// the attempt is not consumed.
func (w *worker) evaluateGate(ctx context.Context, s *session, output string, log *slog.Logger) bool {
	// Closing the issue precedes tombstone writes and cleanup; a poll must let them finish.
	w.settling.Store(true)
	l := w.loop
	outcome, gerr := l.Gates.Evaluate(ctx, GateRequest{
		Issue:     s.issue,
		Attempt:   s.attempt,
		Workspace: s.workspace,
		Revision:  s.wf.Revision,
		Output:    output,
	})
	if gerr != nil || outcome.Verdict == GateErr {
		log.Info("gate error, retrying gate without consuming attempt", "error", gerr)
		l.closeRunnerSession(ctx, s.issue.ID)
		s.gateOutput = output
		w.schedule(s, RetryGate, l.ContinuationDelay)
		return true
	}
	if outcome.Verdict == GateRed {
		log.Info("gate red, scheduling failure retry with gate output")
		s.lastGate = outcome.Output
		w.schedule(s, RetryFailure, l.failureDelay(s.attempt))
		return true
	}
	w.terminal(ctx, s, "gate green")
	return true
}

func (l *Loop) closeRunnerSession(ctx context.Context, issueID string) {
	if closer, ok := l.Runner.(SessionCloser); ok {
		closer.CloseSession(ctx, issueID)
	}
}
