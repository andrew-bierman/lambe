// Package tui implements a Bubble Tea dashboard for lambe.
//
// It displays a scrollable run list, a detail pane with full agent output, a
// daemon status header, and a keybinding footer. The model reads the same
// live.json snapshot that `lambe logs` uses, so it is a pure viewer: it never
// mutates daemon state.
//
// Layout and presentation patterns are informed by other Bubble Tea dashboards
// such as Abacus (Beads issue viewer) and PUG (Terraform TUI): split panes,
// phase badges, pill-style footer hints, and lipgloss-aware layout arithmetic.
package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/reflow/wordwrap"

	"github.com/andrew-bierman/lambe/internal/daemon"
)

const (
	// tickInterval is how often the TUI re-reads the live snapshot.
	tickInterval = 250 * time.Millisecond

	minViewportWidth  = 20
	minViewportHeight = 8
	minListWidth      = 24
	maxListWidth      = 50
)

// Styles.
var (
	baseStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#E0E0E0"))

	headerStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#FAFAFA")).
			Background(lipgloss.Color("#7D56F4")).
			Padding(0, 1)

	footerStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#A0A0A0")).
			Background(lipgloss.Color("#1E1E2E")).
			Padding(0, 1)

	listTitleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#FAFAFA")).
			Background(lipgloss.Color("#5A4FCF")).
			Padding(0, 1)

	selectedRunStyle = lipgloss.NewStyle().
				Background(lipgloss.Color("#3A3A4A")).
				Foreground(lipgloss.Color("#FAFAFA"))

	runStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#E0E0E0"))

	detailTitleStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("#FAFAFA")).
				Background(lipgloss.Color("#5A4FCF")).
				Padding(0, 1)

	phaseRunningStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("#00FF7F"))

	phaseGatingStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("#00BFFF"))

	phaseStoppedGreenStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#00AA00"))

	phaseStoppedRedStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#FF5555"))

	phaseRetryStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#FFAA00"))

	phaseDefaultStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#AAAAAA"))

	labelStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#A0A0A0"))

	valueStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#FAFAFA"))

	keyPillStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#1E1E2E")).
			Background(lipgloss.Color("#A0A0A0")).
			Padding(0, 1)

	errorStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#FF5555"))

	dimStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#808080"))
)

// keyMap defines the TUI's keyboard shortcuts.
type keyMap struct {
	Up     key.Binding
	Down   key.Binding
	PgUp   key.Binding
	PgDown key.Binding
	Home   key.Binding
	End    key.Binding
	Quit   key.Binding
}

func defaultKeyMap() keyMap {
	return keyMap{
		Up: key.NewBinding(
			key.WithKeys("up", "k"),
			key.WithHelp("↑/k", "up"),
		),
		Down: key.NewBinding(
			key.WithKeys("down", "j"),
			key.WithHelp("↓/j", "down"),
		),
		PgUp: key.NewBinding(
			key.WithKeys("pgup", "K"),
			key.WithHelp("pgup/K", "page up"),
		),
		PgDown: key.NewBinding(
			key.WithKeys("pgdown", "J"),
			key.WithHelp("pgdn/J", "page down"),
		),
		Home: key.NewBinding(
			key.WithKeys("home", "g"),
			key.WithHelp("home/g", "top"),
		),
		End: key.NewBinding(
			key.WithKeys("end", "G"),
			key.WithHelp("end/G", "bottom"),
		),
		Quit: key.NewBinding(
			key.WithKeys("q", "ctrl+c", "esc"),
			key.WithHelp("q", "quit"),
		),
	}
}

// footerHint is a single key hint shown in the footer bar.
type footerHint struct {
	key  string
	desc string
}

// globalFooterHints are always shown (space permitting).
var globalFooterHints = []footerHint{
	{"↑↓", "navigate"},
	{"pgup/dn", "scroll"},
	{"g/G", "top/bottom"},
	{"q", "quit"},
}

// Model is the Bubble Tea model for the lambe dashboard.
type Model struct {
	path      string
	keys      keyMap
	snapshot  daemon.LiveSnapshot
	alive     bool
	err       error
	selected  int
	width     int
	height    int
	listWidth int
	vp        viewport.Model
	lastRead  time.Time
	loading   bool
}

// snapshotMsg carries a freshly read live snapshot.
type snapshotMsg struct {
	snapshot daemon.LiveSnapshot
	alive    bool
	err      error
}

