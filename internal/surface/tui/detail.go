package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/andrew-bierman/lambe/internal/daemon"
)

func (m *Model) refreshDetail(follow bool) {
	run, ok := m.selectedRun()
	if !ok {
		m.detail.SetContent(dimStyle.Render("No active run. Output appears here when a run starts."))
		return
	}
	content := detailBody(run, m.detail.Width, m.pins)
	if m.showHistory {
		for _, g := range m.historyGroups() {
			if g[0].IssueID == run.IssueID && len(g) > 1 {
				content = attemptList(g, m.clock()) + "\n\n" + content
			}
		}
	}
	if run.Thread != "" && run.Thread == m.agentSession.thread {
		info := m.agentSession.details
		if m.agentSession.err != nil {
			info = "Agent session · " + cleanTaskText(m.agentSession.err.Error())
		}
		content = ansi.Wrap(info, max(1, m.detail.Width), "") + "\n\n" + content
	}
	m.detail.SetContent(content)
	if follow {
		m.detail.GotoBottom()
	} else {
		m.detail.SetYOffset(m.detail.YOffset)
	}
}

func detailContent(run daemon.LiveRun, width int) string {
	return detailBody(run, width, 0)
}

// detailBody formats a run for the preview. The pane pins the title (pins >= 1)
// and an attempt/turn/workflow line (pins >= 2), so the body leaves those out.
func detailBody(run daemon.LiveRun, width, pins int) string {
	if width < 1 {
		width = 80
	}
	if run.Phase == "ready" || run.Phase == "blocked" {
		text := cleanTaskText(run.Tail)
		if pins == 0 {
			text = cleanTaskText(run.Title) + "\n\n" + text
		}
		return ansi.Wrap(strings.TrimSpace(text), width, "")
	}
	var sections []string

	if pins == 0 {
		title := run.Title
		if title == "" {
			title = "(untitled)"
		}
		sections = append(sections, lipgloss.NewStyle().Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "#21182B", Dark: "#FAFAFA"}).
			Render(ansi.Truncate(title, width, "…")))
	}

	if gate := strings.TrimSpace(ansi.Strip(run.LastGate)); gate != "" {
		sections = append(sections, retryStyle.Bold(true).Render("Last gate rejection")+"\n"+gate)
	}

	row := func(k, v string) string {
		return lipgloss.JoinHorizontal(lipgloss.Left,
			labelStyle.Width(12).Render(k),
			valueStyle.Render(v))
	}

	var left []string
	if pins < 2 {
		left = []string{
			row("Issue:", oneLine(run.IssueID)),
			row("Phase:", phaseLabel(run.Phase)),
			row("Attempt:", fmt.Sprintf("%d", run.Attempt)),
			row("Turn:", fmt.Sprintf("%d", run.Turn)),
		}
		if run.Workflow != "" {
			left = append(left, row("Workflow:", oneLine(run.Workflow)))
		}
	}
	if run.Thread != "" && pins < 2 {
		left = append(left, row("Thread:", oneLine(run.Thread)))
	}

	right := []string{}
	if run.Workspace != "" {
		right = append(right, row("Workspace:", oneLine(run.Workspace)))
	}
	if !run.WakeAt.IsZero() {
		right = append(right, row("Wake at:", run.WakeAt.Format(time.RFC3339)))
	}

	meta := lipgloss.JoinVertical(lipgloss.Left, left...)
	if len(right) > 0 {
		if width >= 70 && len(left) > 0 {
			meta = lipgloss.JoinHorizontal(lipgloss.Top, meta, "    ",
				lipgloss.JoinVertical(lipgloss.Left, right...))
		} else {
			meta = lipgloss.JoinVertical(lipgloss.Left, append(left, right...)...)
		}
	}
	if len(left)+len(right) > 0 {
		sections = append(sections, meta)
	}

	sections = append(sections, sectionRule("Agent output", width)+"\n"+formatTranscript(run.Tail))
	return ansi.Wrap(strings.Join(sections, "\n\n"), width, "")
}

// attemptList renders a history group's attempts, newest first.
func attemptList(group []daemon.LiveRun, now time.Time) string {
	lines := []string{headerStyle.Render(fmt.Sprintf("%d attempts", len(group)))}
	for _, r := range group {
		icon, style := phasePresentation(r.Phase)
		age := ""
		if !r.UpdatedAt.IsZero() {
			age = "  " + dimStyle.Render(ageText(now.Sub(r.UpdatedAt))+" ago")
		}
		lines = append(lines, fmt.Sprintf(" %s #%d %s%s", style.Render(icon), r.Attempt, style.Render(phaseWord(r.Phase, false)), age))
	}
	return strings.Join(lines, "\n")
}

// sectionRule is a labelled divider such as "── Agent output ────".
func sectionRule(label string, width int) string {
	head := "── " + label + " "
	return dimStyle.Render(ansi.Truncate(head+strings.Repeat("─", max(0, width-lipgloss.Width(head))), width, ""))
}

func formatTranscript(tail string) string {
	text := strings.TrimSpace(ansi.Strip(tail))
	if text == "" {
		return dimStyle.Render("Waiting for agent output.")
	}
	var lines []string
	previousStart := ""
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "tool started: ") {
			if line == previousStart {
				continue
			}
			previousStart = line
			line = gatingStyle.Render("›") + " " + strings.TrimPrefix(line, "tool started: ")
		} else {
			previousStart = ""
			if tool, ok := strings.CutPrefix(line, "tool finished: "); ok {
				switch {
				case strings.HasSuffix(tool, " (error=true)"):
					line = redStyle.Render("✕") + " " + strings.TrimSuffix(tool, " (error=true)")
				case strings.HasSuffix(tool, " (error=false)"):
					line = dimStyle.Render("✓ " + strings.TrimSuffix(tool, " (error=false)"))
				}
			}
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}
