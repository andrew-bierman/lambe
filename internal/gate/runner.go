package gate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"sync"
	"time"

	"github.com/andrew-bierman/lambe/internal/proc"
	"github.com/andrew-bierman/lambe/internal/symphony"
	"github.com/andrew-bierman/lambe/internal/tracker"
)

// ExitFailure reports a gate command that ran and exited non-zero. It is
// the red signal; every other execution error is gate-infra failure.
type ExitFailure struct {
	Code   int
	Output string
}

// Error implements error.
func (e *ExitFailure) Error() string {
	return fmt.Sprintf("exit %d: %s", e.Code, e.Output)
}

// ExecFunc runs argv in dir and returns combined output. A non-zero exit
// must be reported as *ExitFailure; any other error means the command
// could not run at all (missing binary, PATH broken) and is treated as
// gate-infra failure. It is a field so tests can substitute fakes.
type ExecFunc func(ctx context.Context, dir string, argv ...string) (string, error)

// OsExec is the process-level ExecFunc.
func OsExec(ctx context.Context, dir string, argv ...string) (string, error) {
	out, err := proc.RunCommand(ctx, dir, argv...)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return out, &ExitFailure{Code: exitErr.ExitCode(), Output: out}
		}
		return out, fmt.Errorf("run %s: %w", argv[0], err)
	}
	return out, nil
}

// GHResult is the GitHub-side condition of a gh gate.
type GHResult int

// The GitHub gate conditions.
const (
	// GHPending means the run or PR has not reached a verdict yet.
	GHPending GHResult = iota
	// GHPass means the run succeeded or the PR merged.
	GHPass
	// GHFail means the run failed or was canceled, or the PR closed.
	GHFail
)

// GHChecker resolves a gh:run or gh:pr gate's condition. The default
// implementation shells to the gh CLI; the seam exists so bd's own gate
// machinery (bd gate check) can be wired in when a future tracker
// build advertises it, and for tests.
type GHChecker interface {
	// CheckRun reports the workflow-run condition of one immutable ref
	// (full commit SHA or run id).
	CheckRun(ctx context.Context, ref string) (GHResult, error)
	// CheckPR reports the pull-request condition of pr (number, url, or
	// branch).
	CheckPR(ctx context.Context, pr string) (GHResult, error)
	// ResolvePRHead returns the current head commit SHA of pr (number,
	// url, or branch), so a gh:run gate can judge the attempt's PR head
	// instead of a static branch ref.
	ResolvePRHead(ctx context.Context, pr string) (string, error)
}

// GhCLI implements GHChecker with the gh CLI. Queries evaluate CI status
// on the PR head, never local test output.
type GhCLI struct {
	// Bin is the gh binary path; empty means PATH lookup.
	Bin string
	// Exec is the execution seam; nil uses OsExec.
	Exec ExecFunc
}