// tickMsg triggers the next snapshot read.
type tickMsg time.Time

// New creates a new TUI model that reads from path.
func New(path string) *Model {
	m := &Model{
		path:    path,
		keys:    defaultKeyMap(),
		loading: true,
	}
	m.vp = viewport.New(0, 0)
	return m
}

// Init implements tea.Model.
func (m *Model) Init() tea.Cmd {
	return tea.Batch(
		readSnapshot(m.path),
		tick(),
	)
}

// Update implements tea.Model.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if key.Matches(msg, m.keys.Quit) {
			return m, tea.Quit
		}
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.updateLayout()
		m.refreshDetail()
		return m, nil

	case snapshotMsg:
		m.loading = false
		m.lastRead = time.Now()
		if msg.err != nil {
			// Keep the previous snapshot on transient read errors so the
			// dashboard does not flicker empty; surface persistent errors.
			if !errors.Is(msg.err, os.ErrNotExist) || m.snapshot.UpdatedAt.IsZero() {
				m.err = msg.err
			}
			return m, nil
		}
		m.err = nil
		m.snapshot = msg.snapshot
		m.alive = msg.alive
		m.clampSelection()
		m.refreshDetail()
		return m, nil

	case tickMsg:
		return m, tea.Batch(readSnapshot(m.path), tick())
	}

	// Navigation updates the list selection or scrolls the detail viewport.
	cmd := m.handleNavigation(msg)
	return m, cmd
}

func (m *Model) handleNavigation(msg tea.Msg) tea.Cmd {
	runs := m.visibleRuns()
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if key.Matches(msg, m.keys.Down) {
			if m.selected < len(runs)-1 {
				m.selected++
				m.refreshDetail()
			}
			return nil
		}
		if key.Matches(msg, m.keys.Up) {
			if m.selected > 0 {
				m.selected--
				m.refreshDetail()
			}
			return nil
		}
		if key.Matches(msg, m.keys.Home) {
			m.selected = 0
			m.refreshDetail()
			return nil
		}
		if key.Matches(msg, m.keys.End) {
			m.selected = len(runs) - 1
			if m.selected < 0 {
				m.selected = 0
			}
			m.refreshDetail()
			return nil
		}
	}

	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	return cmd
}

func (m *Model) updateLayout() {
	if m.width < minViewportWidth || m.height < minViewportHeight {
		return
	}
	m.listWidth = m.width / 3
	if m.listWidth < minListWidth {
		m.listWidth = minListWidth
	}
	if m.listWidth > maxListWidth {
		m.listWidth = maxListWidth
	}
	if m.listWidth > m.width/2 {
		m.listWidth = m.width / 2
	}
	m.vp.Width = m.width - m.listWidth - 1
	if m.vp.Width < minViewportWidth {
		m.vp.Width = minViewportWidth
	}
	m.vp.Height = m.contentHeight()
}

func (m *Model) contentHeight() int {
	header := lipgloss.Height(m.renderHeader())
	footer := lipgloss.Height(m.renderFooter())
	h := m.height - header - footer
	if h < minViewportHeight {
		h = minViewportHeight
	}
	return h
}

func (m *Model) clampSelection() {
	runs := m.visibleRuns()
	if m.selected >= len(runs) {
		m.selected = len(runs) - 1
	}
	if m.selected < 0 {
		m.selected = 0
	}
}

func (m *Model) refreshDetail() {
	runs := m.visibleRuns()
	if m.selected >= len(runs) {
		m.clampSelection()
	}
	if len(runs) == 0 {
		m.vp.SetContent("")
		return
	}
	run := runs[m.selected]
	m.vp.SetContent(detailContent(run, m.vp.Width))
	m.vp.GotoBottom()
}

// View implements tea.Model.
func (m *Model) View() string {
	if m.width == 0 || m.height == 0 {
		return "Initializing..."
	}
	if m.loading && m.snapshot.UpdatedAt.IsZero() {
		return m.centered("Waiting for lambe daemon...")
	}

	parts := []string{
		m.renderHeader(),
		m.renderBody(),
		m.renderFooter(),
	}
	return lipgloss.JoinVertical(lipgloss.Left, parts...)
}

