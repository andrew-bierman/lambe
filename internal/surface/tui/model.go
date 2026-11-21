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
	"unicode"

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
	"github.com/andrew-bierman/lambe/internal/symphony"
	"github.com/andrew-bierman/lambe/internal/tracker"
)

const tickInterval = 250 * time.Millisecond

// Styles.
var (
	accentColor = lipgloss.AdaptiveColor{Light: "#6941A5", Dark: "#C5A7FF"}

	headerStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.AdaptiveColor{Light: "#241447", Dark: "#F4EBFF"}).
			Background(lipgloss.AdaptiveColor{Light: "#EBE1FF", Dark: "#29203A"}).
			Padding(0, 1)

	footerStyle = lipgloss.NewStyle().
			Foreground(lipgloss.AdaptiveColor{Light: "#494256", Dark: "#C7BFD1"}).
			Background(lipgloss.AdaptiveColor{Light: "#F3EFF7", Dark: "#211B2A"}).
			Padding(0, 1)

	labelStyle = lipgloss.NewStyle().
			Foreground(colFaint)

	valueStyle = lipgloss.NewStyle().
			Foreground(lipgloss.AdaptiveColor{Light: "#21182B", Dark: "#FAFAFA"})

	dimStyle = lipgloss.NewStyle().
			Foreground(colFaint)

	errorStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.AdaptiveColor{Light: "#C0262D", Dark: "#FF6B6B"})

	runningStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.AdaptiveColor{Light: "#126447", Dark: "#8DE0BC"})

	gatingStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.AdaptiveColor{Light: "#245C82", Dark: "#9FCAEF"})

	greenStyle = lipgloss.NewStyle().
			Foreground(lipgloss.AdaptiveColor{Light: "#1B7F3B", Dark: "#5FD38D"})

	redStyle = lipgloss.NewStyle().
			Foreground(lipgloss.AdaptiveColor{Light: "#C0262D", Dark: "#FF6B6B"})

	retryStyle = lipgloss.NewStyle().
			Foreground(lipgloss.AdaptiveColor{Light: "#805000", Dark: "#F3C582"})

	neutralStyle = lipgloss.NewStyle().
			Foreground(lipgloss.AdaptiveColor{Light: "#6B6378", Dark: "#AAA3B5"})
)

// runItem adapts a live run to the bubbles/list Item interface.
type runItem struct {
	run  daemon.LiveRun
	live bool // the daemon is alive, so silence means a stall rather than history
}

// stallAfter is how long an active run may stay silent before it is flagged.
// shortcut: the snapshot only carries LastOutputAt, so a chatty-but-stuck run
// is not detected; upgrade when the daemon exports a last-progress time.
const stallAfter = 2 * time.Minute

// shortAge renders a whole-minute duration as "4m" or "1h5m".
func shortAge(d time.Duration) string {
	return strings.TrimSuffix(strings.Replace(d.String(), "m0s", "m", 1), "0s")
}

// stalledFor reports how long an active run has been silent past stallAfter.
func stalledFor(run daemon.LiveRun, live bool, now time.Time) time.Duration {
	switch run.Phase {
	case "running", "gating", "preparing", "claiming":
	default:
		return 0
	}
	if !live || run.LastOutputAt.IsZero() {
		return 0
	}
	if age := now.Sub(run.LastOutputAt); age >= stallAfter {
		return age.Round(time.Minute)
	}
	return 0
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

	Quit      key.Binding
	Focus     key.Binding
	OpenRun   key.Binding
	Back      key.Binding
	Help      key.Binding
	Filter    key.Binding
	History   key.Binding
	Attention key.Binding
	Commands  key.Binding
	ScrollUp  key.Binding
	ScrollDn  key.Binding
	PageUp    key.Binding
	PageDown  key.Binding
	GoTop     key.Binding
	GoBottom  key.Binding
	ListUp    key.Binding
	ListDown  key.Binding
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
		Commands:  key.NewBinding(key.WithKeys(":", "ctrl+p"), key.WithHelp(":", "commands")),
		Attention: key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "attention")),
		History:   key.NewBinding(key.WithKeys("h"), key.WithHelp("h", "history")),
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
	return []key.Binding{k.OpenRun, k.Commands, k.Attention, k.History, k.Filter, k.ListDown, k.Help, k.Quit}
}

// FullHelp implements help.KeyMap.
func (k keyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{k.ListUp, k.ListDown, k.Focus, k.Filter, k.History, k.Attention},
		{k.ScrollUp, k.ScrollDn, k.PageUp, k.PageDown},
		{k.GoTop, k.GoBottom, k.Help, k.Quit},
	}
}

