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
// Wide terminals show the list and detail side by side. Narrow terminals
// show the focused pane, with Enter opening details and Esc returning to runs.
// Tab switches panes and "/" filters the list.
package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
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

	"github.com/andrew-bierman/lambe/internal/daemon"
)

const (
	tickInterval = 250 * time.Millisecond
	splitWidth   = 72
)

// Styles.
var (
	headerStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.AdaptiveColor{Light: "#241447", Dark: "#F4EBFF"}).
			Background(lipgloss.AdaptiveColor{Light: "#EBE1FF", Dark: "#29203A"}).
			Padding(0, 1)

	footerStyle = lipgloss.NewStyle().
			Foreground(lipgloss.AdaptiveColor{Light: "#494256", Dark: "#C7BFD1"}).
			Background(lipgloss.AdaptiveColor{Light: "#F3EFF7", Dark: "#211B2A"}).
			Padding(0, 1)

	separatorStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#3A3A4A"))

	labelStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#A0A0A0"))

	valueStyle = lipgloss.NewStyle().
			Foreground(lipgloss.AdaptiveColor{Light: "#21182B", Dark: "#FAFAFA"})

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
	OpenRun  key.Binding
	Back     key.Binding
	Help     key.Binding
	Filter   key.Binding
	History  key.Binding
	Commands key.Binding
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
		OpenRun: key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "details")),
		Back:    key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "runs")),
		Help: key.NewBinding(
			key.WithKeys("?"),
			key.WithHelp("?", "help"),
		),
		Commands: key.NewBinding(key.WithKeys(":", "ctrl+p"), key.WithHelp(":", "commands")),
		History:  key.NewBinding(key.WithKeys("h"), key.WithHelp("h", "history")),
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
			key.WithHelp("G", "latest"),
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
		return []key.Binding{k.Back, k.Commands, k.History, k.ScrollUp, k.GoBottom, k.Focus, k.Help, k.Quit}
	}
	return []key.Binding{k.OpenRun, k.Commands, k.History, k.Filter, k.ListDown, k.Help, k.Quit}
}

// FullHelp implements help.KeyMap.
func (k keyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{k.ListUp, k.ListDown, k.Focus, k.Filter, k.History},
		{k.ScrollUp, k.ScrollDn, k.PageUp, k.PageDown},
		{k.GoTop, k.GoBottom, k.Help, k.Quit},
	}
}

// Model is the Bubble Tea dashboard model.
type Model struct {
	path          string
	keys          keyMap
	trackerPath   string
	palette       *commandPalette
	commandStatus string
	commandErr    error
	queueTask     func(string) (string, error)
	queuePending  bool
	showActivity  bool

	list     list.Model
	delegate list.DefaultDelegate
	detail   viewport.Model
	help     help.Model
	spinner  spinner.Model

	snapshot        daemon.LiveSnapshot
	alive           bool
	err             error
	width           int
	height          int
	focus           focusArea
	selectedID      string
	selectedAt      time.Time
	selectedAttempt int
	showHistory     bool
	loading         bool
	paneHeaders     bool
	panelInset      int
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
	accent := lipgloss.AdaptiveColor{Light: "#6941A5", Dark: "#C5A7FF"}
	selection := lipgloss.AdaptiveColor{Light: "#EBE1FF", Dark: "#29203A"}
	delegate.Styles.SelectedTitle = delegate.Styles.SelectedTitle.
		Bold(true).Foreground(accent).Background(selection).BorderForeground(accent)
	delegate.Styles.SelectedDesc = delegate.Styles.SelectedDesc.
		Foreground(accent).Background(selection).BorderForeground(accent)
	m.delegate = delegate
	m.list = list.New(nil, delegate, 0, 0)
	m.list.SetShowHelp(false)
	m.list.SetShowTitle(false)
	m.list.SetShowStatusBar(false)
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
	if m.path == "" {
		return nil
	}
	return tea.Batch(readSnapshot(m.path), tick(), m.spinner.Tick)
}

