package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/andrew-bierman/lambe/internal/config"
	"github.com/andrew-bierman/lambe/internal/daemon"
	"github.com/andrew-bierman/lambe/internal/surface/tui"
	"github.com/andrew-bierman/lambe/internal/symphony"
	"github.com/andrew-bierman/lambe/internal/tracker"
)

func terminalText(text string) string {
	return strings.Map(func(r rune) rune {
		if (unicode.IsControl(r) || unicode.In(r, unicode.Cf)) && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, strings.ToValidUTF8(text, "�"))
}

func runLive(args []string, useTUI bool, _ io.Reader, out io.Writer) error {
	command := "lambe logs"
	if useTUI {
		command = "lambe tui"
	}
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	configFile := fs.String("config", "", "config file")
	var issue string
	var demo, follow, asJSON, brief bool
	if useTUI {
		fs.BoolVar(&demo, "demo", false, "open the dashboard with sample runs")
	} else {
		fs.StringVar(&issue, "issue", "", "show one issue")
		fs.BoolVar(&follow, "follow", false, "follow live output")
		fs.BoolVar(&asJSON, "json", false, "print the shared live snapshot as JSON")
		fs.BoolVar(&brief, "brief", false, "with --json: drop agent output, add attention reasons and ages")
	}
	if err := parseFlags(fs, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if err := noArgs(fs); err != nil {
		return err
	}
	if brief && !asJSON {
		return usageError{errors.New("--brief needs --json")}
	}
	if demo {
		if *configFile != "" {
			return usageError{errors.New("-demo requires lambe tui without -config")}
		}
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		return tui.RunDemo(ctx)
	}
	configPath, err := cliConfigPath(*configFile)
	if err != nil {
		return err
	}
	if err := requireConfig(configPath); err != nil {
		return err
	}
	cfg, err := config.Load(config.Options{File: configPath})
	if err != nil {
		return err
	}
	path := daemon.LivePath(daemon.DefaultStateDir(), cfg)

	if useTUI {
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		trackerPath := ""
		if len(cfg.Trackers) > 0 {
			trackerPath = cfg.Trackers[0].Path
		}
		var queue func(string) (string, error)
		var readiness func() ([]tracker.ReadinessRow, error)
		var workflows []tui.QueueWorkflow
		if trackerPath != "" {
			var routes []string
			for _, route := range cfg.Workflows {
				routes = append(routes, route.Label)
			}
			readiness = func() ([]tracker.ReadinessRow, error) {
				local, err := tracker.NewLocal("", trackerPath, "")
				if err != nil {
					return nil, err
				}
				readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				defer cancel()
				// The shared lane selection and post-filter, not a
				// private read: QueueLanes is the one code source for
				// the label-lane choice, and QueueReadiness verifies
				// every row against the tracker's own records — bd has
				// been observed to ignore label filters — so the TUI
				// never shows a wrong-lane row and agrees with the
				// daemon's queue projection.
				every, anyLabels := tracker.QueueLanes(cfg.Labels, routes)
				return tracker.QueueReadiness(readCtx, local, every, anyLabels)
			}
			create := func(title string, labels []string) (string, error) {
				_, actor := hostIdentity(cfg)
				local, err := tracker.NewLocal("", trackerPath, actor)
				if err != nil {
					return "", err
				}
				issue, err := local.CreateTask(ctx, title, labels)
				return issue.ID, err
			}
			if len(cfg.Workflows) == 0 {
				queue = func(title string) (string, error) { return create(title, cfg.Labels) }
			}
			for _, route := range cfg.Workflows {
				workflows = append(workflows, tui.QueueWorkflow{
					Name:    route.Name,
					Preview: fmt.Sprintf("%s · %s", cfg.Harness, route.Label),
					Queue:   func(title string) (string, error) { return create(title, []string{route.Label}) },
				})
			}
		}
		return tui.Run(ctx, path, cfg.Supervisor.StallAfter, trackerPath, queue, readiness, workflows...)
	}

	return runLogsFollow(path, cfg.Supervisor.StallAfter, issue, follow, asJSON, brief, out)
}

// liveJSON is the `lambe logs -json` document: the shared snapshot plus the
// facts a remote viewer such as the Rex palette cannot derive itself. Alive
// reports whether the daemon that wrote the snapshot is still running, so a
// viewer can tell live rows from a stopped daemon's last state. Machine
// names the host that read the snapshot, so a picker can show where a run's
// output opens.
type liveJSON struct {
	daemon.LiveSnapshot
	Alive   bool
	Machine string
}

// briefRun is one run of `lambe logs --json --brief`: the fields of the full
// snapshot's run without the heavy agent output (Tail, up to 4 KiB each), plus
// the ages and attention reason a picker would otherwise have to derive.
// Field names match the full output, so a reader of one reads the other.
type briefRun struct {
	symphony.Progress
	UpdatedAt      time.Time
	TailBytes      int64
	LastOutputAt   time.Time
	LastActivityAt time.Time `json:",omitempty"`
	// AgeSec is seconds since the run last changed or produced anything.
	AgeSec int64
	// Attention is set when daemon.NeedsAttention flags the run.
	Attention *daemon.Attention `json:",omitempty"`
}

// liveBrief is the `lambe logs --json --brief` document. Additive over
// liveJSON: LambeVersion names the binary that wrote it.
type liveBrief struct {
	PID          int
	Identity     string
	UpdatedAt    time.Time
	TickAt       time.Time `json:",omitempty"`
	Runs         []briefRun
	History      []briefRun `json:",omitempty"`
	Alive        bool
	Machine      string
	LambeVersion string
}

func newLiveBrief(s daemon.LiveSnapshot, alive bool, machine, version string, now time.Time, stallAfter time.Duration) liveBrief {
	if !alive {
		stallAfter = 0 // a stopped daemon's rows are history, never stalled
	}
	sec := func(t time.Time) int64 {
		if t.IsZero() {
			return 0
		}
		return max(0, int64(now.Sub(t)/time.Second))
	}
	convert := func(runs []daemon.LiveRun) []briefRun {
		var out []briefRun
		for _, run := range runs {
			b := briefRun{
				Progress: run.Progress, UpdatedAt: run.UpdatedAt, TailBytes: run.TailBytes,
				LastOutputAt: run.LastOutputAt, LastActivityAt: run.LastActivityAt,
			}
			freshest := max(run.UpdatedAt.UnixNano(), run.LastOutputAt.UnixNano(), run.LastActivityAt.UnixNano())
			if freshest > 0 {
				b.AgeSec = sec(time.Unix(0, freshest))
			}
			if reason, ok := daemon.NeedsAttention(run, now, stallAfter); ok {
				age := sec(run.UpdatedAt)
				if daemon.StalledFor(run, now, stallAfter) > 0 {
					age = b.AgeSec // silence, not the last supervisor update
				}
				b.Attention = &daemon.Attention{Reason: reason, AgeSec: age}
			}
			out = append(out, b)
		}
		return out
	}
	return liveBrief{
		PID: s.PID, Identity: s.Identity, UpdatedAt: s.UpdatedAt, TickAt: s.TickAt,
		Runs: convert(s.Runs), History: convert(s.History),
		Alive: alive, Machine: machine, LambeVersion: version,
	}
}

func runLogsFollow(path string, stallAfter time.Duration, issue string, follow, asJSON, brief bool, out io.Writer) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	previous := map[string]daemon.LiveRun{}
	notifiedMissing := false
	render := func() error {
		snapshot, alive, err := daemon.ReadLive(path)
		if err != nil {
			return fmt.Errorf("read live output: %w; start `lambe run` with the same configuration first", err)
		}
		if brief {
			return json.NewEncoder(out).Encode(newLiveBrief(snapshot, alive, daemon.DefaultMachine(), buildVersion().Version, time.Now(), stallAfter))
		}
		if asJSON {
			return json.NewEncoder(out).Encode(liveJSON{LiveSnapshot: snapshot, Alive: alive, Machine: daemon.DefaultMachine()})
		}
		if !alive && len(previous) == 0 {
			if _, err := fmt.Fprintln(out, "Daemon stopped; showing historical output."); err != nil {
				return err
			}
		}
		runs := selectRuns(snapshot, issue)
		if issue != "" && len(runs) == 0 && !notifiedMissing {
			notifiedMissing = true
			if _, err := fmt.Fprintf(out, "No current or archived output for %s yet.\n", terminalText(issue)); err != nil {
				return err
			}
		}
		for _, run := range runs {
			text := run.Tail
			old, seen := previous[run.IssueID]
			if seen && old.TailBytes == run.TailBytes && old.Progress == run.Progress {
				continue
			}
			if seen {
				text = tailDelta(text, run.TailBytes-old.TailBytes)
			} // A first sighting prints its full tail, whatever TailBytes claims.
			previous[run.IssueID] = run
			if !run.WakeAt.IsZero() {
				text = "wake at " + run.WakeAt.Format(time.RFC3339) + "\n" + text
			}
			if _, err := fmt.Fprintf(out, "%s  %s  attempt %d · turn %d  %s\n%s\n", terminalText(run.IssueID), terminalText(run.Phase), run.Attempt, run.Turn, terminalText(run.Title), terminalText(text)); err != nil {
				return err
			}
		}
		return nil
	}
	if err := render(); err != nil {
		if !asJSON && follow && errors.Is(err, os.ErrNotExist) {
			if _, writeErr := fmt.Fprintln(out, "Waiting for Lambe output; start lambe run with the same configuration."); writeErr != nil {
				return writeErr
			}
		} else {
			return err
		}
	}
	if asJSON || !follow {
		return nil
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := render(); err != nil {
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				return err
			}
		}
	}
}

// selectRuns returns the single run a `lambe logs -issue` invocation shows:
// the live projection when the issue is current, otherwise its newest
// archived attempt from history, and nothing when it appears in neither.
// One entry per issue keeps the follow renderer's per-issue output deltas
// well-defined. Unfiltered output keeps its existing Runs-only view:
// history never leaks into `lambe logs`.
func selectRuns(snapshot daemon.LiveSnapshot, issue string) []daemon.LiveRun {
	if issue == "" {
		return snapshot.Runs
	}
	for _, source := range [][]daemon.LiveRun{snapshot.Runs, snapshot.History} {
		for _, run := range source {
			if run.IssueID == issue {
				return []daemon.LiveRun{run}
			}
		}
	}
	return nil
}

func tailDelta(current string, added int64) string {
	if added < 0 || added >= int64(len(current)) {
		return current
	}
	return current[len(current)-int(added):]
}
