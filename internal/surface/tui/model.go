// Package tui implements the lambe dashboard on top of Bubbles components.
//
// The dashboard is a pure viewer over the same live.json snapshot that
// `lambe logs` reads. It composes established Bubbles widgets rather than
// hand-rolling terminal layout:
//
//   - bubbles/list      run list with navigation, paging, and filtering
//   - bubbles/viewport  scrollable run detail and agent output
//   - bubbles/help      keybinding footer
//   - bubbles/spinner   refresh indicator
//
// The left pane is the list, the right pane is the detail viewport. Tab moves
// focus between them. Filtering with "/" is provided by bubbles/list.
package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
	"github.com/muesli/reflow/wordwrap"

	"github.com/andrew-bierman/lambe/internal/daemon"
)

const tickInterval = 250 * time.Millisecond

// Styles.
var (
	headerStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#FAFAFA")).
			Background(lipgloss.Color("#7D56F4")).
			Padding(0, 1)

	footerStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#A0A0A0")).
			Background(lipgloss.Color("#1E1E2E")).
			Padding(0, 1)

	separatorStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#3A3A4A"))

	labelStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#A0A0A0"))

	valueStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#FAFAFA"))

	dimStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#808080"))

	errorStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#FF5555"))

	runningStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#00FF7F"))

	gatingStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#00BFFF"))

	greenStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#00AA00"))

	redStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#FF5555"))

	retryStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#FFAA00"))

	neutralStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#AAAAAA"))
)

// runItem adapts a live run to the bubbles/list Item interface.
type runItem struct {
	run daemon.LiveRun
}

func (i runItem) Title() string {
	title := i.run.IssueID
	if i.run.Title != "" {
		title += " · " + i.run.Title
	}
	return title
}

func (i runItem) Description() string {
	desc := phaseLabel(i.run.Phase)
	if i.run.Attempt > 0 {
		desc += fmt.Sprintf(" · attempt %d", i.run.Attempt)
	}
	if i.run.Turn > 0 {
		desc += fmt.Sprintf(" · turn %d", i.run.Turn)
	}
	return desc
}

func (i runItem) FilterValue() string {
	return strings.Join([]string{i.run.IssueID, i.run.Title, i.run.Phase}, " ")
}

// focusArea is the pane receiving navigation keys.
type focusArea int

const (
	focusList focusArea = iota
	focusDetail
)

// keyMap is the dashboard's own keybindings; list navigation keys come from
// bubbles/list and viewport scrolling from bubbles/viewport.
type keyMap struct {
	focus *focusArea

	Quit     key.Binding
	Focus    key.Binding
	Help     key.Binding
	Filter   key.Binding
	ScrollUp key.Binding
	ScrollDn key.Binding
	PageUp   key.Binding
	PageDown key.Binding
	GoTop    key.Binding
	GoBottom key.Binding
	ListUp   key.Binding
	ListDown key.Binding
}

func defaultKeyMap(focus *focusArea) keyMap {
	return keyMap{
		focus: focus,
		Quit: key.NewBinding(
			key.WithKeys("q", "ctrl+c"),
			key.WithHelp("q", "quit"),
		),
		Focus: key.NewBinding(
			key.WithKeys("tab"),
			key.WithHelp("tab", "focus"),
		),
		Help: key.NewBinding(
			key.WithKeys("?"),
			key.WithHelp("?", "help"),
		),
		Filter: key.NewBinding(
			key.WithKeys("/"),
			key.WithHelp("/", "filter"),
		),
		ScrollUp: key.NewBinding(
			key.WithKeys("up", "k"),
			key.WithHelp("↑/k", "scroll"),
		),
		ScrollDn: key.NewBinding(
			key.WithKeys("down", "j"),
			key.WithHelp("↓/j", "scroll"),
		),
		PageUp: key.NewBinding(
			key.WithKeys("pgup", "K", "u", "ctrl+u"),
			key.WithHelp("u/pgup", "page up"),
		),
		PageDown: key.NewBinding(
			key.WithKeys("pgdown", "J", "d", "ctrl+d"),
			key.WithHelp("d/pgdn", "page down"),
		),
		GoTop: key.NewBinding(
			key.WithKeys("home", "g"),
			key.WithHelp("g", "top"),
		),
		GoBottom: key.NewBinding(
			key.WithKeys("end", "G"),
			key.WithHelp("G", "bottom"),
		),
		ListUp: key.NewBinding(
			key.WithKeys("up", "k"),
			key.WithHelp("↑/k", "run"),
		),
		ListDown: key.NewBinding(
			key.WithKeys("down", "j"),
			key.WithHelp("↓/j", "run"),
		),
	}
}