// Model is the Bubble Tea dashboard model.
type Model struct {
	path           string
	keys           keyMap
	trackerPath    string
	palette        *commandPalette
	commandStatus  string
	commandErr     error
	queueTask      func(string) (string, error)
	queueWorkflows []QueueWorkflow
	queuePending   bool
	showActivity   bool
	readiness      func() ([]tracker.ReadinessRow, error)
	upcoming       []daemon.LiveRun
	readinessErr   error

	sessionContext context.Context
	agentSession   agentSessionMsg

	list    list.Model
	detail  viewport.Model
	help    help.Model
	spinner spinner.Model

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
	showAttention   bool
	loading         bool
	stacked         bool
	split           bool
	colHeader       bool
	previewHeader   bool
	tabsH           int
	bodyH           int
	pins            int
	tableWidth      int
	previewWidth    int
}

// snapshotMsg carries a freshly read live snapshot.
type snapshotMsg struct {
	snapshot daemon.LiveSnapshot
	alive    bool
	err      error
}

// tickMsg triggers the next snapshot read.
type tickMsg time.Time

type readinessMsg struct {
	rows []tracker.ReadinessRow
	err  error
}

func (m *Model) readReadiness(after time.Duration) tea.Cmd {
	read := m.readiness
	if read == nil {
		return nil
	}
	return tea.Tick(after, func(time.Time) tea.Msg {
		rows, err := read()
		return readinessMsg{rows, err}
	})
}

// New creates a dashboard that reads the live snapshot at path.
func New(path string) *Model {
	m := &Model{
		path:    path,
		loading: true,
	}
	m.keys = defaultKeyMap(&m.focus)
	m.list = list.New(nil, rowDelegate{}, 0, 0)
	m.list.SetShowHelp(false)
	m.list.SetShowTitle(false)
	m.list.SetShowStatusBar(false)
	m.list.SetShowPagination(false)
	m.list.SetShowFilter(false) // the table draws its own filter prompt
	m.list.FilterInput.Prompt = "/ "
	m.list.DisableQuitKeybindings()

	m.detail = viewport.New(0, 0)

	m.help = help.New()
	m.help.ShowAll = false
	shortcut := lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#382553", Dark: "#EADFFF"}).Bold(true)
	helpText := lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#51485E", Dark: "#BEB5CB"})
	m.help.Styles.ShortKey, m.help.Styles.FullKey = shortcut, shortcut
	m.help.Styles.ShortDesc, m.help.Styles.FullDesc = helpText, helpText
	m.help.Styles.ShortSeparator, m.help.Styles.FullSeparator = helpText, helpText

	m.spinner = spinner.New()
	m.spinner.Spinner = spinner.Dot
	return m
}

// Init implements tea.Model.
func (m *Model) Init() tea.Cmd {
	if m.path == "" {
		return nil
	}
	return tea.Batch(readSnapshot(m.path), tick(), m.spinner.Tick, m.readReadiness(0))
}

// Update implements tea.Model.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		follow := !m.showHistory && !m.showAttention && m.detail.AtBottom()
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
		if runsChanged || aliveChanged || (m.showHistory && historyChanged) {
			m.setRuns(m.visibleRuns())
			m.refreshDetail(!m.showHistory && !m.showAttention && m.detail.AtBottom())
		}
		return m, nil

	case agentSessionMsg:
		if selected, ok := m.selectedRun(); ok && selected.Thread == msg.thread {
			m.agentSession = msg
			m.setFocus(focusDetail)
			m.refreshDetail(false)
			m.detail.GotoTop()
		}
		return m, nil

	case readinessMsg:
		m.readinessErr = msg.err
		if msg.err == nil {
			upcoming := make([]daemon.LiveRun, 0, len(msg.rows))
			for _, row := range msg.rows {
				phase := "ready"
				reason := row.Reason
				if row.Blocked {
					phase = "blocked"
					var blockers []string
					for _, blocker := range row.Blockers {
						blockers = append(blockers, blocker.ID+" · "+blocker.Title)
					}
					if len(blockers) > 0 {
						reason += "\n\nBlocked by\n" + strings.Join(blockers, "\n")
					}
				}
				upcoming = append(upcoming, daemon.LiveRun{
					Progress: symphony.Progress{IssueID: cleanTaskText(row.ID), Title: strings.Join(strings.Fields(cleanTaskText(row.Title)), " "), Phase: phase},
					Tail:     reason,
				})
			}
			if !slices.Equal(m.upcoming, upcoming) {
				m.upcoming = upcoming
				if !m.showHistory {
					m.setRuns(m.visibleRuns())
					m.refreshDetail(false)
				}
			}
		}
		return m, m.readReadiness(15 * time.Second)

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
	case msg.String() == "1":
		m.setQueueView(false, false)
		return m, nil
	case msg.String() == "2":
		m.setQueueView(false, true)
		return m, nil
	case msg.String() == "3":
		m.setHistory(true)
		return m, nil
	case key.Matches(msg, m.keys.Attention):
		m.setQueueView(false, !m.showAttention)
		return m, nil
	case key.Matches(msg, m.keys.History):
		m.setHistory(!m.showHistory)
		return m, nil
	case key.Matches(msg, m.keys.Help):
		follow := !m.showHistory && !m.showAttention && m.detail.AtBottom()
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