func (m *Model) renderHeader() string {
	state := "stopped"
	stateStyle := phaseStoppedRedStyle
	if m.alive {
		state = "running"
		stateStyle = phaseRunningStyle
	}
	identity := m.snapshot.Identity
	if identity == "" {
		identity = "lambe"
	}
	updated := "never"
	if !m.snapshot.UpdatedAt.IsZero() {
		updated = m.snapshot.UpdatedAt.Format("15:04:05")
	}
	running, stopped, other := m.countRuns()
	stats := fmt.Sprintf("%d runs · %d active · %d stopped · %d other", len(m.visibleRuns()), running, stopped, other)
	left := fmt.Sprintf(" lambe  ·  %s  ·  %s  ·  %s  ·  updated %s ",
		stateStyle.Render(state),
		identity,
		stats,
		updated,
	)
	if m.err != nil {
		left += " · " + errorStyle.Render("read error: "+truncate(m.err.Error(), 50))
	}
	return headerStyle.Width(m.width).Render(left)
}

func (m *Model) countRuns() (running, stopped, other int) {
	for _, run := range m.visibleRuns() {
		switch {
		case run.Phase == "running" || run.Phase == "gating" || run.Phase == "preparing" || run.Phase == "claiming":
			running++
		case strings.HasPrefix(run.Phase, "stopped:"):
			stopped++
		default:
			other++
		}
	}
	return running, stopped, other
}

func (m *Model) renderBody() string {
	height := m.contentHeight()
	list := m.renderList(height)
	detail := m.renderDetail(height)
	return lipgloss.JoinHorizontal(lipgloss.Top, list, detail)
}

func (m *Model) renderList(height int) string {
	if m.listWidth == 0 {
		m.updateLayout()
	}
	runs := m.visibleRuns()
	var lines []string
	title := listTitleStyle.Width(m.listWidth).Render(" Runs ")
	lines = append(lines, title)

	available := height - lipgloss.Height(title)
	if available < 1 {
		available = 1
	}

	if len(runs) == 0 {
		lines = append(lines, runStyle.Render("  (none)"))
		return fillToHeight(lipgloss.JoinVertical(lipgloss.Left, lines...), height, m.listWidth)
	}

	// Scroll the list window so the selected run stays visible.
	start := 0
	if m.selected >= available {
		start = m.selected - available + 1
	}
	end := start + available
	if end > len(runs) {
		end = len(runs)
		start = end - available
		if start < 0 {
			start = 0
		}
	}

	for i := start; i < end; i++ {
		run := runs[i]
		marker := "  "
		style := runStyle
		if i == m.selected {
			marker = "> "
			style = selectedRunStyle
		}
		phase := phaseBadge(run.Phase)
		id := truncate(run.IssueID, m.listWidth-lipgloss.Width(phase)-7)
		line := fmt.Sprintf("%s%s  %s", marker, phase, id)
		lines = append(lines, style.Width(m.listWidth).Render(line))
	}
	return fillToHeight(lipgloss.JoinVertical(lipgloss.Left, lines...), height, m.listWidth)
}

func (m *Model) renderDetail(height int) string {
	if m.width-m.listWidth <= 0 {
		return ""
	}
	paneWidth := m.width - m.listWidth - 1
	if paneWidth < minViewportWidth {
		paneWidth = minViewportWidth
	}
	var title string
	runs := m.visibleRuns()
	if len(runs) == 0 {
		title = " Detail "
		m.vp.SetContent("")
	} else {
		run := runs[m.selected]
		title = fmt.Sprintf(" %s · %s ", run.IssueID, truncate(run.Title, paneWidth-25))
	}
	header := detailTitleStyle.Width(paneWidth).Render(title)
	content := m.vp.View()
	body := lipgloss.NewStyle().Width(paneWidth).Height(height - lipgloss.Height(header)).Render(content)
	return lipgloss.JoinVertical(lipgloss.Left, header, body)
}

func (m *Model) renderFooter() string {
	left := renderFooterHints(globalFooterHints)
	leftWidth := lipgloss.Width(left)

	refresh := ""
	if !m.lastRead.IsZero() {
		refresh = "refreshed " + m.lastRead.Format("15:04:05")
	}
	if m.err != nil {
		refresh = errorStyle.Render("read error (!)")
	}
	right := dimStyle.Render(refresh)
	rightWidth := lipgloss.Width(right)

	spacerWidth := m.width - leftWidth - rightWidth
	if spacerWidth < 1 {
		spacerWidth = 1
	}
	spacer := strings.Repeat(" ", spacerWidth)

	return footerStyle.Width(m.width).Render(left + spacer + right)
}

func (m *Model) centered(text string) string {
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, text)
}

