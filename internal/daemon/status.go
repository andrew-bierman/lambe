package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/andrew-bierman/lambe/internal/config"
	"github.com/andrew-bierman/lambe/internal/gate"
	"github.com/andrew-bierman/lambe/internal/httpstate"
	"github.com/andrew-bierman/lambe/internal/mcp"
	"github.com/andrew-bierman/lambe/internal/symphony"
	"github.com/andrew-bierman/lambe/internal/tracker"
)

// ActiveRun is one in-progress supervised run in the morning report.
type ActiveRun struct {
	Issue   string
	Session string
	Attempt int
	// LastFrameAge is the time since the run's last observed agent frame;
	// zero means unknown (no frame observation recorded).
	LastFrameAge time.Duration
	// Assignee is the claim holder.
	Assignee string
}

// DeferredRun is one parked issue with its wake time.
type DeferredRun struct {
	Issue  string
	WakeAt time.Time
	Reason string
}

// NeedsHumanItem is one item blocked on a person: with the reason the
// escalation wrote into tracker metadata when present, the summary of its
// last gate output when one exists, and when the tracker last updated the
// issue.
type NeedsHumanItem struct {
	Issue          string
	Reason         string
	LastGateOutput string
	// Since is the issue's last update time — the closest thing the parked
	// record carries to when it entered needs-human. Zero when the tracker
	// did not report one.
	Since time.Time
}

// GateRejection is one run the completion gate rejected overnight.
type GateRejection struct {
	Issue   string
	Attempt int
	Output  string
}

// Report is the morning report: the daemon's whole supervised world as of
// one read.
type Report struct {
	Active         []ActiveRun
	Deferred       []DeferredRun
	NeedsHuman     []NeedsHumanItem
	GateRejections []GateRejection
	// Queue is the dispatch-front readiness projection for the configured
	// label lanes, read in the same grouped pass as every other section so
	// the report, the queue route, and the TUI share one canonical result.
	Queue []tracker.ReadinessRow
	// Orphaned lists recovered claims with no continuation state that the
	// daemon keeps (or kept) heartbeating; see `lambe claims`.
	Orphaned []symphony.OrphanClaim
	// AttemptsUnreadable is set when the attempt records could not be read, so
	// attempt numbers and wake timers are unknown rather than zero.
	AttemptsUnreadable bool
}

// BuildReport reads the morning report from the local tracker: active runs
// from the in-progress front joined with each session's newest attempt,
// deferred runs from armed wake timers, issues parked as needs-human or
// blocked, and the queue readiness projection. When labels is non-empty,
// only issues carrying every listed label are included (opt-in work lane on
// a shared tracker); routeLabels, when non-empty, are the named-workflow
// routing labels and supersede it, matching the dispatcher's per-route
// reads.
//
// Attempt state arrives as one grouped read. On a shell transport every
// per-issue query costs an engine start, so joining the attempt records that
// way made a production-sized hub report take minutes; every section here
// joins the same grouped set instead.
func BuildReport(ctx context.Context, tr tracker.Tracker, labels, routeLabels []string) (Report, error) {
	rep := Report{}

	// A failed attempt read degrades to "no attempts known": the sections
	// below still render, with attempt numbers at zero and no wake timers.
	attempts, attemptsErr := readAttempts(ctx, tr)
	rep.AttemptsUnreadable = attemptsErr != nil

	issues, err := tr.Dispatchable(ctx, tracker.DispatchQuery{Statuses: []string{tracker.StatusInProgress}, Labels: labels})
	if err != nil {
		return rep, fmt.Errorf("read active runs: %w", err)
	}
	for _, issue := range issues {
		run := ActiveRun{Issue: issue.ID, Session: issue.ID, Assignee: issue.Assignee}
		if session := attempts[issue.ID]; len(session) > 0 {
			run.Attempt = session[len(session)-1].Number
		}
		rep.Active = append(rep.Active, run)
	}

	// Deferred: every attempt record with a future wake time. The tracker
	// surface exposes wake timers only through attempt records, so
	// deferred issues are discovered from the attempts of the non-active
	// front. "ready" is a bd pseudo-status only valid as a sole filter, so
	// the multi-status read must not include it; a deferred run's issue
	// stays open until its wake fires.
	deferred, err := tr.Dispatchable(ctx, tracker.DispatchQuery{Statuses: []string{tracker.StatusOpen, tracker.StatusDeferred}, Labels: labels})
	if err == nil {
		now := time.Now()
		for _, issue := range deferred {
			for _, a := range attempts[issue.ID] {
				if a.WakeAt.After(now) {
					rep.Deferred = append(rep.Deferred, DeferredRun{Issue: issue.ID, WakeAt: a.WakeAt})
					break
				}
			}
		}
		sort.Slice(rep.Deferred, func(i, j int) bool { return rep.Deferred[i].WakeAt.Before(rep.Deferred[j].WakeAt) })
	}

	query := tracker.DispatchQuery{Statuses: []string{tracker.StatusBlocked, gate.StatusNeedsHuman}, Labels: labels, Unlimited: true}
	parked, err := tr.Dispatchable(ctx, query)
	var exitErr *tracker.ExitError
	if errors.As(err, &exitErr) && strings.HasPrefix(exitErr.Stderr, `Error: invalid status "`+gate.StatusNeedsHuman+`" `) {
		// Beads rejects the entire filter when the custom status is unconfigured.
		query.Statuses = query.Statuses[:1]
		parked, err = tr.Dispatchable(ctx, query)
	}
	if err != nil {
		return rep, fmt.Errorf("read issues needing human attention: %w", err)
	}
	for _, issue := range parked {
		if issue.Metadata[tracker.MetaKind] != "" {
			continue
		}
		output := issue.Metadata[gate.MetaLastGateOutput]
		rep.NeedsHuman = append(rep.NeedsHuman, NeedsHumanItem{
			Issue:          issue.ID,
			Reason:         needsHumanReason(issue, output),
			LastGateOutput: summarizeGateOutput(output),
			Since:          issue.UpdatedAt,
		})
	}

	// Queue readiness rides the same grouped read set; a failed readiness
	// read degrades to no rows rather than discarding the sections above —
	// the queue is a viewer projection and the next refresh retries it.
	if rows, err := tracker.QueueReadiness(ctx, tr, labels, routeLabels); err == nil {
		rep.Queue = rows
	}

	return rep, nil
}

