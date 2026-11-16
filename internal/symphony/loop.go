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
	"syscall"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/andrew-bierman/lambe/internal/logging"
	"github.com/andrew-bierman/lambe/internal/proc"
	"github.com/andrew-bierman/lambe/internal/tracker"
)

// ErrRestartFromFullPrompt requests a full task prompt without consuming an attempt.
var ErrRestartFromFullPrompt = errors.New("saved conversation unavailable; restart with full task prompt")

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

// CompletionFinalizer finishes durable bookkeeping after a gate closed an issue
// just before the supervisor stopped. It must be safe to call repeatedly.
type CompletionFinalizer interface {
	FinalizeCompletion(ctx context.Context, issueID string) error
}

// TurnRequest is one turn of an attempt.
type TurnRequest struct {
	// RecordThread saves the backend conversation and originating harness before prompting.
	RecordThread  func(string, string) error `json:"-"`
	SessionID     string
	ThreadID      string
	ThreadHarness string
	Issue         tracker.Issue
	Attempt       int
	Turn          int
	Prompt        string
	Workspace     string
	Revision      Revision
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
	ThreadHarness string
	ThreadID      string
	Exit          TurnExit
	Output        string
	Settled       bool
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
		meta, err := toml.DecodeFile(path, &spec)
		if err != nil { // #nosec G304 -- path built from the configured formulas directory
			return nil, fmt.Errorf("decode formula %s: %w", path, err)
		}
		if !meta.IsDefined("trigger") {
			continue
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
// tracker so they enter the same two-read
// dispatch as any other work. Implementations own durable deduplication and re-arming.
type FormulaMaterializer interface {
	Materialize(ctx context.Context, f Formula) error
}

// BeadsMaterializer materializes through the bd CLI in the tracker
// directory. A durable ledger (.beads/lambe-materialized.json) records the
// poured root issue id per formula: bd mol pour is not deduped by bd itself
// (each invocation materializes a fresh issue set — issue #6), so the
// materializer skips a pour while the recorded root mol is still open and
// re-allowed once it closes. Re-arming is rate-limited by RefireInterval:
// a root that just closed (for example a close-race limbo the next tick
// misreads) must not rapid-fire fresh pours while upstream is still
// broken. Protos are ephemeral by default; both cook and pour support
// --dry-run, which callers can enable for a rehearsal pass.
type BeadsMaterializer struct {
	Bin    string
	Dir    string
	DryRun bool
	Log    *slog.Logger
	// RefireInterval is the minimum time between pours of the same
	// formula after its recorded root closes. Non-positive values use
	// DefaultRefireInterval.
	RefireInterval time.Duration
}

// DefaultRefireInterval is the default pour re-arm delay: three ticks of
// the default 10-second loop cadence, long enough for reconcile to
// settle a close-race limbo before the same formula pours again.
const DefaultRefireInterval = 30 * time.Second

// materializedEntry is one ledger record: the poured root mol and when.
type materializedEntry struct {
	Pending  bool   `json:"pending,omitempty"`
	RootID   string `json:"root_id"`
	PouredAt string `json:"poured_at"`
}

func (m *BeadsMaterializer) ledgerPath() string {
	return filepath.Join(m.Dir, ".beads", "lambe-materialized.json")
}

func (m *BeadsMaterializer) loadLedger() (map[string]materializedEntry, error) {
	out := map[string]materializedEntry{}
	raw, err := os.ReadFile(m.ledgerPath()) // #nosec G304 -- operator configured tracker state
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	if out == nil {
		return nil, errors.New("materialization ledger is null")
	}
	return out, nil
}

func (m *BeadsMaterializer) saveLedger(entries map[string]materializedEntry) error {
	raw, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	path := m.ledgerPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".materialized-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(file.Name()) }()
	if _, err = file.Write(raw); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}

// pouredRootOpen reports the ledger's root issue id for name and whether it
// is still live (not closed). A tracker read failure counts as live: a
// flaky read must never cause a duplicate pour, and retries converge.
func (m *BeadsMaterializer) pouredRootOpen(ctx context.Context, entry materializedEntry) (string, bool, error) {
	if entry.RootID == "" {
		return "", false, nil
	}
	transport := tracker.NewShellTransport(m.Bin, m.Dir, "")
	result, err := transport.Show(ctx, entry.RootID)
	if err != nil {
		var exit *tracker.ExitError
		if errors.As(err, &exit) {
			for _, out := range []string{exit.Stdout, exit.Stderr} {
				var failure struct {
					Error  string `json:"error"`
					Schema int    `json:"schema_version"`
				}
				if json.Unmarshal([]byte(out), &failure) == nil && failure.Schema == 1 && failure.Error == "no issues found matching the provided IDs" {
					return entry.RootID, false, nil
				}
			}
		}
		return entry.RootID, true, fmt.Errorf("read materialized root %s: %w", entry.RootID, err)
	}
	res := string(result.Data)
	var payload struct {
		Status string `json:"status"`
	}
	if strings.HasPrefix(strings.TrimSpace(res), "[") {
		var rows []struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal([]byte(res), &rows); err != nil {
			return entry.RootID, true, err
		}
		if len(rows) == 0 {
			return entry.RootID, false, nil
		}
		if len(rows) != 1 {
			return entry.RootID, true, fmt.Errorf("materialized root lookup returned %d rows", len(rows))
		}
		payload.Status = rows[0].Status
	} else if err := json.Unmarshal([]byte(res), &payload); err != nil {
		return entry.RootID, true, err
	}
	if payload.Status == "" {
		return entry.RootID, true, errors.New("materialized root lookup omitted status")
	}
	return entry.RootID, payload.Status != "closed", nil
}

// run executes the bd CLI in the tracker directory. The first argv element
// is the bd subcommand; the binary path is prepended here so callers pass
// logical arguments only.
func (m *BeadsMaterializer) run(ctx context.Context, argv ...string) (string, error) {
	return proc.RunCommand(ctx, m.Dir, append([]string{m.Bin}, argv...)...)
}

// refireInterval resolves the configured or default re-arm delay.
func (m *BeadsMaterializer) refireInterval() time.Duration {
	if m.RefireInterval > 0 {
		return m.RefireInterval
	}
	return DefaultRefireInterval
}

// withinRefireWindow reports whether the formula's last pour happened
// less than the re-fire interval ago, so a root that just closed cannot
// trigger an immediate re-pour.
func (m *BeadsMaterializer) withinRefireWindow(entry materializedEntry) bool {
	pouredAt, err := time.Parse(time.RFC3339, entry.PouredAt)
	if err != nil {
		return false
	}
	return time.Since(pouredAt) < m.refireInterval()
}

// Materialize implements FormulaMaterializer. The formula name doubles
// as the proto id for bd mol pour, which resolves the formula file from
// Dir/.beads/formulas and creates the step issues in one command. bd
// prints "Root issue: <id>" for the poured mol root; that id anchors the
// dedup ledger.
func (m *BeadsMaterializer) Materialize(ctx context.Context, f Formula) error {
	deadline, limited := ctx.Deadline()
	minimumPourBudget := time.Until(deadline) / 2
	enoughTime := func() bool { return !limited || time.Until(deadline) >= minimumPourBudget }
	if m.DryRun {
		// Rehearsal pass: surface the pour command without side effects.
		_, err := m.run(ctx, "mol", "pour", f.Name, "--dry-run")
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	lock, err := m.materializationLock()
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()

	entries, err := m.loadLedger()
	if err != nil {
		return fmt.Errorf("read materialization ledger: %w", err)
	}
	entry, exists := entries[f.Name]
	if exists {
		if !entry.Pending && entry.RootID == "" {
			return fmt.Errorf("invalid materialization root for %s", f.Name)
		}
		if _, err := time.Parse(time.RFC3339, entry.PouredAt); err != nil {
			return fmt.Errorf("invalid materialization timestamp for %s: %w", f.Name, err)
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if entry.Pending {
		return fmt.Errorf("formula %s has an unresolved pour; inspect tracker and reconcile %s before retrying", f.Name, m.ledgerPath())
	}
	if rootID, live, err := m.pouredRootOpen(ctx, entry); err != nil {
		return err
	} else if live {
		m.Log.Info("formula already materialized; root mol still open", "formula", f.Name)
		return nil
	} else if rootID != "" && m.withinRefireWindow(entry) {
		m.Log.Info("formula root closed but the last pour is inside the refire interval; skipping this cycle", "formula", f.Name, "root", rootID)
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !enoughTime() {
		return errors.New("insufficient control time to start a pour; retrying without side effects")
	}
	entries[f.Name] = materializedEntry{Pending: true, PouredAt: time.Now().UTC().Format(time.RFC3339)}
	if err := m.saveLedger(entries); err != nil {
		return fmt.Errorf("record pour intent: %w", err)
	}
	if !enoughTime() {
		if exists {
			entries[f.Name] = entry
		} else {
			delete(entries, f.Name)
		}
		return errors.Join(errors.New("insufficient control time after recording pour intent"), m.saveLedger(entries))
	}
	out, err := m.run(ctx, "mol", "pour", f.Name)
	if err != nil {
		if errors.Is(err, proc.ErrCommandNotStarted) {
			if exists {
				entries[f.Name] = entry
			} else {
				delete(entries, f.Name)
			}
			if saveErr := m.saveLedger(entries); saveErr != nil {
				return errors.Join(err, fmt.Errorf("restore unstarted pour intent: %w", saveErr))
			}
		}
		return fmt.Errorf("bd mol pour %s: %w: %s", f.Name, err, out)
	}
	for _, line := range strings.Split(out, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "Root issue: ")
		if !ok {
			continue
		}
		rootID := strings.Trim(strings.TrimSpace(rest), "`")
		if rootID == "" {
			continue
		}
		entries[f.Name] = materializedEntry{RootID: rootID, PouredAt: time.Now().UTC().Format(time.RFC3339)}
		if err := m.saveLedger(entries); err != nil {
			return err
		}
		m.Log.Info("formula materialized", "formula", f.Name, "root", rootID)
		return nil
	}
	return fmt.Errorf("pour returned no root issue; inspect tracker and reconcile %s", m.ledgerPath())
}

// session is one supervised issue: the pinned workflow revision, the
// attempt counter, the live thread, and the workspace key.
type session struct {
	issue         tracker.Issue
	wf            *Workflow
	attempt       int
	turn          int
	thread        string
	threadHarness string
	lastGate      string
	key           string
	workspace     string
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
	// Observe receives progress from concurrent workers; it must be concurrency-safe.
	Observe func(Progress)

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
	// CheckpointDir enables private durable continuation records for this instance.
	CheckpointDir string
	recoveryScan  bool
	recoveryActor string
	recovery      map[string]checkpoint
	// FailureDelay overrides the failure backoff computation; the default
	// wraps FailureBackoff with RetryCap.
	FailureDelay func(attempt int) time.Duration
	// ContinuationDelay overrides the fixed continuation retry delay.
	ContinuationDelay time.Duration
	Log               *slog.Logger

	sessions map[string]*session
	retries  *Queue
	active   map[string]*worker
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
		recovery:          map[string]checkpoint{},
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
	if err := ctx.Err(); err != nil {
		return err
	}
	l.collect(ctx)
	l.recoverPending(ctx)
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
		// The materializer owns durable deduplication and re-arming.
		if err := l.runFormula(ctx, f); err != nil {
			l.Log.Error("formula run failed", "formula", f.Name, "error", err)
			continue
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

// runFormula evaluates one fired responsibility under the loop's control
// timeout and materializes its finite issues into the dispatch front.
func (l *Loop) runFormula(ctx context.Context, f Formula) error {
	triggerCtx, cancel := l.controlContext(ctx)
	fired, err := TriggerFired(triggerCtx, f)
	cancel()
	if err != nil {
		return fmt.Errorf("formula trigger errored: %w", err)
	}
	if !fired {
		return nil
	}
	materializeCtx, cancel := l.controlContext(ctx)
	defer cancel()
	if err := l.Materializer.Materialize(materializeCtx, f); err != nil {
		return fmt.Errorf("formula materialization failed: %w", err)
	}
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
		// Record creation and its blocked-state update are separate tracker writes.
		if issue.Metadata[tracker.MetaKind] != "" {
			continue
		}
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
		if _, recovering := l.recovery[issue.ID]; recovering {
			continue
		}
		wf := l.Loader.Current()
		s := &session{issue: issue, wf: wf, key: issue.ID}
		kind := RetryFailure
		if l.CheckpointDir != "" {
			saved, readErr := l.readCheckpoint(l.checkpointPath(issue.ID))
			if readErr == nil {
				s = checkpointSession(saved)
				kind = RetryContinuation
				if saved.Retry != nil {
					if saved.Retry.At.After(time.Now()) {
						continue
					}
					kind = saved.Retry.Kind
				}
				if saved.Attempt == 0 {
					kind = RetryFailure
				}
			} else if !os.IsNotExist(readErr) {
				l.Log.Error("cannot read continuation; refusing fresh attempt", "issue", issue.ID, "error", readErr)
				continue
			}
		}
		if err := l.saveCheckpoint(s, checkpointClaiming, &Retry{IssueID: issue.ID, Kind: kind, At: time.Now()}); err != nil {
			l.Log.Error("cannot persist prospective claim", "issue", issue.ID, "error", err)
			continue
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
		s.issue = claimed
		if l.CheckpointDir != "" {
			if err := l.stopRecordedAgent(ctx, claimed.ID); err != nil {
				l.recovery[claimed.ID] = checkpoint{Version: 1, Phase: checkpointClaiming, Issue: claimed, Workflow: s.wf, Attempt: s.attempt, Turn: s.turn, Thread: s.thread, ThreadHarness: s.threadHarness, Key: s.key, Workspace: s.workspace, Retry: &Retry{IssueID: claimed.ID, Kind: kind, At: time.Now()}}
				l.Log.Error("claim recovery blocked", "issue", claimed.ID, "error", err)
				continue
			}
		}
		l.sessions[claimed.ID] = s
		l.Log.Info("issue claimed", "issue", claimed.ID, "revision", wf.Revision)
		l.start(ctx, s, kind)
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
		// Retire the previous process before its binding ceases to be the latest attempt.
		if s.attempt > 0 {
			l.closeRunnerSession(ctx, s.issue.ID)
			if l.CheckpointDir != "" {
				if err := l.stopRecordedAgent(ctx, s.issue.ID); err != nil {
					log.Error("previous attempt not safely stopped", "error", err)
					w.schedule(s, RetryFailure, l.failureDelay(s.attempt))
					return
				}
			}
		}
		s.attempt++
		s.turn = 0
	}
	wf := s.wf
	if err := l.saveCheckpoint(s, checkpointPreparing, &Retry{IssueID: s.issue.ID, Kind: RetryContinuation, At: time.Now()}); err != nil {
		log.Error("cannot persist attempt before hooks", "error", err)
		w.schedule(s, RetryContinuation, l.ContinuationDelay)
		return
	}

	ws, err := l.Spaces.Ensure(ctx, s.key, wf.Hooks)
	if err != nil {
		log.Error("workspace creation failed, retrying later", "error", err)
		if ctx.Err() != nil {
			w.schedule(s, RetryContinuation, l.ContinuationDelay)
		} else {
			w.schedule(s, RetryFailure, l.failureDelay(s.attempt))
		}
		return
	}
	s.workspace = ws.Dir

	if err := l.Spaces.RunBeforeRun(ctx, ws.Dir, wf.Hooks); err != nil {
		log.Error("before_run failed, attempt aborted", "error", err)
		l.Spaces.RunAfterRun(ctx, ws.Dir, wf.Hooks)
		if ctx.Err() != nil {
			w.schedule(s, RetryContinuation, l.ContinuationDelay)
		} else {
			w.schedule(s, RetryFailure, l.failureDelay(s.attempt))
		}
		return
	}

	if err := l.Tracker.PutAttempt(ctx, tracker.Attempt{
		SessionID: s.issue.ID,
		Number:    s.attempt,
	}); err != nil {
		log.Error("record attempt failed; not prompting agent", "error", err)
		w.schedule(s, RetryContinuation, l.ContinuationDelay)
		return
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
	if err := l.saveCheckpoint(s, checkpointRunning, nil); err != nil {
		log.Error("checkpoint failed; not prompting agent", "error", err)
		s.turn--
		w.schedule(s, RetryContinuation, l.ContinuationDelay)
		return true
	}
	res, err := l.Runner.RunTurn(ctx, TurnRequest{
		RecordThread: func(thread, backend string) error {
			previous, previousBackend := s.thread, s.threadHarness
			s.thread, s.threadHarness = thread, backend
			if err := l.saveCheckpoint(s, checkpointRunning, nil); err != nil {
				s.thread, s.threadHarness = previous, previousBackend
				return err
			}
			return nil
		},
		SessionID:     s.issue.ID,
		ThreadID:      s.thread,
		ThreadHarness: s.threadHarness,
		Issue:         s.issue,
		Attempt:       s.attempt,
		Turn:          s.turn,
		Prompt:        prompt,
		Workspace:     ws.Dir,
		Revision:      wf.Revision,
	})
	w.interrupted = ctx.Err() != nil
	if err == nil && w.interrupted {
		err = ctx.Err()
	}
	if res.ThreadID != "" {
		s.thread, s.threadHarness = res.ThreadID, res.ThreadHarness
	}
	l.Spaces.RunAfterRun(ctx, ws.Dir, wf.Hooks)

	if errors.Is(err, ErrRestartFromFullPrompt) {
		l.closeRunnerSession(ctx, s.issue.ID)
		s.turn = 0
		w.interrupted = false
		w.schedule(s, RetryContinuation, l.ContinuationDelay)
		return true
	}

	if errors.Is(err, proc.ErrBindingUnavailable) {
		// The child never received a prompt; preserve the attempt and replay this turn.
		s.turn--
		w.interrupted = false
		w.schedule(s, RetryContinuation, l.ContinuationDelay)
		return true
	}

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
		if s.turn > 0 {
			s.turn--
		}
		if err := l.saveCheckpoint(s, checkpointDeferred, &Retry{IssueID: s.issue.ID, Kind: RetryContinuation, At: until}); err != nil {
			log.Error("persist deferred continuation", "error", err)
		}
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
	l.removeCheckpoint(s.issue.ID)
	delete(l.sessions, s.issue.ID)
}

func (l *Loop) cleanup(ctx context.Context, s *session, reason string) {
	l.progress(s, "stopped: "+reason, nil)
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
	phase := checkpointRetry
	if w.interrupted && kind != RetryGate {
		phase = checkpointInterrupted
	}
	if err := w.loop.saveCheckpoint(s, phase, w.retry); err != nil {
		w.loop.Log.Error("persist pending retry", "issue", s.issue.ID, "error", err)
	}
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
	s.gateOutput = output
	if err := l.saveCheckpoint(s, checkpointGate, &Retry{IssueID: s.issue.ID, Kind: RetryGate, At: time.Now()}); err != nil {
		log.Error("checkpoint failed; not evaluating gate", "error", err)
		w.schedule(s, RetryGate, l.ContinuationDelay)
		return true
	}
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

func (m *BeadsMaterializer) materializationLock() (*os.File, error) {
	path := m.ledgerPath() + ".lock"
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) // #nosec G304 -- configured private materialization ledger lock
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("another process is materializing responsibilities: %w", err)
	}
	return file, nil
}
