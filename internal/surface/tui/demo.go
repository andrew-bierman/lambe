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
	return run(ctx, demoModel(time.Now()))
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
