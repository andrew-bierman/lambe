package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/andrew-bierman/lambe/internal/daemon"
)

const (
	// splitWidth is the width at which the run table and the preview pane sit
	// side by side. Below it the preview stacks under the table (when tall
	// enough) or replaces it on Enter.
	splitWidth    = 100
	stackedWidth  = 36
	previewMinRow = 14
)

// layout sizes the table and preview from the current window and help state.
func (m *Model) layout() {
	if m.width <= 0 || m.height <= 0 {
		return
	}
	footerH := lipgloss.Height(m.renderFooter())
	m.tabsH = 0
	switch {
	case m.height >= 16:
		m.tabsH = 2
	case m.height >= 7:
		m.tabsH = 1
	}
	bodyH := max(1, m.height-1-m.tabsH-footerH)
	m.bodyH = bodyH
	m.colHeader = bodyH >= 4
	hdr := 0
	if m.colHeader {
		hdr = 1
	}
	m.previewHeader = bodyH >= 3
	pre := 0
	if m.previewHeader {
		pre = 1
	}
	m.split = m.width >= splitWidth
	m.stacked = !m.split && m.width >= stackedWidth && bodyH >= previewMinRow
	m.tableWidth, m.previewWidth = m.width, m.width
	listH, detailH := bodyH-hdr, bodyH-pre
	switch {
	case m.split:
		m.previewWidth = max(36, m.width*2/5)
		m.tableWidth = m.width - m.previewWidth - 1
	case m.stacked:
		avail := bodyH - hdr - 1
		detailH = avail * 2 / 5
		listH = avail - detailH
	}
	// Pin the run title (and a meta line) above the scrollback when there is room.
	m.pins = 0
	switch {
	case !m.previewHeader:
	case detailH >= 14:
		m.pins = 2
	case detailH >= 8:
		m.pins = 1
	}
	m.list.SetSize(m.tableWidth, max(1, listH))
	m.detail.Width = max(1, m.previewWidth-2)
	m.detail.Height = max(1, detailH-m.pins)
}

// renderBody draws the table, the preview, or both depending on the layout.
func (m *Model) renderBody() string {
	hdr := 0
	if m.colHeader {
		hdr = 1
	}
	table := func() string { return m.renderTable(m.tableWidth, m.list.Height()+hdr) }
	switch {
	case m.split:
		rows := m.bodyH
		bar := strings.TrimSuffix(strings.Repeat("│\n", rows), "\n")
		color := colBorder
		if m.focus == focusDetail {
			color = accentColor
		}
		divider := lipgloss.NewStyle().Foreground(color).Render(bar)
		return lipgloss.JoinHorizontal(lipgloss.Top, table(), divider, m.renderPreview(m.previewWidth))
	case m.stacked:
		return lipgloss.JoinVertical(lipgloss.Left, table(), m.renderPreview(m.width))
	case m.focus == focusDetail:
		return m.renderPreview(m.width)
	}
	return table()
}

// renderPreview draws the preview title bar over the scrolling run detail.
func (m *Model) renderPreview(width int) string {
	pad := lipgloss.NewStyle().Padding(0, 1).Width(width).MaxWidth(width)
	body := pad.Height(m.detail.Height).MaxHeight(m.detail.Height).Render(m.detail.View())
	if !m.previewHeader {
		return body
	}
	parts := []string{m.previewBar(width)}
	if run, ok := m.selectedRun(); ok && m.pins > 0 {
		title := strings.Join(strings.Fields(cleanTaskText(run.Title)), " ")
		if title == "" {
			title = "(untitled)"
		}
		parts = append(parts, pad.Render(valueStyle.Bold(true).Render(ansi.Truncate(title, width-2, "…"))))
		if m.pins > 1 {
			parts = append(parts, pad.Render(dimStyle.Render(ansi.Truncate(runMeta(run), width-2, "…"))))
		}
	} else {
		for range m.pins {
			parts = append(parts, pad.Render(""))
		}
	}
	return lipgloss.JoinVertical(lipgloss.Left, append(parts, body)...)
}