func (m *Model) visibleRuns() []daemon.LiveRun {
	return m.snapshot.Runs
}

// detailContent formats one run for the detail pane.
func detailContent(run daemon.LiveRun, width int) string {
	if width < 1 {
		width = 80
	}
	var sections []string

	header := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("#FAFAFA")).
		Render(truncate(run.Title, width-2))
	sections = append(sections, header)

	makeRow := func(k, v string) string {
		return lipgloss.JoinHorizontal(lipgloss.Left,
			labelStyle.Width(12).Render(k),
			valueStyle.Render(v),
		)
	}

	col1 := []string{
		makeRow("Issue:", run.IssueID),
		makeRow("Phase:", phaseBadge(run.Phase)),
		makeRow("Attempt:", fmt.Sprintf("%d", run.Attempt)),
		makeRow("Turn:", fmt.Sprintf("%d", run.Turn)),
	}
	if run.Thread != "" {
		col1 = append(col1, makeRow("Thread:", run.Thread))
	}

	col2 := []string{}
	if run.Workspace != "" {
		col2 = append(col2, makeRow("Workspace:", run.Workspace))
	}
	if !run.WakeAt.IsZero() {
		col2 = append(col2, makeRow("Wake at:", run.WakeAt.Format(time.RFC3339)))
	}

	left := lipgloss.JoinVertical(lipgloss.Left, col1...)
	var meta string
	if len(col2) > 0 && width >= 70 {
		right := lipgloss.JoinVertical(lipgloss.Left, col2...)
		meta = lipgloss.JoinHorizontal(lipgloss.Top, left, baseStyle.Render(strings.Repeat(" ", 4)), right)
	} else {
		meta = left
	}
	sections = append(sections, meta)

	text := strings.TrimSpace(run.Tail)
	if text == "" {
		text = "(no output yet)"
	} else {
		// Account for viewport padding/borders when wrapping.
		wrapWidth := width - 4
		if wrapWidth < 20 {
			wrapWidth = 20
		}
		text = wordwrap.String(text, wrapWidth)
	}
	sections = append(sections, dimStyle.Render("--- output ---")+"\n"+text)

	return strings.Join(sections, "\n\n")
}

func phaseBadge(phase string) string {
	icon, style := phasePresentation(phase)
	return style.Render(icon + " " + truncate(phase, 14))
}

func phasePresentation(phase string) (string, lipgloss.Style) {
	switch {
	case phase == "running":
		return "▶", phaseRunningStyle
	case phase == "gating" || phase == "preparing" || phase == "claiming":
		return "◉", phaseGatingStyle
	case phase == "stopped: gate green":
		return "✔", phaseStoppedGreenStyle
	case strings.HasPrefix(phase, "stopped:"):
		return "✘", phaseStoppedRedStyle
	case phase == "retry" || phase == "deferred" || phase == "interrupted":
		return "↻", phaseRetryStyle
	default:
		return "○", phaseDefaultStyle
	}
}

func renderFooterHints(hints []footerHint) string {
	parts := make([]string, 0, len(hints))
	for _, h := range hints {
		parts = append(parts, keyPill(h.key, h.desc))
	}
	return strings.Join(parts, "  ")
}

func keyPill(key, desc string) string {
	return keyPillStyle.Render(" "+key+" ") + baseStyle.Render(" ") + baseStyle.Render(desc)
}

func fillToHeight(content string, height, width int) string {
	lines := strings.Split(content, "\n")
	pad := baseStyle.Width(width).Render("")
	for len(lines) < height {
		lines = append(lines, pad)
	}
	if len(lines) > height {
		lines = lines[:height]
	}
	return strings.Join(lines, "\n")
}

func truncate(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(s) <= limit {
		return s
	}
	return s[:limit-1] + "…"
}

// readSnapshot returns a command that reads the live snapshot once.
func readSnapshot(path string) tea.Cmd {
	return func() tea.Msg {
		snapshot, alive, err := daemon.ReadLive(path)
		return snapshotMsg{snapshot: snapshot, alive: alive, err: err}
	}
}

// tick returns a command that fires after tickInterval.
func tick() tea.Cmd {
	return tea.Tick(tickInterval, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

// Run starts the TUI and blocks until the user quits. The context is used
// only for startup; the TUI owns its own lifecycle after Init.
func Run(ctx context.Context, path string) error {
	m := New(path)
	p := tea.NewProgram(m, tea.WithContext(ctx), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}
	return nil
}
