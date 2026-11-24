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

	"github.com/andrew-bierman/lambe/internal/daemon"
	"github.com/andrew-bierman/lambe/internal/symphony"
	"github.com/andrew-bierman/lambe/internal/tracker"
)

// runItem adapts a live run to the bubbles/list Item interface.
type runItem struct {
	run  daemon.LiveRun
	live bool // the daemon is alive, so silence means a stall rather than history
	// attempts is how many attempts of the issue the history row stands for;
	// zero outside the history view.
	attempts int
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

	list        list.Model
	filterTerms []filterTerm // parsed active filter, for match highlighting
	detail      viewport.Model
	help        help.Model
	spinner     spinner.Model

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
	m.list.Filter = filterRuns
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
	m.list.ResetSelected()
	m.selectedID = ""
	m.setRuns(m.visibleRuns())
	m.refreshDetail(!history && !attention)
	if history || attention {
		m.detail.GotoTop()
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

func truncate(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	return ansi.Truncate(s, limit, "…")
}