// ShortHelp implements help.KeyMap. Hints depend on which pane has focus.
func (k keyMap) ShortHelp() []key.Binding {
	if k.focus != nil && *k.focus == focusDetail {
		return []key.Binding{k.ScrollUp, k.ScrollDn, k.PageUp, k.PageDown, k.Focus, k.Help, k.Quit}
	}
	return []key.Binding{k.ListUp, k.ListDown, k.PageUp, k.PageDown, k.Focus, k.Filter, k.Help, k.Quit}
}

// FullHelp implements help.KeyMap.
func (k keyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{k.ListUp, k.ListDown, k.Focus, k.Filter},
		{k.ScrollUp, k.ScrollDn, k.PageUp, k.PageDown},
		{k.GoTop, k.GoBottom, k.Help, k.Quit},
	}
}

// Model is the Bubble Tea dashboard model.
type Model struct {
	path string
	keys keyMap

	list    list.Model
	detail  viewport.Model
	help    help.Model
	spinner spinner.Model

	snapshot   daemon.LiveSnapshot
	alive      bool
	err        error
	width      int
	height     int
	focus      focusArea
	selectedID string
	lastRead   time.Time
	loading    bool
}

// snapshotMsg carries a freshly read live snapshot.
type snapshotMsg struct {
	snapshot daemon.LiveSnapshot
	alive    bool
	err      error
}

// tickMsg triggers the next snapshot read.
type tickMsg time.Time

// New creates a dashboard that reads the live snapshot at path.
func New(path string) *Model {
	m := &Model{
		path:    path,
		loading: true,
	}
	m.keys = defaultKeyMap(&m.focus)
	delegate := list.NewDefaultDelegate()
	m.list = list.New(nil, delegate, 0, 0)
	m.list.Title = "Runs ▸"
	m.list.SetShowHelp(false)
	m.list.SetShowTitle(true)
	m.list.SetShowStatusBar(true)
	m.list.DisableQuitKeybindings()

	m.detail = viewport.New(0, 0)

	m.help = help.New()
	m.help.ShowAll = false

	m.spinner = spinner.New()
	m.spinner.Spinner = spinner.Dot
	return m
}

// Init implements tea.Model.
func (m *Model) Init() tea.Cmd {
	return tea.Batch(readSnapshot(m.path), tick(), m.spinner.Tick)
}

// Update implements tea.Model.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.layout()
		m.refreshDetail()
		return m, nil

	case snapshotMsg:
		m.loading = false
		m.lastRead = time.Now()
		if msg.err != nil {
			// Keep the last good snapshot on transient errors (for example a
			// daemon restart rewriting the file) instead of flashing empty.
			if !errors.Is(msg.err, os.ErrNotExist) || m.snapshot.UpdatedAt.IsZero() {
				m.err = msg.err
			}
			return m, nil
		}
		m.err = nil
		m.snapshot = msg.snapshot
		m.alive = msg.alive
		cmd := m.setRuns(msg.snapshot.Runs)
		m.refreshDetail()
		return m, cmd

	case list.FilterMatchesMsg:
		var cmd tea.Cmd
		m.list, cmd = m.list.Update(msg)
		m.syncSelection()
		return m, cmd

	case tickMsg:
		return m, tea.Batch(readSnapshot(m.path), tick())

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		cmds = append(cmds, cmd)
		return m, tea.Batch(cmds...)

	case tea.KeyMsg:
		return m.handleKey(msg)
	}

	// Non-key messages (mouse, etc.) go to the focused component.
	cmds = append(cmds, m.updateFocused(msg))
	return m, tea.Batch(cmds...)
}

func (m *Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Filtering captures all keys except ctrl+c.
	if m.list.FilterState() == list.Filtering {
		if msg.Type == tea.KeyCtrlC {
			return m, tea.Quit
		}
		var cmd tea.Cmd
		m.list, cmd = m.list.Update(msg)
		m.syncSelection()
		return m, cmd
	}

	switch {
	case key.Matches(msg, m.keys.Quit):
		return m, tea.Quit
	case key.Matches(msg, m.keys.Focus):
		if m.focus == focusList {
			m.focus = focusDetail
			m.list.Title = "Runs"
		} else {
			m.focus = focusList
			m.list.Title = "Runs ▸"
		}
		return m, nil
	case key.Matches(msg, m.keys.Help):
		m.help.ShowAll = !m.help.ShowAll
		m.layout()
		return m, nil
	case key.Matches(msg, m.keys.Filter):
		// Hand the filter key to the list even when detail has focus.
		var cmd tea.Cmd
		m.list, cmd = m.list.Update(msg)
		m.syncSelection()
		return m, cmd
	}

	if m.focus == focusDetail {
		if cmd, handled := m.scrollDetail(msg); handled {
			return m, cmd
		}
	}

	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	m.syncSelection()
	return m, cmd
}

