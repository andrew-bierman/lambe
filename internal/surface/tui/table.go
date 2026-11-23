package tui

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/andrew-bierman/lambe/internal/daemon"
)

// clock is the time source for rendered ages; tests replace it.
var clock = time.Now

// Adaptive palette shared by the table, tabs and chrome.
var (
	colBorder = lipgloss.AdaptiveColor{Light: "#B9AFC9", Dark: "#4A3F5E"}
	colFaint  = lipgloss.AdaptiveColor{Light: "#645B73", Dark: "#9A90A8"}
	colSelBg  = lipgloss.AdaptiveColor{Light: "#E4D8FB", Dark: "#352A4C"}
	colBarBg  = lipgloss.AdaptiveColor{Light: "#EBE1FF", Dark: "#29203A"}
)

// Table columns. The title column takes whatever the fixed columns leave.
const (
	phaseColWidth = 9
	ageColWidth   = 6
	phaseColMin   = 58 // narrower tables drop the phase column and rely on the glyph
)

// rowDelegate satisfies list.ItemDelegate for sizing only: the dashboard
// draws rows itself (see renderTable) so it can align columns.
type rowDelegate struct{}

func (rowDelegate) Height() int                                  { return 1 }
func (rowDelegate) Spacing() int                                 { return 0 }
func (rowDelegate) Update(tea.Msg, *list.Model) tea.Cmd          { return nil }
func (rowDelegate) Render(io.Writer, list.Model, int, list.Item) {}

// phaseWord is the short status word shown in the PHASE column.
func phaseWord(phase string, stalled bool) string {
	switch {
	case stalled:
		return "stalled"
	case phase == "stopped: gate green":
		return "passed"
	case phase == "stopped: gate red":
		return "failed"
	case phase == "stopped: gate escalation":
		return "escalated"
	case phase == daemon.PhaseClosedExternally:
		return "closed"
	case strings.HasPrefix(phase, "stopped"):
		return "stopped"
	case phase == "":
		return "unknown"
	}
	return phase
}

// ageText renders a duration as 8s, 4m, 3h or 2d.
func ageText(d time.Duration) string {
	d = max(0, d)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
}

// rowAge is the AGE column: time to retry, time since output, or since update.
func (i runItem) rowAge(now time.Time) string {
	run := i.run
	switch {
	case !run.WakeAt.IsZero():
		return "in " + ageText(run.WakeAt.Sub(now))
	case run.Phase == "ready" || run.Phase == "blocked":
		return ""
	case !i.live || strings.HasPrefix(run.Phase, "stopped"):
		if !run.UpdatedAt.IsZero() {
			return ageText(now.Sub(run.UpdatedAt))
		}
	case !run.LastOutputAt.IsZero():
		return ageText(now.Sub(run.LastOutputAt))
	}
	return ""
}

// tableCols is the resolved column geometry for one table width.
type tableCols struct {
	id, title  int
	showPhase  bool
	totalWidth int
}

func resolveCols(width, idWidth int) tableCols {
	c := tableCols{totalWidth: width, showPhase: width >= phaseColMin}
	c.id = max(4, min(idWidth, 10))
	if width < 50 {
		c.id = min(c.id, 8)
	}
	fixed := 3 + c.id + 1 + 1 + ageColWidth // marker+glyph+space, id, gaps, age
	if c.showPhase {
		fixed += 1 + phaseColWidth
	}
	c.title = max(1, width-fixed)
	return c
}

func (m *Model) idWidth() int {
	w := 0
	for _, it := range m.list.Items() {
		if r, ok := it.(runItem); ok {
			w = max(w, lipgloss.Width(r.run.IssueID))
		}
	}
	return w
}

// pad truncates s to width and right-pads (or left-pads when right) with spaces.
func pad(s string, width int, right bool) string {
	s = ansi.Truncate(s, width, "…")
	gap := strings.Repeat(" ", max(0, width-lipgloss.Width(s)))
	if right {
		return gap + s
	}
	return s + gap
}

func (m *Model) tableHeader(c tableCols) string {
	style := lipgloss.NewStyle().Foreground(colFaint).Bold(true).Background(colBarBg)
	line := "   " + pad("ID", c.id, false) + " " + pad("TITLE", c.title, false)
	if c.showPhase {
		line += " " + pad("PHASE", phaseColWidth, false)
	}
	line += " " + pad("AGE", ageColWidth, true)
	return style.Width(c.totalWidth).MaxWidth(c.totalWidth).Render(ansi.Truncate(line, c.totalWidth, ""))
}