// Update implements tea.Model.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		follow := m.detail.AtBottom()
		m.width = msg.Width
		m.height = msg.Height
		if m.width < splitWidth && m.list.FilterState() == list.Filtering {
			m.setFocus(focusList)
		}
		m.layout()
		m.refreshDetail(follow)
		return m, nil

	case snapshotMsg:
		m.loading = false
		if msg.err != nil {
			// Keep the last good snapshot on transient errors (for example a
			// daemon restart rewriting the file) instead of flashing empty.
			if !errors.Is(msg.err, os.ErrNotExist) || m.snapshot.UpdatedAt.IsZero() {
				m.err = msg.err
			}
			return m, nil
		}
		m.err = nil
		runs, history := m.snapshot.Runs, m.snapshot.History
		runsChanged := !slices.Equal(runs, msg.snapshot.Runs)
		historyChanged := !slices.Equal(history, msg.snapshot.History)
		if runsChanged {
			runs = slices.Clone(msg.snapshot.Runs)
		}
		if historyChanged {
			history = slices.Clone(msg.snapshot.History)
		}
		m.snapshot = msg.snapshot
		m.snapshot.Runs, m.snapshot.History = runs, history
		aliveChanged := m.alive != msg.alive
		m.alive = msg.alive
		if runsChanged || (m.showHistory && (historyChanged || aliveChanged)) {
			m.setRuns(m.visibleRuns())
			m.refreshDetail(m.detail.AtBottom())
		}
		return m, nil

	case list.FilterMatchesMsg:
		// The current results were applied synchronously; this message may be stale.
		return m, nil

	case tickMsg:
		return m, tea.Batch(readSnapshot(m.path), tick())

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		cmds = append(cmds, cmd)
		return m, tea.Batch(cmds...)

	case tea.KeyMsg:
		if m.showActivity {
			switch msg.Type {
			case tea.KeyCtrlC:
				return m, tea.Quit
			case tea.KeyEsc, tea.KeyEnter:
				m.showActivity = false
			}
			if key.Matches(msg, m.keys.Commands) {
				m.showActivity = false
				return m, m.openPalette()
			}
			return m, nil
		}
		if m.palette != nil {
			return m, m.updatePalette(msg)
		}
		return m.handleKey(msg)
	case commandResult:
		m.commandErr = msg.err
		m.commandStatus = msg.status
		if msg.queued {
			m.queuePending = false
		}
		return m, nil
	case tea.MouseMsg:
		if m.palette != nil || m.showActivity {
			return m, nil
		}
		if m.list.FilterState() != list.Filtering && (msg.Button == tea.MouseButtonWheelUp || msg.Button == tea.MouseButtonWheelDown) {
			m.scrollMouse(msg)
			return m, nil
		}
	}

	if m.palette != nil {
		var cmd tea.Cmd
		m.palette.input, cmd = m.palette.input.Update(msg)
		return m, cmd
	}
	if m.list.FilterState() == list.Filtering {
		return m, m.updateFilter(msg)
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
		return m, m.updateFilter(msg)
	}

	switch {
	case key.Matches(msg, m.keys.Quit):
		return m, tea.Quit
	case key.Matches(msg, m.keys.Focus):
		if m.focus == focusList {
			m.setFocus(focusDetail)
		} else {
			m.setFocus(focusList)
		}
		return m, nil
	case m.focus == focusList && m.selectedID != "" && key.Matches(msg, m.keys.OpenRun):
		m.setFocus(focusDetail)
		return m, nil
	case m.focus == focusDetail && key.Matches(msg, m.keys.Back):
		m.setFocus(focusList)
		return m, nil
	case key.Matches(msg, m.keys.Commands):
		return m, m.openPalette()
	case key.Matches(msg, m.keys.History):
		m.setHistory(!m.showHistory)
		return m, nil
	case key.Matches(msg, m.keys.Help):
		follow := m.detail.AtBottom()
		m.help.ShowAll = !m.help.ShowAll
		m.layout()
		if follow {
			m.detail.GotoBottom()
		} else {
			m.detail.SetYOffset(m.detail.YOffset)
		}
		return m, nil
	case key.Matches(msg, m.keys.Filter):
		if m.width < splitWidth {
			m.setFocus(focusList)
		}
		var cmd tea.Cmd
		m.list, cmd = m.list.Update(msg)
		m.syncSelection()
		return m, cmd
	}

	if m.focus == focusDetail {
		if cmd, handled := m.scrollDetail(msg); handled {
			return m, cmd
		}
		return m, m.updateFocused(msg)
	}

	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	m.syncSelection()
	return m, cmd
}