// scrollDetail applies viewport scrolling keys when the detail pane has focus.
func (m *Model) scrollDetail(msg tea.KeyMsg) (tea.Cmd, bool) {
	switch {
	case key.Matches(msg, m.keys.ScrollUp):
		m.detail.ScrollUp(1)
		return nil, true
	case key.Matches(msg, m.keys.ScrollDn):
		m.detail.ScrollDown(1)
		return nil, true
	case key.Matches(msg, m.keys.PageUp):
		m.detail.HalfPageUp()
		return nil, true
	case key.Matches(msg, m.keys.PageDown):
		m.detail.HalfPageDown()
		return nil, true
	case key.Matches(msg, m.keys.GoTop):
		m.detail.GotoTop()
		return nil, true
	case key.Matches(msg, m.keys.GoBottom):
		m.detail.GotoBottom()
		return nil, true
	}
	return nil, false
}

func (m *Model) updateFocused(msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	if m.focus == focusDetail {
		m.detail, cmd = m.detail.Update(msg)
	} else {
		m.list, cmd = m.list.Update(msg)
	}
	return cmd
}

// layout sizes the panes from the current window and help state.
func (m *Model) layout() {
	if m.width <= 0 || m.height <= 0 {
		return
	}
	headerHeight := 1
	footerHeight := lipgloss.Height(m.renderFooter())
	bodyHeight := m.height - headerHeight - footerHeight
	if bodyHeight < 3 {
		bodyHeight = 3
	}

	listWidth := m.width / 3
	if listWidth < 24 {
		listWidth = 24
	}
	if listWidth > m.width/2 {
		listWidth = m.width / 2
	}
	if listWidth > 48 {
		listWidth = 48
	}

	m.list.SetSize(listWidth, bodyHeight)

	detailWidth := m.width - listWidth - 3 // separator column + padding
	if detailWidth < 5 {
		detailWidth = 5
	}
	m.detail.Width = detailWidth
	m.detail.Height = bodyHeight
}

// setRuns replaces the list contents while preserving the selected issue.
func (m *Model) setRuns(runs []daemon.LiveRun) tea.Cmd {
	items := make([]list.Item, len(runs))
	for i, run := range runs {
		items[i] = runItem{run: run}
	}
	cmd := m.list.SetItems(items)
	if m.selectedID == "" {
		m.syncSelection()
		return cmd
	}
	for i, item := range items {
		if runItem, ok := item.(runItem); ok && runItem.run.IssueID == m.selectedID {
			m.list.Select(i)
			return cmd
		}
	}
	m.syncSelection()
	return cmd
}

// syncSelection records the selected issue and refreshes the detail pane when
// the selection moved.
func (m *Model) syncSelection() {
	previous := m.selectedID
	if item, ok := m.list.SelectedItem().(runItem); ok {
		m.selectedID = item.run.IssueID
	} else {
		m.selectedID = ""
	}
	if m.selectedID != previous {
		m.refreshDetail()
	}
}

func (m *Model) selectedRun() (daemon.LiveRun, bool) {
	item, ok := m.list.SelectedItem().(runItem)
	if !ok {
		return daemon.LiveRun{}, false
	}
	return item.run, true
}

func (m *Model) refreshDetail() {
	run, ok := m.selectedRun()
	if !ok {
		m.detail.SetContent(dimStyle.Render("No active run. Waiting for the daemon to claim work."))
		return
	}
	follow := m.detail.AtBottom()
	m.detail.SetContent(detailContent(run, m.detail.Width))
	if follow {
		m.detail.GotoBottom()
	}
}

// View implements tea.Model.
func (m *Model) View() string {
	if m.width == 0 || m.height == 0 {
		return "Initializing..."
	}
	if m.loading && m.snapshot.UpdatedAt.IsZero() {
		return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center,
			m.spinner.View()+" Waiting for lambe daemon...")
	}

	header := m.renderHeader()
	body := m.renderBody()
	footer := m.renderFooter()
	return lipgloss.JoinVertical(lipgloss.Left, header, body, footer)
}

func (m *Model) renderHeader() string {
	state := "stopped"
	stateStyle := redStyle
	if m.alive {
		state = "running"
		stateStyle = runningStyle
	}
	identity := m.snapshot.Identity
	if identity == "" {
		identity = "lambe"
	}
	updated := "never"
	if !m.snapshot.UpdatedAt.IsZero() {
		updated = m.snapshot.UpdatedAt.Format("15:04:05")
	}
	runs := len(m.snapshot.Runs)
	text := fmt.Sprintf(" lambe  ·  %s  ·  %s  ·  %d runs  ·  updated %s ",
		stateStyle.Render(state), identity, runs, updated)
	if m.err != nil {
		text += " · " + errorStyle.Render("read error: "+truncate(m.err.Error(), 60))
	}
	return headerStyle.Width(m.width).Render(ansi.Truncate(text, m.width-2, "…"))
}

