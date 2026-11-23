package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/andrew-bierman/lambe/internal/daemon"
	"github.com/andrew-bierman/lambe/internal/symphony"
	"github.com/andrew-bierman/lambe/internal/tracker"
)

// RunDemo opens the dashboard with sample runs held entirely in memory.
func RunDemo(ctx context.Context) error {
	return run(ctx, demoRichModel(time.Now()))
}

func demoModel(now time.Time) *Model {
	model := New("")
	model.Update(snapshotMsg{snapshot: demoSnapshot(now), alive: true})
	model.Update(readinessMsg{rows: []tracker.ReadinessRow{
		{ID: "demo-47", Title: "Document the recovery workflow", Reason: "Dependencies resolved"},
		{ID: "demo-48", Title: "Publish migration notes", Reason: "Waiting for demo-45 · Validate the migration", Blocked: true},
	}})
	return model
}

func demoRichModel(now time.Time) *Model {
	model := New("")
	model.Update(snapshotMsg{snapshot: demoRichSnapshot(now), alive: true})
	model.Update(readinessMsg{rows: []tracker.ReadinessRow{
		{ID: "demo-47", Title: "Document the recovery workflow", Reason: "Dependencies resolved"},
		{ID: "demo-48", Title: "Publish migration notes", Reason: "Waiting for demo-45 · Validate the migration", Blocked: true},
		{ID: "demo-57", Title: "Draft the 1.0 release notes", Reason: "Dependencies resolved"},
		{ID: "demo-58", Title: "Cut the release candidate", Reason: "Waiting for demo-49 · Bump the Go toolchain", Blocked: true},
	}})
	return model
}

// demoRichSnapshot is the live demo: demoSnapshot plus enough varied runs to
// fill a wide window. Tests use demoSnapshot directly so their goldens stay put.
func demoRichSnapshot(now time.Time) daemon.LiveSnapshot {
	snap := demoSnapshot(now)
	snap.Runs[0].Tail = `Build event 01: resolving the release matrix
▸ tool bash: go build ./...
Build event 02: compiled 14 packages
Build event 03: linux amd64 binary written
Build event 04: linux arm64 binary written
Build event 05: darwin arm64 binary written
▸ tool bash: go test -race ./internal/gate/...
ok  	internal/gate	3.412s
ok  	internal/daemon	5.108s
Build event 06: packaging archives
▸ tool bash: sha256sum dist/*.tar.gz
Build event 07: manifest written
✓ Linux amd64 and arm64 checks passed
✓ Release archives match the manifest
✓ Gate passed: shell "make verify"
… Waiting for the macOS smoke test
`
	quiet := func(id, title, phase string, attempt int, ago time.Duration, tail string) daemon.LiveRun {
		return daemon.LiveRun{
			Progress:     symphony.Progress{IssueID: id, Title: title, Phase: phase, Attempt: attempt, Turn: 1},
			LastOutputAt: now.Add(-ago),
			Tail:         tail,
		}
	}
	done := func(id, title, phase string, attempt int, ago time.Duration, tail string) daemon.LiveRun {
		return daemon.LiveRun{
			Progress:  symphony.Progress{IssueID: id, Title: title, Phase: phase, Attempt: attempt},
			UpdatedAt: now.Add(-ago),
			Tail:      tail,
		}
	}
	snap.Runs = append(snap.Runs,
		quiet("demo-49", "Bump the Go toolchain to 1.27", "running", 1, 3*time.Second, "▸ tool bash: mise use go@1.27\nToolchain updated. Running the full test suite."),
		quiet("demo-50", "Add retry jitter to the tracker client", "running", 1, 14*time.Second, "▸ tool edit: internal/tracker/client.go\nJitter added. Writing a table-driven test."),
		quiet("demo-51", "Regenerate the API fixtures", "gating", 1, 20*time.Second, "Fixtures regenerated. Waiting for the contract check."),
		quiet("demo-52", "Profile the startup path", "gating", 1, 11*time.Minute, "Profile captured. Reading the flame graph."),
		daemon.LiveRun{
			Progress: symphony.Progress{IssueID: "demo-53", Title: "Fix the flaky queue ordering test", Phase: "retry", Attempt: 2, WakeAt: now.Add(2 * time.Minute), LastGate: "TestQueueOrdering failed 1 of 20 runs."},
			Tail:     "Gate rejected the first attempt. Waiting for the next retry.",
		},
		done("demo-54", "Update the changelog", "stopped: gate green", 1, 9*time.Minute, "Changelog updated."),
		done("demo-55", "Tighten config validation errors", "stopped: gate green", 1, 12*time.Minute, "Validation errors now list every problem."),
		done("demo-56", "Port the install script to POSIX sh", "stopped: gate escalation", 3, 7*time.Minute, "Gate failed: shellcheck reported 2 warnings."),
	)
	return snap
}