func (m *Model) updateFilter(msg tea.Msg) tea.Cmd {
	query := m.list.FilterInput.Value()
	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	if current := m.list.FilterInput.Value(); m.list.FilterState() == list.Filtering && current != query {
		// Bubbles' queued filter shares the old rune slice, which text edits can overwrite.
		m.list.FilterInput.SetValue(current)
		// Update matches before Enter can accept the query, while keeping cursor commands.
		m.replaceListItems(m.list.Items())
	}
	m.syncSelection()
	return cmd
}

func (m *Model) setFocus(focus focusArea) {
	m.focus = focus
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

func (m *Model) scrollMouse(msg tea.MouseMsg) {
	firstRow := 1 + m.panelInset
	if m.paneHeaders {
		firstRow++
	}
	if msg.Y < firstRow || msg.Y >= firstRow+m.list.Height() {
		return
	}
	pane := m.focus
	if m.width >= splitWidth {
		switch {
		case msg.X < m.list.Width()+4*m.panelInset:
			pane = focusList
		case msg.X >= m.list.Width()+3+4*m.panelInset:
			pane = focusDetail
		default:
			return
		}
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
	bodyHeight := max(1, m.height-1-lipgloss.Height(m.renderFooter()))
	listWidth, detailWidth := m.width, m.width
	if m.width >= splitWidth {
		listWidth = min(48, m.width/3)
		detailWidth = m.width - listWidth - 3
	}

	m.paneHeaders = bodyHeight > 1
	if m.paneHeaders {
		bodyHeight--
	}
	m.panelInset = 0
	if m.width >= 28 && bodyHeight >= 5 {
		m.panelInset = 1
		bodyHeight -= 2
		listWidth -= 4
		detailWidth -= 4
	}
	spacing := 1
	if m.width < splitWidth {
		spacing = 0
	}
	m.delegate.SetSpacing(spacing)
	m.list.SetDelegate(m.delegate)
	m.list.SetSize(listWidth, bodyHeight)
	m.detail.Width = detailWidth
	m.detail.Height = bodyHeight
}

func (m *Model) setHistory(show bool) {
	if m.showHistory == show {
		return
	}
	m.showHistory = show
	if show {
		m.keys.History.SetHelp("h", "queue")
	} else {
		m.keys.History.SetHelp("h", "history")
	}
	m.setFocus(focusList)
	m.list.ResetFilter()
	m.list.ResetSelected()
	m.selectedID = ""
	m.setRuns(m.visibleRuns())
	m.refreshDetail(!show)
	if show {
		m.detail.GotoTop()
	}
}

func (m *Model) visibleRuns() []daemon.LiveRun {
	if !m.showHistory {
		return m.snapshot.Runs
	}
	history := slices.Clone(m.snapshot.History)
	for _, run := range m.snapshot.Runs {
		if !m.alive || strings.HasPrefix(run.Phase, "stopped:") {
			history = append(history, run)
		}
	}
	slices.SortStableFunc(history, func(a, b daemon.LiveRun) int {
		return b.UpdatedAt.Compare(a.UpdatedAt)
	})
	return history
}

// setRuns replaces the list contents while preserving the selected issue.
func (m *Model) setRuns(runs []daemon.LiveRun) {
	items := make([]list.Item, len(runs))
	for i, run := range runs {
		items[i] = runItem{run: run}
	}
	m.replaceListItems(items)
	if m.selectedID != "" {
		for i, item := range m.list.VisibleItems() {
			if runItem, ok := item.(runItem); ok && runItem.run.IssueID == m.selectedID &&
				(!m.showHistory || (runItem.run.Attempt == m.selectedAttempt && runItem.run.UpdatedAt.Equal(m.selectedAt))) {
				m.list.Select(i)
				return
			}
		}
	}
	if visible := m.list.VisibleItems(); len(visible) > 0 && m.list.SelectedItem() == nil {
		m.list.Select(len(visible) - 1)
	}
	m.syncSelection()
}

func (m *Model) replaceListItems(items []list.Item) {
	if filter := m.list.SetItems(items); filter != nil {
		m.list, _ = m.list.Update(filter())
		m.list.SetSize(m.list.Width(), m.list.Height())
	}
}

// syncSelection records the selected issue and refreshes the detail pane when
// the selection moved.
func (m *Model) syncSelection() {
	previousID, previousAt, previousAttempt := m.selectedID, m.selectedAt, m.selectedAttempt
	if item, ok := m.list.SelectedItem().(runItem); ok {
		m.selectedID = item.run.IssueID
		m.selectedAt = item.run.UpdatedAt
		m.selectedAttempt = item.run.Attempt
	} else {
		m.selectedID = ""
	}
	if m.selectedID != previousID || (m.showHistory && (!m.selectedAt.Equal(previousAt) || m.selectedAttempt != previousAttempt)) {
		m.refreshDetail(!m.showHistory && m.detail.AtBottom())
		if m.showHistory {
			m.detail.GotoTop()
		}
	}
}

func (m *Model) selectedRun() (daemon.LiveRun, bool) {
	item, ok := m.list.SelectedItem().(runItem)
	if !ok {
		return daemon.LiveRun{}, false
	}
	return item.run, true
}

func (m *Model) refreshDetail(follow bool) {
	run, ok := m.selectedRun()
	if !ok {
		m.detail.SetContent(dimStyle.Render("No active run. Output appears here when a run starts."))
		return
	}
	m.detail.SetContent(detailContent(run, m.detail.Width))
	if follow {
		m.detail.GotoBottom()
	} else {
		m.detail.SetYOffset(m.detail.YOffset)
	}
}

// View implements tea.Model.
func (m *Model) View() string {
	if m.width == 0 || m.height == 0 {
		return "Initializing..."
	}
	if m.showActivity {
		return m.renderActivity()
	}
	if m.palette != nil {
		return m.renderPalette()
	}
	if m.loading && m.snapshot.UpdatedAt.IsZero() {
		return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center,
			ansi.Truncate(m.spinner.View()+" Waiting for lambe daemon...", m.width, "…"))
	}

	footer := m.renderFooter()
	if m.height == 1 {
		return footer
	}
	header := m.renderHeader()
	if m.height == 2 {
		return lipgloss.JoinVertical(lipgloss.Left, header, footer)
	}
	return lipgloss.JoinVertical(lipgloss.Left, header, m.renderBody(), footer)
}

func (m *Model) renderHeader() string {
	state := "stopped"
	stateStyle := redStyle
	if m.alive {
		state = "running"
		stateStyle = runningStyle
	}
	updated := "never"
	if !m.snapshot.UpdatedAt.IsZero() {
		updated = m.snapshot.UpdatedAt.Format("15:04:05")
	}
	active, waiting := 0, 0
	if m.alive {
		for _, run := range m.snapshot.Runs {
			switch run.Phase {
			case "running", "claiming", "preparing", "gating":
				active++
			case "queued", "retry", "deferred", "interrupted":
				waiting++
			}
		}
	}
	text := fmt.Sprintf("lambe  %s   %d active / %d waiting", stateStyle.Render(state), active, waiting)
	if m.width < 44 {
		text = "lambe  ·  " + stateStyle.Render(state)
		if m.alive {
			text = fmt.Sprintf("lambe %d active · %d waiting", active, waiting)
		}
	}
	if m.commandErr != nil {
		text = "lambe  ·  " + errorStyle.Render(strings.Join(strings.Fields(ansi.Strip(m.commandErr.Error())), " "))
	} else if m.commandStatus != "" {
		text = "lambe  ·  " + m.commandStatus
	} else if m.err != nil {
		text += " · " + errorStyle.Render("read error: "+truncate(m.err.Error(), 60))
	}
	if m.path == "" {
		text = "Demo" + strings.TrimPrefix(text, "lambe")
	} else if m.width >= splitWidth {
		text += "  ·  Updated " + updated
	}
	return headerStyle.Width(m.width).MaxWidth(m.width).Render(ansi.Truncate(text, max(1, m.width-2), "…"))
}

func (m *Model) renderBody() string {
	if m.width < splitWidth && m.focus == focusDetail {
		return m.renderInspector()
	}
	queueContent := m.list.View()
	if len(m.list.Items()) == 0 {
		empty := "No runs yet."
		if m.showHistory {
			empty = "No past attempts yet."
		}
		queueContent = labelStyle.Padding(1, 1).Render(empty)
	}
	listView := lipgloss.NewStyle().
		Width(m.list.Width()).
		MaxWidth(m.list.Width()).
		Height(m.list.Height()).
		MaxHeight(m.list.Height()).
		Render(queueContent)
	if m.paneHeaders {
		name := "Run queue"
		if m.showHistory {
			name = "History"
		}
		queue := fmt.Sprintf("%s  ·  %d", name, len(m.list.VisibleItems()))
		listView = lipgloss.JoinVertical(lipgloss.Left, m.paneHeader(queue, m.list.Width(), focusList), listView)
	}
	listView = m.framePanel(listView, focusList)
	if m.width < splitWidth {
		return listView
	}
	separator := "   "
	if m.panelInset == 0 {
		separator = lipgloss.JoinHorizontal(lipgloss.Top, " ", separatorStyle.Render(strings.Repeat("│\n", lipgloss.Height(listView)-1)+"│"), " ")
	}
	return lipgloss.JoinHorizontal(lipgloss.Top,
		listView,
		separator,
		m.renderInspector(),
	)
}

func (m *Model) renderInspector() string {
	if !m.paneHeaders {
		return m.detail.View()
	}
	label := "Inspector"
	if run, ok := m.selectedRun(); ok {
		label = run.IssueID + "  ·  " + phaseLabel(run.Phase)
		if !m.showHistory {
			now := time.Now()
			timing := ""
			switch {
			case !run.WakeAt.IsZero():
				timing = "retry in " + max(0, run.WakeAt.Sub(now)).Round(time.Second).String()
			case !run.LastOutputAt.IsZero() && run.Phase == "running":
				timing = max(0, now.Sub(run.LastOutputAt)).Round(time.Second).String() + " ago"
				if m.detail.Width >= 44 {
					timing = "output " + timing
				}
			}
			if timing != "" && lipgloss.Width(label)+lipgloss.Width(timing)+5 <= m.detail.Width-2 {
				label += "  ·  " + timing
			}
		}
	}
	content := lipgloss.JoinVertical(lipgloss.Left,
		m.paneHeader(label, m.detail.Width, focusDetail), m.detail.View())
	return m.framePanel(content, focusDetail)
}

func (m *Model) framePanel(content string, pane focusArea) string {
	if m.panelInset == 0 {
		return content
	}
	color := lipgloss.AdaptiveColor{Light: "#80748D", Dark: "#574A68"}
	if m.focus == pane {
		color = lipgloss.AdaptiveColor{Light: "#6941A5", Dark: "#C5A7FF"}
	}
	return lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(color).Padding(0, 1).Render(content)
}

func (m *Model) paneHeader(label string, width int, pane focusArea) string {
	style := footerStyle.Bold(true)
	if m.focus == pane {
		style = headerStyle
	}
	return style.Width(width).MaxWidth(width).
		Render(ansi.Truncate(label, max(1, width-2), "…"))
}

func (m *Model) renderFooter() string {
	style := footerStyle.Width(m.width).MaxWidth(m.width).MaxHeight(max(1, m.height-2))
	if m.width < splitWidth {
		switchPane := m.keys.OpenRun
		if m.focus == focusDetail {
			switchPane = m.keys.Back
		}
		compactHelp := m.help
		compactHelp.Width = max(1, m.width-2)
		compactHelp.ShortSeparator = " "
		shortcuts := []key.Binding{switchPane, m.keys.Commands, m.keys.History, m.keys.Filter, m.keys.Help, m.keys.Quit}
		if m.focus == focusDetail {
			shortcuts = []key.Binding{switchPane, m.keys.GoBottom, m.keys.Commands, m.keys.History, m.keys.Help, m.keys.Quit}
		}
		if m.width < 44 {
			commands := m.keys.Commands
			commands.SetHelp(":", "cmd")
			history := m.keys.History
			if !m.showHistory {
				history.SetHelp("h", "hist")
			}
			helpKey := m.keys.Help
			helpKey.SetHelp("?", "")
			if m.focus == focusDetail {
				latest := m.keys.GoBottom
				latest.SetHelp("G", "end")
				shortcuts = []key.Binding{switchPane, latest, commands, helpKey}
			} else {
				if m.width < 34 {
					switchPane.SetHelp("enter", "view")
				}
				shortcuts = []key.Binding{switchPane, commands, history, helpKey}
			}
		}
		lines := []string{compactHelp.ShortHelpView(shortcuts)}
		if m.help.ShowAll {
			for _, group := range m.keys.FullHelp() {
				lines = append(lines, compactHelp.ShortHelpView(group))
			}
		}
		for i, line := range lines {
			lines[i] = ansi.Truncate(line, compactHelp.Width, "…")
		}
		return style.Render(strings.Join(lines, "\n"))
	}

	left := m.help.View(m.keys)
	status := "Run queue"
	if m.showHistory {
		status = "History"
	}
	if m.focus == focusDetail {
		status = "Following output"
		if m.showHistory {
			status = "Archived output"
		}
		if !m.detail.AtBottom() && !m.showHistory {
			status = fmt.Sprintf("Scrollback %d%%", int(m.detail.ScrollPercent()*100))
		}
	}
	right := valueStyle.Render(status)
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
	return style.Render(left + strings.Repeat(" ", space) + right)
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
	sections = append(sections, lipgloss.NewStyle().Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "#21182B", Dark: "#FAFAFA"}).
		Render(ansi.Truncate(title, width, "…")))

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
	if len(right) > 0 {
		if width >= 70 {
			meta = lipgloss.JoinHorizontal(lipgloss.Top, meta, "    ",
				lipgloss.JoinVertical(lipgloss.Left, right...))
		} else {
			meta = lipgloss.JoinVertical(lipgloss.Left, append(left, right...)...)
		}
	}
	sections = append(sections, meta)

	text := strings.TrimSpace(run.Tail)
	if text == "" {
		text = "(no output yet)"
	}
	sections = append(sections, dimStyle.Render("--- output ---")+"\n"+text)
	return ansi.Wrap(strings.Join(sections, "\n\n"), width, "")
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
func Run(ctx context.Context, path, trackerPath string, queue func(string) (string, error)) error {
	model := New(path)
	model.trackerPath = trackerPath
	model.queueTask = queue
	return run(ctx, model)
}

func run(ctx context.Context, model *Model) error {
	if !term.IsTerminal(os.Stdout.Fd()) {
		return errors.New("lambe tui needs an interactive terminal; use `lambe logs -follow` for pipes")
	}
	p := tea.NewProgram(model, tea.WithContext(ctx), tea.WithAltScreen(), tea.WithMouseCellMotion())
	if _, err := p.Run(); err != nil {
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}
	return nil
}
