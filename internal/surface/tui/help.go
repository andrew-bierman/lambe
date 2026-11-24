package tui

import (
	"strings"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// helpView names the screen the help overlay documents.
type helpView int

const (
	helpQueue helpView = iota
	helpAttention
	helpHistory
	helpDetail
)

func (v helpView) String() string {
	return [...]string{"Queue", "Attention", "History", "Run details"}[v]
}

// helpRow is one documented key and what it does.
type helpRow struct{ keys, desc string }

type helpGroup struct {
	title string
	rows  []helpRow
}

func rows(bs ...key.Binding) []helpRow {
	out := make([]helpRow, len(bs))
	for i, b := range bs {
		out[i] = helpRow{b.Help().Key, b.Help().Desc}
	}
	return out
}

// helpGroups lists every binding that works in view v. It reads the same
// keyMap that drives the footer hints, so the two cannot drift apart.
func (k keyMap) helpGroups(v helpView) []helpGroup {
	var groups []helpGroup
	if v == helpDetail {
		groups = append(groups, helpGroup{"Details", rows(k.ScrollDn, k.ScrollUp, k.PageDown, k.PageUp, k.GoTop, k.GoBottom, k.Back, k.Focus)})
	} else {
		groups = append(groups,
			helpGroup{"Runs", rows(k.ListDown, k.ListUp, k.PageDown, k.PageUp, k.GoTop, k.GoBottom, k.OpenRun, k.Focus)},
			helpGroup{"Filter", rows(k.Filter, k.FilterApply, k.FilterCancel)})
	}
	groups = append(groups,
		helpGroup{"Views", rows(k.Views, k.Attention, k.History)},
		helpGroup{"General", rows(k.Commands, k.Help, k.Close, k.Quit)})
	return groups
}

func (m *Model) currentHelpView() helpView {
	switch {
	case m.focus == focusDetail:
		return helpDetail
	case m.showHistory:
		return helpHistory
	case m.showAttention:
		return helpAttention
	}
	return helpQueue
}

// helpText renders the overlay body: key groups, then palette commands.
func (m *Model) helpText(width int) string {
	const keyW = 14
	bold := lipgloss.NewStyle().Bold(true).Foreground(accentColor)
	line := func(keys, desc string) string {
		pad := strings.Repeat(" ", max(1, keyW-lipgloss.Width(keys)))
		return m.help.Styles.FullKey.Render(keys) + pad + m.help.Styles.FullDesc.Render(desc)
	}
	var out []string
	for _, g := range m.keys.helpGroups(m.currentHelpView()) {
		out = append(out, bold.Render(g.title))
		for _, r := range g.rows {
			out = append(out, line(r.keys, r.desc))
		}
		out = append(out, "")
	}
	out = append(out, bold.Render("Commands (press : to run)"))
	for _, c := range m.paletteCommands(&commandPalette{}) {
		out = append(out, line(c.hint, c.label))
	}
	out = append(out, "", dimStyle.Render("Mouse wheel scrolls the pane under the pointer."))
	return lipgloss.NewStyle().Width(width).Render(strings.Join(out, "\n"))
}

func (m *Model) openHelp() {
	m.showHelp = true
	m.layoutHelp()
	m.helpVP.GotoTop()
}

// layoutHelp sizes the overlay to the window and reloads its content.
func (m *Model) layoutHelp() {
	width := max(1, min(64, m.width-4))
	m.helpVP = viewport.New(width, max(1, m.height-6))
	m.helpVP.SetContent(m.helpText(width))
}

// updateHelp handles keys while the overlay is open.
func (m *Model) updateHelp(msg tea.KeyMsg) tea.Cmd {
	switch {
	case msg.Type == tea.KeyCtrlC:
		return tea.Quit
	case key.Matches(msg, m.keys.Close), key.Matches(msg, m.keys.Help), msg.Type == tea.KeyEnter:
		m.showHelp = false
	case key.Matches(msg, m.keys.Commands):
		m.showHelp = false
		return m.openPalette()
	case key.Matches(msg, m.keys.GoTop):
		m.helpVP.GotoTop()
	case key.Matches(msg, m.keys.GoBottom):
		m.helpVP.GotoBottom()
	default:
		var cmd tea.Cmd
		m.helpVP, cmd = m.helpVP.Update(msg)
		return cmd
	}
	return nil
}

func (m *Model) renderHelp() string {
	width := m.helpVP.Width
	title := headerStyle.Width(width).MaxWidth(width).Render("Help · " + m.currentHelpView().String())
	lines := []string{title, m.helpVP.View(), dimStyle.Render("↑↓ scroll   Esc close")}
	return m.centerPanel(lines, width, m.width >= 12 && m.height >= 6)
}