// runMeta is the one-line attempt/turn/workflow summary under the preview title.
func runMeta(run daemon.LiveRun) string {
	meta := fmt.Sprintf("attempt %d · turn %d", run.Attempt, run.Turn)
	for _, extra := range []string{run.Workflow, run.Thread} {
		if extra != "" {
			meta += " · " + extra
		}
	}
	return meta
}

func (m *Model) previewBar(width int) string {
	style := lipgloss.NewStyle().Foreground(colFaint).Background(colBarBg).Bold(true)
	if m.focus == focusDetail || m.stacked {
		style = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#241447", Dark: "#F4EBFF"}).Background(colBarBg).Bold(true)
	}
	label := " Preview"
	if run, ok := m.selectedRun(); ok {
		now := clock()
		stalled := stalledFor(run, m.alive, now)
		icon, phaseStyle := phasePresentation(run.Phase)
		if stalled > 0 {
			icon, phaseStyle = "⚠", redStyle.Bold(true)
		}
		bg := lipgloss.NewStyle().Background(colBarBg)
		label = " " + style.Render(run.IssueID) + bg.Render("  ") +
			phaseStyle.Background(colBarBg).Render(icon+" "+phaseWord(run.Phase, stalled > 0))
		timing := ""
		switch {
		case !run.WakeAt.IsZero():
			timing = "retry in " + ageText(run.WakeAt.Sub(now))
		case stalled > 0:
			timing = "silent " + shortAge(stalled)
		case !run.LastOutputAt.IsZero() && run.Phase == "running":
			timing = "output " + ageText(now.Sub(run.LastOutputAt)) + " ago"
		}
		if timing != "" && lipgloss.Width(label)+lipgloss.Width(timing)+4 <= width {
			label += bg.Render("  ") + lipgloss.NewStyle().Foreground(colFaint).Background(colBarBg).Render(timing)
		}
	}
	return lipgloss.NewStyle().Background(colBarBg).Width(width).MaxWidth(width).
		Render(ansi.Truncate(label, width, "…"))
}

// paneAt maps a screen cell to the pane drawn there.
func (m *Model) paneAt(x, y int) (pane focusArea, ok bool) {
	hdr, pre := 0, 0
	if m.colHeader {
		hdr = 1
	}
	if m.previewHeader {
		pre = 1
	}
	top := 1 + m.tabsH
	inList := func(w int) bool { return x >= 0 && x < w && y >= top+hdr && y < top+hdr+m.list.Height() }
	switch {
	case m.split:
		if inList(m.tableWidth) {
			return focusList, true
		}
		if x > m.tableWidth && y >= top+pre+m.pins && y < top+pre+m.pins+m.detail.Height {
			return focusDetail, true
		}
	case m.stacked:
		if inList(m.width) {
			return focusList, true
		}
		start := top + hdr + m.list.Height() + pre + m.pins
		if y >= start && y < start+m.detail.Height {
			return focusDetail, true
		}
	case m.focus == focusDetail:
		if y >= top+pre+m.pins && y < top+pre+m.pins+m.detail.Height {
			return focusDetail, true
		}
	case inList(m.width):
		return focusList, true
	}
	return focusList, false
}

func (m *Model) scrollMouse(msg tea.MouseMsg) {
	pane, ok := m.paneAt(msg.X, msg.Y)
	if !ok {
		return
	}
	if pane == focusDetail {
		m.detail, _ = m.detail.Update(msg)
		return
	}
	if msg.Button == tea.MouseButtonWheelUp {
		m.list.CursorUp()
	} else {
		m.list.CursorDown()
	}
	m.syncSelection()
}

// position reports "3/7" for the selected row of the current view.
func (m *Model) position() string {
	n := len(m.list.VisibleItems())
	if n == 0 {
		return "0/0"
	}
	return fmt.Sprintf("%d/%d", m.list.Index()+1, n)
}