func (m *Model) filterHeader(width int) string {
	style := lipgloss.NewStyle().Background(colBarBg).Width(width).MaxWidth(width)
	if m.list.FilterState() == list.Filtering {
		return style.Render(ansi.Truncate(m.list.FilterInput.View(), width, ""))
	}
	text := lipgloss.NewStyle().Foreground(accentColor).Bold(true).Render("/ "+m.list.FilterValue()) +
		dimStyle.Render("  esc clears")
	return style.Render(ansi.Truncate(text, width, "…"))
}

func (m *Model) renderRow(item runItem, c tableCols, selected bool, now time.Time) string {
	st := func(s lipgloss.Style) lipgloss.Style {
		if selected {
			return s.Background(colSelBg)
		}
		return s
	}
	stalled := stalledFor(item.run, item.live, now) > 0
	icon, phaseStyle := phasePresentation(item.run.Phase)
	if stalled {
		icon, phaseStyle = "⚠", redStyle.Bold(true)
	}
	marker := " "
	if selected {
		marker = lipgloss.NewStyle().Foreground(accentColor).Background(colSelBg).Render("▌")
	}
	title := strings.Join(strings.Fields(cleanTaskText(item.run.Title)), " ")
	titleStyle := valueStyle
	if selected {
		titleStyle = titleStyle.Bold(true)
	}
	age := item.rowAge(now)
	ageStyle := dimStyle
	if stalled {
		age, ageStyle = ageText(stalledFor(item.run, true, now)), redStyle
	}
	titleCell := st(titleStyle).Render(pad(title, c.title, false))
	if suffix := fmt.Sprintf(" ×%d", item.attempts); item.attempts > 1 && c.title > len(suffix)+8 {
		titleCell = st(titleStyle).Render(pad(title, c.title-len(suffix), false)) + st(dimStyle).Render(suffix)
	}
	parts := []string{
		marker + st(phaseStyle).Render(icon) + st(lipgloss.NewStyle()).Render(" "),
		st(valueStyle.Bold(selected)).Render(pad(item.run.IssueID, c.id, false)),
		st(lipgloss.NewStyle()).Render(" "),
		titleCell,
	}
	if c.showPhase {
		parts = append(parts, st(lipgloss.NewStyle()).Render(" "),
			st(phaseStyle.Bold(false)).Render(pad(phaseWord(item.run.Phase, stalled), phaseColWidth, false)))
	}
	parts = append(parts, st(lipgloss.NewStyle()).Render(" "), st(ageStyle).Render(pad(age, ageColWidth, true)))
	row := strings.Join(parts, "")
	return st(lipgloss.NewStyle()).Width(c.totalWidth).MaxWidth(c.totalWidth).Render(row)
}

func (m *Model) emptyMessage() []string {
	switch {
	case m.list.FilterState() != list.Unfiltered:
		return []string{"No runs match the filter."}
	case m.showHistory:
		return []string{"No past attempts yet.", "Finished runs land here."}
	case m.showAttention:
		return []string{"Nothing needs attention.", "Retries and failures show up here."}
	}
	return []string{"No runs yet.", "Press : to queue a task."}
}

// renderTable draws the column header (or filter prompt) and the visible rows,
// padded to exactly height lines.
func (m *Model) renderTable(width, height int) string {
	if width < 1 || height < 1 {
		return ""
	}
	now := clock()
	cols := resolveCols(width, m.idWidth())
	var lines []string
	if m.colHeader {
		if m.list.FilterState() != list.Unfiltered {
			lines = append(lines, m.filterHeader(width))
		} else {
			lines = append(lines, m.tableHeader(cols))
		}
	}
	items := m.list.VisibleItems()
	if len(items) == 0 {
		for i, msg := range m.emptyMessage() {
			style := dimStyle
			if i == 0 {
				style = valueStyle
			}
			lines = append(lines, "", " "+style.Render(ansi.Truncate(msg, width-2, "…")))
		}
	} else {
		start, end := m.list.Paginator.GetSliceBounds(len(items))
		for i := start; i < end; i++ {
			if r, ok := items[i].(runItem); ok {
				lines = append(lines, m.renderRow(r, cols, i == m.list.Index(), now))
			}
		}
	}
	return fitLines(lines, width, height)
}

// fitLines clamps lines to height rows of exactly width columns.
func fitLines(lines []string, width, height int) string {
	if len(lines) > height {
		lines = lines[:height]
	}
	for i, l := range lines {
		if w := lipgloss.Width(l); w < width {
			lines[i] = l + strings.Repeat(" ", width-w)
		}
	}
	blank := strings.Repeat(" ", width)
	for len(lines) < height {
		lines = append(lines, blank)
	}
	return strings.Join(lines, "\n")
}

// viewCounts returns the number of runs in each dashboard view.
func (m *Model) viewCounts() (queue, attention, history int) {
	return len(m.runsFor(false, false)), len(m.runsFor(false, true)), len(m.runsFor(true, false))
}