func (m *Model) renderBody() string {
	bodyHeight := m.height - 1 - lipgloss.Height(m.renderFooter())
	if bodyHeight < 1 {
		bodyHeight = 1
	}
	separator := separatorStyle.Render(strings.Repeat("│\n", bodyHeight-1) + "│")
	return lipgloss.JoinHorizontal(lipgloss.Top,
		m.list.View(),
		" ",
		separator,
		" ",
		m.detail.View(),
	)
}

func (m *Model) renderFooter() string {
	left := m.help.View(m.keys)
	focus := "list"
	if m.focus == focusDetail {
		focus = "detail"
	}
	right := dimStyle.Render(fmt.Sprintf("focus %s · %s", focus, m.lastRead.Format("15:04:05")))
	contentWidth := m.width - 2
	leftWidth := lipgloss.Width(left)
	rightWidth := lipgloss.Width(right)
	if leftWidth+rightWidth+1 > contentWidth {
		avail := contentWidth - rightWidth - 1
		if avail < 0 {
			avail = 0
		}
		left = ansi.Truncate(left, avail, "…")
		leftWidth = lipgloss.Width(left)
	}
	space := contentWidth - leftWidth - rightWidth
	if space < 1 {
		space = 1
	}
	return footerStyle.Width(m.width).Render(left + strings.Repeat(" ", space) + right)
}

// detailContent formats one run for the detail viewport.
func detailContent(run daemon.LiveRun, width int) string {
	if width < 1 {
		width = 80
	}
	var sections []string

	title := run.Title
	if title == "" {
		title = "(untitled)"
	}
	sections = append(sections, lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FAFAFA")).
		Render(truncate(title, width-2)))

	row := func(k, v string) string {
		return lipgloss.JoinHorizontal(lipgloss.Left,
			labelStyle.Width(12).Render(k),
			valueStyle.Render(v))
	}

	left := []string{
		row("Issue:", run.IssueID),
		row("Phase:", phaseLabel(run.Phase)),
		row("Attempt:", fmt.Sprintf("%d", run.Attempt)),
		row("Turn:", fmt.Sprintf("%d", run.Turn)),
	}
	if run.Thread != "" {
		left = append(left, row("Thread:", run.Thread))
	}

	right := []string{}
	if run.Workspace != "" {
		right = append(right, row("Workspace:", run.Workspace))
	}
	if !run.WakeAt.IsZero() {
		right = append(right, row("Wake at:", run.WakeAt.Format(time.RFC3339)))
	}

	meta := lipgloss.JoinVertical(lipgloss.Left, left...)
	if len(right) > 0 && width >= 70 {
		meta = lipgloss.JoinHorizontal(lipgloss.Top, meta, "    ",
			lipgloss.JoinVertical(lipgloss.Left, right...))
	}
	sections = append(sections, meta)

	text := strings.TrimSpace(run.Tail)
	if text == "" {
		text = "(no output yet)"
	} else {
		wrapWidth := width - 2
		if wrapWidth < 20 {
			wrapWidth = 20
		}
		text = wordwrap.String(text, wrapWidth)
	}
	sections = append(sections, dimStyle.Render("--- output ---")+"\n"+text)
	return strings.Join(sections, "\n\n")
}

// phaseLabel renders a phase with an icon and color.
func phaseLabel(phase string) string {
	icon, style := phasePresentation(phase)
	if phase == "" {
		phase = "unknown"
	}
	return style.Render(icon + " " + phase)
}

func phasePresentation(phase string) (string, lipgloss.Style) {
	switch {
	case phase == "running":
		return "▶", runningStyle
	case phase == "gating" || phase == "preparing" || phase == "claiming":
		return "◉", gatingStyle
	case phase == "stopped: gate green":
		return "✔", greenStyle
	case strings.HasPrefix(phase, "stopped:"):
		return "✘", redStyle
	case phase == "retry" || phase == "deferred" || phase == "interrupted":
		return "↻", retryStyle
	default:
		return "○", neutralStyle
	}
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

// Run starts the dashboard and blocks until the user quits.
func Run(ctx context.Context, path string) error {
	if !term.IsTerminal(os.Stdout.Fd()) {
		return errors.New("lambe tui needs an interactive terminal; use `lambe logs -follow` for pipes")
	}
	p := tea.NewProgram(New(path), tea.WithContext(ctx), tea.WithAltScreen(), tea.WithMouseCellMotion())
	if _, err := p.Run(); err != nil {
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}
	return nil
}