func demoSnapshot(now time.Time) daemon.LiveSnapshot {
	var output strings.Builder
	for step := range 30 {
		fmt.Fprintf(&output, "Build event %02d: verifying release artifacts\n", step+1)
	}
	output.WriteString("✓ Linux amd64 and arm64 checks passed\n")
	output.WriteString("✓ Release archives match the manifest\n")
	output.WriteString("… Waiting for the macOS smoke test\n")

	return daemon.LiveSnapshot{
		UpdatedAt: now,
		History: []daemon.LiveRun{
			// demo-41 shows grouping: three restart-interrupted attempts and
			// the final green one collapse into a single "passed ×3" row.
			{
				Progress:  symphony.Progress{IssueID: "demo-41", Title: "Publish the changelog", Phase: "stopped: gate green", Attempt: 3},
				UpdatedAt: now.Add(-20 * time.Minute),
				Tail:      "Changelog published.",
			},
			{
				Progress:  symphony.Progress{IssueID: "demo-41", Title: "Publish the changelog", Phase: "interrupted", Attempt: 3},
				UpdatedAt: now.Add(-10 * time.Minute),
			},
			{
				Progress:  symphony.Progress{IssueID: "demo-41", Title: "Publish the changelog", Phase: "interrupted", Attempt: 2},
				UpdatedAt: now.Add(-30 * time.Minute),
			},
			{
				Progress:  symphony.Progress{IssueID: "demo-41", Title: "Publish the changelog", Phase: "interrupted", Attempt: 1},
				UpdatedAt: now.Add(-40 * time.Minute),
			},
			{
				Progress:  symphony.Progress{IssueID: "demo-39", Title: "Close the stale branch", Phase: daemon.PhaseClosedExternally, Attempt: 1},
				UpdatedAt: now.Add(-3 * time.Hour),
				Tail:      "Issue closed outside lambe.",
			},
			{
				Progress:  symphony.Progress{IssueID: "demo-44", Title: "Retry the install smoke test", Phase: "retry", Attempt: 1},
				UpdatedAt: now.Add(-15 * time.Minute),
				Tail:      "First attempt: registry rejected the transient connection.",
			},
		},
		Runs: []daemon.LiveRun{
			{
				Progress:     symphony.Progress{IssueID: "demo-42", Title: "Verify the release build", Phase: "running", Attempt: 1, Turn: 2},
				Tail:         output.String(),
				LastOutputAt: now.Add(-8 * time.Second),
			},
			{
				Progress:     symphony.Progress{IssueID: "demo-43", Title: "Recheck the artifact checksum", Phase: "gating", Attempt: 1},
				LastOutputAt: now.Add(-4 * time.Minute),
				Tail:         "Build finished. Verifying the release manifest.",
			},
			{
				Progress: symphony.Progress{IssueID: "demo-44", Title: "Retry the install smoke test", Phase: "retry", Attempt: 2, WakeAt: now.Add(time.Minute), LastGate: "The registry timed out during the install smoke test."},
				Tail:     "Registry timed out. Waiting for the next retry.",
			},
			{
				Progress:  symphony.Progress{IssueID: "demo-45", Title: "Validate the migration", Phase: "stopped: gate escalation", Attempt: 3},
				UpdatedAt: now.Add(-2 * time.Minute),
				Tail:      "Gate failed: migration snapshot differs from expected output.",
			},
			{
				Progress:  symphony.Progress{IssueID: "demo-46", Title: "Check upgrade instructions", Phase: "stopped: gate green", Attempt: 1},
				UpdatedAt: now.Add(-5 * time.Minute),
				Tail:      "Upgrade walkthrough passed.",
			},
		},
	}
}