func (m *Model) updateFocused(msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	if m.focus == focusDetail {
		m.detail, cmd = m.detail.Update(msg)
	} else {
		m.list, cmd = m.list.Update(msg)
	}
	return cmd
}

func (m *Model) setHistory(show bool) {
	m.setQueueView(show, false)
}

func (m *Model) setQueueView(history, attention bool) {
	if m.showHistory == history && m.showAttention == attention {
		return
	}
	m.showHistory, m.showAttention = history, attention
	if attention {
		m.keys.Attention.SetHelp("a", "queue")
	} else {
		m.keys.Attention.SetHelp("a", "attention")
	}
	if history {
		m.keys.History.SetHelp("h", "queue")
	} else {
		m.keys.History.SetHelp("h", "history")
	}
	m.setFocus(focusList)
	m.list.ResetFilter()
	m.list.ResetSelected()
	m.selectedID = ""
	m.setRuns(m.visibleRuns())
	m.refreshDetail(!history && !attention)
	if history || attention {
		m.detail.GotoTop()
	}
}

func (m *Model) visibleRuns() []daemon.LiveRun {
	return m.runsFor(m.showHistory, m.showAttention)
}

// runsFor returns the runs of one dashboard view (queue, attention or history).
func (m *Model) runsFor(showHistory, showAttention bool) []daemon.LiveRun {
	if !showHistory {
		if !showAttention && len(m.upcoming) == 0 {
			return m.snapshot.Runs
		}
		runs := make([]daemon.LiveRun, 0, len(m.snapshot.Runs)+len(m.upcoming))
		seen := make(map[string]bool, len(m.snapshot.Runs))
		for _, run := range m.snapshot.Runs {
			seen[run.IssueID] = true
			if !showAttention || needsAttention(run.Phase) {
				runs = append(runs, run)
			}
		}
		for _, run := range m.upcoming {
			if !seen[run.IssueID] && (!showAttention || run.Phase == "blocked") {
				runs = append(runs, run)
			}
		}
		return runs
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

func needsAttention(phase string) bool {
	switch phase {
	case "retry", "deferred", "interrupted", "stopped: gate escalation", "stopped: gate red":
		return true
	}
	return false
}

// setRuns replaces the list contents while preserving the selected issue.
func (m *Model) setRuns(runs []daemon.LiveRun) {
	items := make([]list.Item, len(runs))
	for i, run := range runs {
		items[i] = runItem{run: run, live: m.alive}
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
		m.refreshDetail(!m.showHistory && !m.showAttention && m.detail.AtBottom())
		if m.showHistory || m.showAttention {
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
	content := detailBody(run, m.detail.Width, m.pins)
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
	if m.loading && m.snapshot.UpdatedAt.IsZero() && len(m.upcoming) == 0 {
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
	if m.tabsH == 0 {
		return lipgloss.JoinVertical(lipgloss.Left, header, m.renderBody(), footer)
	}
	return lipgloss.JoinVertical(lipgloss.Left, header, m.renderTabs(), m.renderBody(), footer)
}

// detailContent formats one run for the detail viewport.
func cleanTaskText(s string) string {
	return strings.Map(func(r rune) rune {
		if r != '\n' && (unicode.IsControl(r) || unicode.In(r, unicode.Cf)) {
			return -1
		}
		return r
	}, ansi.Strip(s))
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
			row("Issue:", run.IssueID),
			row("Phase:", phaseLabel(run.Phase)),
			row("Attempt:", fmt.Sprintf("%d", run.Attempt)),
			row("Turn:", fmt.Sprintf("%d", run.Turn)),
		}
		if run.Workflow != "" {
			left = append(left, row("Workflow:", run.Workflow))
		}
	}
	if run.Thread != "" && pins < 2 {
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
	case phase == "ready":
		return "◇", greenStyle
	case phase == "blocked":
		return "!", retryStyle
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
	return ansi.Truncate(s, limit, "…")
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
func Run(ctx context.Context, path, trackerPath string, queue func(string) (string, error), readiness func() ([]tracker.ReadinessRow, error), workflows ...QueueWorkflow) error {
	model := New(path)
	model.trackerPath = trackerPath
	model.queueTask = queue
	model.readiness = readiness
	model.sessionContext = ctx
	model.queueWorkflows = workflows
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