// ghRunJSON mirrors gh run view --json output.
type ghRunJSON struct {
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

// ghPrJSON mirrors gh pr view --json output.
type ghPrJSON struct {
	State string `json:"state"`
}

// ghPrHeadJSON mirrors gh pr view --json headRefOid output.
type ghPrHeadJSON struct {
	HeadRefOid string `json:"headRefOid"`
}

func (g *GhCLI) exec(ctx context.Context, argv ...string) (string, error) {
	fn := g.Exec
	if fn == nil {
		fn = OsExec
	}
	return fn(ctx, "", argv...)
}

// CheckRun implements GHChecker: gh run view <ref> --json status,conclusion.
// A completed run with conclusion success passes; failure, cancellation, or
// a startup failure is red; anything else is pending. Execution errors
// (missing gh, unreachable API) are infra failures.
func (g *GhCLI) CheckRun(ctx context.Context, ref string) (GHResult, error) {
	out, err := g.exec(ctx, g.ghBin(), "run", "view", ref, "--json", "status,conclusion")
	if err != nil {
		return GHPending, fmt.Errorf("gh run view %s: %w", ref, err)
	}
	var run ghRunJSON
	if err := unmarshalStrict(out, &run); err != nil {
		return GHPending, fmt.Errorf("decode gh run %s: %w", ref, err)
	}
	if run.Status != "completed" {
		return GHPending, nil
	}
	switch run.Conclusion {
	case "success":
		return GHPass, nil
	case "failure", "cancelled", "canceled", "timed_out", "startup_failure":
		return GHFail, nil
	default:
		return GHPending, nil
	}
}

// CheckPR implements GHChecker: gh pr view <pr> --json state. MERGED passes,
// CLOSED is red, OPEN is pending. Execution errors are infra failures.
func (g *GhCLI) CheckPR(ctx context.Context, pr string) (GHResult, error) {
	out, err := g.exec(ctx, g.ghBin(), "pr", "view", pr, "--json", "state")
	if err != nil {
		return GHPending, fmt.Errorf("gh pr view %s: %w", pr, err)
	}
	var pull ghPrJSON
	if err := unmarshalStrict(out, &pull); err != nil {
		return GHPending, fmt.Errorf("decode gh pr %s: %w", pr, err)
	}
	switch pull.State {
	case "MERGED":
		return GHPass, nil
	case "CLOSED":
		return GHFail, nil
	default:
		return GHPending, nil
	}
}

// ghBin resolves the gh binary name.
func (g *GhCLI) ghBin() string {
	if g.Bin != "" {
		return g.Bin
	}
	return "gh"
}

// ResolvePRHead implements GHChecker: gh pr view <pr> --json headRefOid.
// The returned SHA is what a gh:run gate judges, so CI status is read at
// the attempt's PR head rather than a static branch ref. Execution and
// decode errors are infra failures.
func (g *GhCLI) ResolvePRHead(ctx context.Context, pr string) (string, error) {
	out, err := g.exec(ctx, g.ghBin(), "pr", "view", pr, "--json", "headRefOid")
	if err != nil {
		return "", fmt.Errorf("gh pr view %s: %w", pr, err)
	}
	var head ghPrHeadJSON
	if err := unmarshalStrict(out, &head); err != nil {
		return "", fmt.Errorf("decode gh pr %s head: %w", pr, err)
	}
	if head.HeadRefOid == "" {
		return "", fmt.Errorf("gh pr view %s: no headRefOid in output", pr)
	}
	return head.HeadRefOid, nil
}

// Runner is the gate evaluator behind symphony's GateEvaluator seam. On
// every settled turn it judges the attempt against the gate revision
// pinned at claim time and performs the hub writeback for the verdict:
// green closes (CAS-guarded) and tombstones; red writes the rejection
// back as a hub comment so the next attempt's reconstructed context
// explains it; error returns GateErr without parking the issue — the
// caller owns re-evaluation timing — escalating to needs-human when the
// consecutive-error or consecutive-deferral counter exceeds its
// threshold.
type Runner struct {
	// Dir is the formulas directory holding *.formula.toml files.
	Dir string
	// Hub performs every tracker writeback.
	Hub Hub
	// Esc counts attempts and escalation counters.
	Esc *Escalator
	// Actor is the claim identity used to CAS-guard closes.
	Actor string
	// Gh resolves gh gate conditions; nil uses the gh CLI.
	Gh GHChecker
	// Exec runs shell gate commands; nil uses OsExec.
	Exec ExecFunc
	// CheckTimeout bounds each external gate check, separately from tracker writeback.
	// Non-positive values use the tracker command timeout.
	CheckTimeout time.Duration
	// Log is the diagnostics sink.
	Log *slog.Logger

	mu   sync.Mutex
	pins map[symphony.Revision][]Spec
}

// NewRunner assembles a Runner.
func NewRunner(dir string, hub Hub, esc *Escalator, actor string, log *slog.Logger) *Runner {
	if log == nil {
		log = slog.Default()
	}
	if esc == nil {
		esc = NewEscalator(nil)
	}
	return &Runner{
		Dir:   dir,
		Hub:   hub,
		Esc:   esc,
		Actor: actor,
		Exec:  OsExec,
		Log:   log,
		pins:  map[symphony.Revision][]Spec{},
	}
}

// Pin snapshots the current formulas directory under rev. The snapshot is
// the gate revision an attempt is judged by when the symphony loop passes
// the same revision in its gate request.
func (r *Runner) Pin(rev symphony.Revision) error {
	specs, err := LoadDir(r.Dir)
	if err != nil {
		return fmt.Errorf("pin gate revision %s: %w", rev, err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pins[rev] = specs
	return nil
}

// specsFor returns the pinned specs for rev, falling back to the current
// formulas directory when no pin exists.
func (r *Runner) specsFor(rev symphony.Revision) ([]Spec, error) {
	r.mu.Lock()
	pinned, ok := r.pins[rev]
	r.mu.Unlock()
	if ok {
		return pinned, nil
	}
	return LoadDir(r.Dir)
}

// ensurePin makes claim-time pinning engage through the request path: the
// first evaluation carrying rev snapshots the formulas directory under
// it, and later evaluations with the same rev are judged by that
// snapshot even if the operator edits the formulas mid-attempt. An empty
// revision always reads the live directory.
func (r *Runner) ensurePin(rev symphony.Revision) ([]Spec, error) {
	if rev == "" {
		return LoadDir(r.Dir)
	}
	r.mu.Lock()
	_, pinned := r.pins[rev]
	r.mu.Unlock()
	if !pinned {
		if err := r.Pin(rev); err != nil {
			return nil, err
		}
	}
	return r.specsFor(rev)
}

// ShouldDispatch reports whether an issue may enter the dispatch front.
// A tombstoned issue (closed green and cleaned up) must not be
// re-dispatched even if its record was later purged.
func (r *Runner) ShouldDispatch(ctx context.Context, issue string) (bool, error) {
	ok, err := r.Hub.IsTombstoned(ctx, issue)
	if err != nil {
		return false, fmt.Errorf("tombstone check %s: %w", issue, err)
	}
	return !ok, nil
}

// Evaluate implements symphony.GateEvaluator. The returned outcome's
// verdict is the three-valued gate decision; the error return is always
// nil because infra failures are themselves the error verdict. On
// GateErr the runner performs no writeback that parks or re-routes the
// issue: the caller owns re-evaluation timing (the loop retries the
// gate), so a defer here would collide with the caller's own retry
// scheduling and reconcile.
func (r *Runner) Evaluate(ctx context.Context, req symphony.GateRequest) (symphony.GateOutcome, error) {
	var specs []Spec
	var err error
	// A checkpoint round-trips an absent snapshot as JSON null, which is not a
	// snapshot: fall through to the live formulas directory.
	if snapshot := bytes.TrimSpace(req.GateSpec); len(snapshot) > 0 && string(snapshot) != "null" {
		err = json.Unmarshal(req.GateSpec, &specs)
	} else {
		specs, err = r.ensurePin(req.Revision)
	}
	if err != nil {
		return r.settleErr(ctx, req, "gate config unreadable: "+err.Error(), true)
	}
	if len(specs) == 0 {
		// Without a gate the claim of completion is unverifiable: never
		// auto-green, park the issue instead.
		return r.settleErr(ctx, req, "no gate formulas configured in "+r.Dir, true)
	}

	verdict := symphony.GateGreen
	var output, errOutput string
	var infra, anyRed bool
	for _, spec := range specs {
		v, out, isInfra := r.evaluateSpec(ctx, spec.Config, req)
		if v == symphony.GateErr {
			// An error dominates: a partially observed gate set cannot
			// certify green, and a red under infra failure would consume
			// an attempt unfairly.
			if errOutput == "" {
				errOutput = out
			}
			verdict = symphony.GateErr
			infra = infra || isInfra
			continue
		}
		if v == symphony.GateRed {
			anyRed = true
			if output == "" {
				output = out
			}
		}
	}
	if verdict == symphony.GateErr {
		return r.settleErr(ctx, req, errOutput, infra)
	}
	if anyRed {
		return r.settleRed(ctx, req, output)
	}
	return r.settleGreen(ctx, req, output)
}

// evaluateSpec runs one gate config and returns its verdict, output, and
// whether a GateErr verdict was an infra failure (as opposed to a pending
// external condition).
func (r *Runner) evaluateSpec(ctx context.Context, cfg Config, req symphony.GateRequest) (symphony.GateVerdict, string, bool) {
	bound := r.CheckTimeout
	if bound <= 0 {
		bound = tracker.DefaultCommandTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	switch cfg.Kind {
	case KindShell:
		fn := r.Exec
		if fn == nil {
			fn = OsExec
		}
		// Pass the issue ID as data, then restore the gate's empty positional arguments.
		out, err := fn(ctx, req.Workspace, "sh", "-lc", `export LAMBE_ISSUE_ID="$1"; shift; `+cfg.Command, "sh", req.Issue.ID)
		if err == nil {
			return symphony.GateGreen, trimSpace(out), false
		}
		var exit *ExitFailure
		if errors.As(err, &exit) {
			return symphony.GateRed, trimSpace(exit.Output), false
		}
		return symphony.GateErr, err.Error(), true
	case KindGhRun:
		checker := r.ghChecker()
		ref := cfg.Ref
		if cfg.PR != "" {
			// Judge the attempt's PR head, never a static branch ref.
			head, herr := checker.ResolvePRHead(ctx, cfg.PR)
			if herr != nil {
				return symphony.GateErr, "gh:run pr " + cfg.PR + ": " + herr.Error(), true
			}
			ref = head
		}
		res, err := checker.CheckRun(ctx, ref)
		return ghVerdict(res, err, "gh:run "+ref)
	case KindGhPr:
		res, err := r.ghChecker().CheckPR(ctx, cfg.PR)
		return ghVerdict(res, err, "gh:pr "+cfg.PR)
	default:
		return symphony.GateErr, "unknown gate kind", true
	}
}

// ghVerdict maps a GitHub condition to the three-valued outcome. Checker
// errors are infra failures; GHPending defers without an infra count.
func ghVerdict(res GHResult, err error, what string) (symphony.GateVerdict, string, bool) {
	switch {
	case err != nil:
		return symphony.GateErr, what + ": " + err.Error(), true
	case res == GHPass:
		return symphony.GateGreen, what + ": green", false
	case res == GHFail:
		return symphony.GateRed, what + ": failed", false
	default:
		return symphony.GateErr, what + ": still pending", false
	}
}

// ghChecker resolves the gh checker with its default.
func (r *Runner) ghChecker() GHChecker {
	if r.Gh == nil {
		return &GhCLI{}
	}
	return r.Gh
}

// settleGreen closes the issue behind the actor's claim, tombstones it,
// and resets escalation counters. A close race means someone else already
// closed it: the runner re-reads the issue status — already closed means
// the race winner finished the same terminal transition, so the
// completion stands (returned green, without a tombstone: the close
// winner owns it); anything else (someone re-opened it or holds it
// in_progress) surfaces GateErr so the caller re-reconciles instead of
// treating the issue as terminal here.
func (r *Runner) settleGreen(ctx context.Context, req symphony.GateRequest, output string) (symphony.GateOutcome, error) {
	id := req.Issue.ID
	if err := r.Hub.Close(ctx, id, r.Actor, "gate green"); err != nil {
		if errors.Is(err, ErrCloseRace) {
			status, serr := r.Hub.IssueStatus(ctx, id)
			switch {
			case serr != nil:
				r.Log.Warn("gate green close lost race; status re-read failed", "issue", id, "error", serr)
			case status == StatusClosed:
				r.Log.Info("gate green close lost race; issue already terminal, completion stands", "issue", id)
				return symphony.GateOutcome{Verdict: symphony.GateGreen, Output: output}, nil
			default:
				r.Log.Info("gate green close lost race; issue not terminal, deferring to reconcile", "issue", id, "status", status)
			}
			return symphony.GateOutcome{Verdict: symphony.GateErr, Output: "close lost race: another actor already closed the issue"}, nil
		}
		r.Log.Error("gate green close failed, reporting gate error", "issue", id, "error", err)
		return r.settleErr(ctx, req, "close failed: "+err.Error(), true)
	}
	if err := r.Hub.Tombstone(ctx, id); err != nil {
		r.Log.Warn("tombstone write failed", "issue", id, "error", err)
	}
	if err := r.Esc.ResetCounters(ctx, id); err != nil {
		r.Log.Warn("counter reset failed", "issue", id, "error", err)
	}
	return symphony.GateOutcome{Verdict: symphony.GateGreen, Output: output}, nil
}

// settleRed writes the rejection back to the issue as a hub comment so
// the next attempt's reconstructed context explains it, and escalates to
// needs-human at the attempt cap. The red verdict itself consumes the
// attempt budget in the caller.
func (r *Runner) settleRed(ctx context.Context, req symphony.GateRequest, output string) (symphony.GateOutcome, error) {
	id := req.Issue.ID
	if err := r.Esc.ResetCounters(ctx, id); err != nil {
		r.Log.Warn("counter reset failed", "issue", id, "error", err)
	}
	if r.Esc.CapReached(req.Attempt) {
		r.Log.Error("attempt cap reached, escalating to needs-human", "issue", id, "attempt", req.Attempt)
		if err := r.Hub.NeedsHuman(ctx, id, fmt.Sprintf("attempt cap %d reached with the gate still red", r.Esc.AttemptCap), output); err != nil {
			r.Log.Error("needs-human transition failed", "issue", id, "error", err)
		}
		return symphony.GateOutcome{Verdict: symphony.GateRed, Output: output}, nil
	}
	body := fmt.Sprintf("lambe gate rejected attempt %d.\nGate output:\n%s", req.Attempt, output)
	if err := r.Hub.Comment(ctx, id, body); err != nil {
		r.Log.Error("rejection comment failed", "issue", id, "error", err)
	}
	return symphony.GateOutcome{Verdict: symphony.GateRed, Output: output}, nil
}

// settleErr records the undecided-gate outcome without consuming the
// attempt budget, counting infra failures and pending-condition deferrals
// separately; either counter past its threshold escalates to
// needs-human so deferral is never an infinite silent loop. It performs
// no parking writeback: GateErr is returned bare and the caller owns
// re-evaluation timing, so the gate must not defer the issue out from
// under the caller's retry schedule.
func (r *Runner) settleErr(ctx context.Context, req symphony.GateRequest, output string, infra bool) (symphony.GateOutcome, error) {
	id := req.Issue.ID
	var escalated bool
	var err error
	if infra {
		escalated, err = r.Esc.RecordGateError(ctx, id)
	} else {
		escalated, err = r.Esc.RecordDeferral(ctx, id)
	}
	if err != nil {
		r.Log.Warn("escalation counter update failed", "issue", id, "error", err)
	}
	if escalated {
		reason := "consecutive gate errors exceeded the escalation threshold"
		if !infra {
			reason = "consecutive gate deferrals exceeded the escalation threshold"
		}
		r.Log.Error("gate escalation threshold reached", "issue", id, "infra", infra)
		if nhErr := r.Hub.NeedsHuman(ctx, id, reason, output); nhErr != nil {
			r.Log.Error("needs-human transition failed", "issue", id, "error", nhErr)
		}
		return symphony.GateOutcome{Verdict: symphony.GateErr, Output: output}, nil
	}
	return symphony.GateOutcome{Verdict: symphony.GateErr, Output: output}, nil
}

// compile-time seam check.
var _ symphony.GateEvaluator = (*Runner)(nil)

// FinalizeCompletion repairs bookkeeping after an owned issue was durably
// closed before the supervisor could finish its green transition.
func (r *Runner) FinalizeCompletion(ctx context.Context, issueID string) error {
	if err := r.Hub.Tombstone(ctx, issueID); err != nil {
		return err
	}
	return r.Esc.ResetCounters(ctx, issueID)
}