// needsHumanReason resolves a parked issue's real reason: the authoritative
// metadata the escalation path stamps first, degrading to the gate-output
// summary when it is absent. It is never a hardcoded constant.
func needsHumanReason(issue tracker.Issue, gateOutput string) string {
	if reason := strings.TrimSpace(issue.Metadata[gate.MetaNeedsHumanReason]); reason != "" {
		return reason
	}
	return summarizeGateOutput(gateOutput)
}

// readAttempts reads lambe's whole attempt set in one query and groups it by
// session. An unreadable tracker yields nil so the report still renders.
func readAttempts(ctx context.Context, tr tracker.Tracker) (map[string][]tracker.Attempt, error) {
	records, err := tr.ListByMetadata(ctx, tracker.MetaKind, tracker.KindAttempt)
	if err != nil {
		return nil, err
	}
	return tracker.GroupAttempts(records), nil
}

// summarizeGateOutput reduces a gate output blob to its first line, capped.
func summarizeGateOutput(out string) string {
	line, _, _ := strings.Cut(out, "\n")
	const capLen = 72
	if len(line) > capLen {
		return line[:capLen] + "…"
	}
	return line
}

// RenderReport writes the morning report as a plain-text table: active
// runs, deferred with wake times, needs-human items with last gate output,
// and last-night gate rejections. No TUI dependencies. The first write
// error aborts rendering and is returned.
func RenderReport(w io.Writer, rep Report) error {
	if err := reportf(w, "active runs:"); err != nil {
		return err
	}
	if len(rep.Active) == 0 {
		if err := reportf(w, "  (none)"); err != nil {
			return err
		}
	} else {
		tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
		if _, err := fmt.Fprintln(tw, "  ISSUE\tSESSION\tATTEMPT\tLAST FRAME\tASSIGNEE"); err != nil {
			return err
		}
		for _, r := range rep.Active {
			age := "unknown"
			if r.LastFrameAge > 0 {
				age = r.LastFrameAge.Truncate(time.Second).String() + " ago"
			}
			if _, err := fmt.Fprintf(tw, "  %s\t%s\t%d\t%s\t%s\n", r.Issue, r.Session, r.Attempt, age, orDash(r.Assignee)); err != nil {
				return err
			}
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}

	if err := reportf(w, "deferred:"); err != nil {
		return err
	}
	if len(rep.Deferred) == 0 {
		if err := reportf(w, "  (none)"); err != nil {
			return err
		}
	} else {
		tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
		if _, err := fmt.Fprintln(tw, "  ISSUE\tWAKE AT"); err != nil {
			return err
		}
		for _, d := range rep.Deferred {
			if _, err := fmt.Fprintf(tw, "  %s\t%s\n", d.Issue, d.WakeAt.Format(time.RFC3339)); err != nil {
				return err
			}
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}

	if err := reportf(w, "needs human:"); err != nil {
		return err
	}
	if len(rep.NeedsHuman) == 0 {
		if err := reportf(w, "  (none)"); err != nil {
			return err
		}
	} else {
		tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
		if _, err := fmt.Fprintln(tw, "  ISSUE\tLAST GATE OUTPUT"); err != nil {
			return err
		}
		for _, n := range rep.NeedsHuman {
			if _, err := fmt.Fprintf(tw, "  %s\t%s\n", n.Issue, orDash(n.LastGateOutput)); err != nil {
				return err
			}
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}

	if err := renderOrphans(w, rep.Orphaned); err != nil {
		return err
	}
	if err := reportf(w, "last-night gate rejections:"); err != nil {
		return err
	}
	if len(rep.GateRejections) == 0 {
		return reportf(w, "  (history unavailable)")
	}
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "  ISSUE\tATTEMPT\tOUTPUT"); err != nil {
		return err
	}
	for _, g := range rep.GateRejections {
		if _, err := fmt.Fprintf(tw, "  %s\t%d\t%s\n", g.Issue, g.Attempt, orDash(g.Output)); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// reportf writes one formatted report line.
func reportf(w io.Writer, format string, args ...any) error {
	_, err := fmt.Fprintf(w, format+"\n", args...)
	return err
}

// readReport reads the morning report from the first configured tracker.
// No lock is taken and no daemon is contacted.
func readReport(cfg *config.Config, machine string) (Report, error) {
	if len(cfg.Trackers) == 0 {
		return Report{}, errors.New("no trackers configured")
	}
	t := cfg.Trackers[0]
	local, err := tracker.NewLocal("", t.Path, "lambe@"+machine)
	if err != nil {
		return Report{}, fmt.Errorf("open tracker %s: %w", t.Name, err)
	}
	labels, routeLabels := queueLanes(cfg)
	rep, err := BuildReport(context.Background(), local, labels, routeLabels)
	if err != nil {
		return rep, err
	}
	// A failed checkpoint scan only hides the section; the report stands.
	rep.Orphaned, _ = ListOrphanClaims(context.Background(), local, DefaultStateDir(), cfg)
	return rep, nil
}

func renderOrphans(w io.Writer, claims []symphony.OrphanClaim) error {
	if len(claims) == 0 {
		return nil
	}
	if err := reportf(w, "orphaned claims (no continuation state; release with `lambe claims release ISSUE`):"); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "  ISSUE\tAGE\tOWNER\tWORKSPACE\tREASON"); err != nil {
		return err
	}
	for _, c := range claims {
		ws := "absent"
		if c.WorkspacePresent {
			ws = "present"
		}
		age := time.Since(c.Since).Truncate(time.Minute).String()
		if _, err := fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n", c.Issue, age, orDash(c.Owner), ws, c.Reason); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// Status reads the morning report from the configured local tracker and
// writes it to w. It is the body of `lambe status`: no lock is taken and no
// daemon needs to be running. Tracker open failures surface as errors so
// the caller can report them distinctly from an empty report.
func Status(w io.Writer, cfg *config.Config, machine string) error {
	rep, err := readReport(cfg, machine)
	if err != nil {
		return err
	}
	return RenderReport(w, rep)
}

// StatusJSON is `lambe status -json`: the same tracker read as Status,
// written as a state-API snapshot (see RenderStatusJSON).
func StatusJSON(w io.Writer, cfg *config.Config, id httpstate.Daemon, now time.Time) error {
	rep, err := readReport(cfg, id.Machine)
	if err != nil {
		return err
	}
	if rep.AttemptsUnreadable {
		return errors.New("read attempts: tracker attempt records unreadable, so attempt numbers would be wrong")
	}
	stall := config.DefaultStallAfter
	if cfg.Supervisor.StallAfter > 0 {
		stall = cfg.Supervisor.StallAfter
	}
	return RenderStatusJSON(w, rep, id, stall, now)
}

// RenderStatusJSON writes rep as a GET /api/v1/state document, built with
// the same conversion the daemon's route uses. It is a documented subset
// because only the tracker was read: runs are the tracker's in-progress
// claims (state "running", no phase, no event times), and needs_human comes
// from the tracker. Gate rejections, the loop heartbeat, and per-run
// phases live only in a running daemon's projection, so gate_rejections is
// empty with no gate_rejections_as_of, tick_at is absent, and the daemon
// block carries no pid or identity: this process is not the daemon.
func RenderStatusJSON(w io.Writer, rep Report, id httpstate.Daemon, stallAfter time.Duration, now time.Time) error {
	reports := &ReportCache{}
	reports.Store(rep)
	src := NewStateSource(NewRunRegistry(nil, nil), reports, nil)
	src.info = id
	src.SetStallAfter(func() time.Duration { return stallAfter })
	snap := src.Snapshot()
	snap.RunsAsOf, snap.NeedsHumanAsOf = now, now
	snap.GateRejectionsAsOf, snap.GateRejections = time.Time{}, nil
	for _, a := range rep.Active {
		snap.Runs = append(snap.Runs, httpstate.Run{
			Issue: a.Issue, Session: a.Session, Attempt: a.Attempt,
			State: stateName(mcp.StateRunning), Actor: a.Assignee,
		})
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(httpstate.NewSnapshotResponse(snap, id, now))
}

// queueLanes resolves a configuration's readiness lanes: the configured
// required labels and the named-workflow routing labels, joined through the
// tracker package's one code source for that selection. A nil configuration
// has no lanes.
func queueLanes(cfg *config.Config) (labels, routeLabels []string) {
	if cfg == nil {
		return nil, nil
	}
	for _, route := range cfg.Workflows {
		routeLabels = append(routeLabels, route.Label)
	}
	return cfg.Labels, routeLabels
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
