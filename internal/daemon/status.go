package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/andrew-bierman/lambe/internal/config"
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

// NeedsHumanItem is one item blocked on a person, with the summary of its
// last gate output when one exists.
type NeedsHumanItem struct {
	Issue          string
	LastGateOutput string
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
}

// gateOutputMeta is the tracker metadata key the gate seam stamps its last
// rejection summary under; empty until the gate unit is wired.
const gateOutputMeta = "lambe_last_gate_output"

// BuildReport reads the morning report from the local tracker: active runs
// from the in-progress front joined with each session's newest attempt,
// deferred runs from armed wake timers, and needs-human items from blocked
// issues carrying a gate-output summary. When labels is non-empty, only
// issues carrying every listed label are included (opt-in work lane on a
// shared tracker). Unreadable sections degrade to empty with their error
// reported once.
func BuildReport(ctx context.Context, tr tracker.Tracker, labels []string) (Report, error) {
	rep := Report{}

	issues, err := tr.Dispatchable(ctx, tracker.DispatchQuery{Statuses: []string{tracker.StatusInProgress}, Labels: labels})
	if err != nil {
		return rep, fmt.Errorf("read active runs: %w", err)
	}
	for _, issue := range issues {
		run := ActiveRun{Issue: issue.ID, Session: issue.ID, Assignee: issue.Assignee}
		attempts, err := tr.Attempts(ctx, issue.ID)
		if err == nil && len(attempts) > 0 {
			run.Attempt = attempts[len(attempts)-1].Number
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
		seen := map[string]bool{}
		for _, issue := range deferred {
			attempts, aerr := tr.Attempts(ctx, issue.ID)
			if aerr != nil {
				continue
			}
			for _, a := range attempts {
				if a.WakeAt.After(now) && !seen[issue.ID] {
					seen[issue.ID] = true
					rep.Deferred = append(rep.Deferred, DeferredRun{Issue: issue.ID, WakeAt: a.WakeAt})
				}
			}
		}
		sort.Slice(rep.Deferred, func(i, j int) bool { return rep.Deferred[i].WakeAt.Before(rep.Deferred[j].WakeAt) })
	}

	blocked, err := tr.Dispatchable(ctx, tracker.DispatchQuery{Statuses: []string{tracker.StatusBlocked}, Labels: labels})
	if err == nil {
		for _, issue := range blocked {
			if issue.Metadata[tracker.MetaKind] != "" {
				continue
			}
			rep.NeedsHuman = append(rep.NeedsHuman, NeedsHumanItem{
				Issue:          issue.ID,
				LastGateOutput: summarizeGateOutput(issue.Metadata[gateOutputMeta]),
			})
		}
	}

	return rep, nil
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

	if err := reportf(w, "last-night gate rejections:"); err != nil {
		return err
	}
	if len(rep.GateRejections) == 0 {
		return reportf(w, "  (none)")
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

// Status reads the morning report from the configured local tracker and
// writes it to w. It is the body of `lambe status`: no lock is taken and no
// daemon needs to be running. Tracker open failures surface as errors so
// the caller can report them distinctly from an empty report.
func Status(w io.Writer, cfg *config.Config, machine string) error {
	if len(cfg.Trackers) == 0 {
		return errors.New("no trackers configured")
	}
	t := cfg.Trackers[0]
	local, err := tracker.NewLocal("", t.Path, "lambe@"+machine)
	if err != nil {
		return fmt.Errorf("open tracker %s: %w", t.Name, err)
	}
	rep, err := BuildReport(context.Background(), local, cfg.Labels)
	if err != nil {
		return err
	}
	return RenderReport(w, rep)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
