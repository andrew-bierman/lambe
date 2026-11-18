package tui

import (
	"fmt"
	"strings"

	"github.com/andrew-bierman/lambe/internal/daemon"
	"github.com/charmbracelet/lipgloss"
)

func (m *Model) renderActivity() string {
	type activityRow struct {
		label string
		count int
		style lipgloss.Style
	}
	rows := []activityRow{
		{label: "Active", style: runningStyle},
		{label: "Waiting", style: retryStyle},
		{label: "Passed", style: greenStyle},
		{label: "Failed", style: redStyle},
		{label: "Other", style: neutralStyle},
	}
	count := func(run daemon.LiveRun, historical bool) {
		switch {
		case run.Phase == "stopped: gate green":
			rows[2].count++
		case run.Phase == "stopped: gate red" || run.Phase == "stopped: gate escalation":
			rows[3].count++
		case !historical && (run.Phase == "running" || run.Phase == "gating" || run.Phase == "preparing" || run.Phase == "claiming"):
			rows[0].count++
		case !historical && (run.Phase == "retry" || run.Phase == "queued" || run.Phase == "deferred" || run.Phase == "interrupted"):
			rows[1].count++
		default:
			rows[4].count++
		}
	}
	for _, run := range m.snapshot.Runs {
		count(run, !m.alive)
	}
	for _, run := range m.snapshot.History {
		count(run, true)
	}

	width := max(1, min(56, m.width-4))
	n := max(1, rows[0].count, rows[1].count, rows[2].count, rows[3].count, rows[4].count)
	barWidth := max(0, width-15)
	lines := []string{headerStyle.Width(width).MaxWidth(width).Render("Run activity")}
	for _, row := range rows {
		length := (row.count*barWidth + n - 1) / n
		bar := row.style.Render(strings.Repeat("━", length))
		lines = append(lines, fmt.Sprintf(" %-8s %4d %s", row.label, row.count, bar))
	}
	lines = append(lines, dimStyle.Render("Esc back   : commands"))
	return m.centerPanel(lines, width, m.width >= 12 && m.height >= 9)
}
