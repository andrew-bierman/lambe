package tui

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/andrew-bierman/lambe/internal/daemon"
	"github.com/andrew-bierman/lambe/internal/symphony"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/sahilm/fuzzy"
)

type paletteCommand struct {
	label string
	desc  string // searched along with the label and shown for the selected row
	hint  string // the key that does the same thing, shown at the right edge
	run   func() tea.Cmd
}

// QueueWorkflow describes an explicitly selectable workflow in the queue palette.
type QueueWorkflow struct {
	Name    string
	Preview string
	Queue   func(string) (string, error)
}

type commandPalette struct {
	input     textinput.Model
	commands  []paletteCommand
	selected  int
	queueForm bool
	workflow  QueueWorkflow
}

type commandResult struct {
	err    error
	status string
	queued bool
}

func (p *commandPalette) matches() []paletteCommand {
	query := strings.TrimSpace(p.input.Value())
	if query == "" {
		return p.commands
	}
	labels := make([]string, len(p.commands))
	for i, c := range p.commands {
		labels[i] = c.label
	}
	var matches []paletteCommand
	seen := make(map[int]bool)
	for _, r := range fuzzy.Find(query, labels) { // best label match first
		matches = append(matches, p.commands[r.Index])
		seen[r.Index] = true
	}
	for i, c := range p.commands { // then commands whose description mentions the query
		if !seen[i] && strings.Contains(strings.ToLower(c.desc), strings.ToLower(query)) {
			matches = append(matches, c)
		}
	}
	return matches
}

func (m *Model) openPalette() tea.Cmd {
	m.commandErr = nil
	m.commandStatus = ""
	p := &commandPalette{input: textinput.New()}
	p.input.Prompt = "› "
	p.input.Placeholder = "Search commands"
	p.commands = m.paletteCommands(p)
	m.palette = p
	return p.input.Focus()
}

// paletteCommands builds the command table for the current selection. The key
// hints are checked against the keyMap by a test.
func (m *Model) paletteCommands(p *commandPalette) []paletteCommand {
	cmds := []paletteCommand{
		{"Show run queue", "all runs in flight", "1", func() tea.Cmd { m.setHistory(false); m.setFocus(focusList); return nil }},
		{"Show runs needing attention", "retries, deferred, interrupted, failed gates", "2", func() tea.Cmd { m.setQueueView(false, true); m.setFocus(focusList); return nil }},
		{"Show attempt history", "finished attempts", "3", func() tea.Cmd { m.setHistory(true); m.setFocus(focusList); return nil }},
		{"Show activity chart", "breakdown of runs and outcomes", "", func() tea.Cmd { m.showActivity = true; return nil }},
		{"Show key help", "every key for this view", "?", func() tea.Cmd { m.openHelp(); return nil }},
	}
	if run, ok := m.selectedRun(); ok {
		cmds = append(cmds,
			paletteCommand{"Inspect selected run", "focus the detail pane", "enter", func() tea.Cmd { m.setFocus(focusDetail); return nil }},
			paletteCommand{"Jump to latest output", "scroll details to the bottom", "G", func() tea.Cmd {
				m.setFocus(focusDetail)
				m.detail.GotoBottom()
				return nil
			}},
		)
		if m.sessionContext != nil && run.Thread != "" {
			cmds = append(cmds, paletteCommand{"Show agent session details", "provider and workspace of the agent session", "", func() tea.Cmd {
				return readAgentSession(m.sessionContext, run.Thread)
			}})
		}
		if m.path != "" && run.Workspace != "" {
			cmds = append(cmds, paletteCommand{"Open run workspace shell", "a shell in the run workspace", "", func() tea.Cmd {
				return openDirectoryShell(run.Workspace)
			}})
		}
	}
	if m.trackerPath != "" {
		cmds = append(cmds, paletteCommand{"Open Beads tracker shell", "a shell in the tracker directory", "", func() tea.Cmd {
			return openDirectoryShell(m.trackerPath)
		}})
	}
	if !m.queuePending {
		choices := m.queueWorkflows
		if len(choices) == 0 && (m.queueTask != nil || m.path == "") {
			choices = []QueueWorkflow{{Queue: m.queueTask}}
		}
		for _, choice := range choices {
			label := "Queue an agent task"
			if choice.Name != "" {
				label = "Queue task · " + choice.Name
			}
			cmds = append(cmds, paletteCommand{label, "add a task for the daemon to run", "", func() tea.Cmd {
				p.queueForm = true
				p.workflow = choice
				p.input.SetValue("")
				p.input.Placeholder = "Task title"
				p.input.CharLimit = 160
				m.palette = p
				return nil
			}})
		}
	}
	return cmds
}

func (m *Model) updatePalette(msg tea.KeyMsg) tea.Cmd {
	p := m.palette
	if p.queueForm && msg.Type == tea.KeyEnter {
		title := strings.TrimSpace(p.input.Value())
		if title == "" {
			return nil
		}
		m.palette = nil
		return m.submitTask(title, p.workflow.Queue)
	}
	switch msg.Type {
	case tea.KeyCtrlC:
		return tea.Quit
	case tea.KeyEsc:
		m.palette = nil
	case tea.KeyEnter:
		matches := p.matches()
		if len(matches) > 0 {
			m.palette = nil
			return matches[p.selected].run()
		}
	case tea.KeyUp:
		p.selected = max(0, p.selected-1)
	case tea.KeyDown:
		p.selected = min(p.selected+1, max(0, len(p.matches())-1))
	default:
		before := p.input.Value()
		var cmd tea.Cmd
		p.input, cmd = p.input.Update(msg)
		if before != p.input.Value() {
			p.selected = 0
		}
		return cmd
	}
	return nil
}

func (m *Model) renderPalette() string {
	p := m.palette
	width := max(1, min(56, m.width-4))
	frame := m.width >= 12 && m.height >= 6
	p.input.Width = max(1, width-3)
	lines := []string{
		headerStyle.Width(width).MaxWidth(width).Render("Commands"),
		p.input.View(),
	}
	matches := p.matches()
	if len(matches) == 0 {
		lines = append(lines, dimStyle.Render("No matching commands"))
	}
	rows := max(1, m.height-5)
	start := max(0, p.selected-rows+1)
	for i, command := range matches[start:min(len(matches), start+rows)] {
		label := command.label
		if command.hint != "" {
			pad := max(1, width-4-lipgloss.Width(label)-lipgloss.Width(command.hint))
			label += strings.Repeat(" ", pad) + command.hint
		}
		if start+i == p.selected {
			lines = append(lines, headerStyle.Width(width).MaxWidth(width).Render("› "+label))
		} else {
			lines = append(lines, "  "+label)
		}
	}
	if p.selected < len(matches) && matches[p.selected].desc != "" && m.height >= 10 {
		lines = append(lines, dimStyle.Render(matches[p.selected].desc))
	}
	lines = append(lines, dimStyle.Render("↑↓ select   Enter run   Esc close"))
	if p.queueForm {
		title := "Queue an agent task"
		if p.workflow.Name != "" {
			title = "Queue task · " + p.workflow.Name
		}
		lines = []string{headerStyle.Width(width).MaxWidth(width).Render(title), p.input.View()}
		if p.workflow.Preview != "" && m.height >= 8 {
			lines = append(lines, dimStyle.Render(p.workflow.Preview))
		}
		lines = append(lines, dimStyle.Render("Enter queue   Esc cancel"))
	}
	return m.centerPanel(lines, width, frame)
}

func (m *Model) centerPanel(lines []string, width int, frame bool) string {
	parts := strings.Split(strings.Join(lines, "\n"), "\n") // lines may hold embedded newlines
	for i, line := range parts {
		parts[i] = ansi.Truncate(line, width, "…")
	}
	content := strings.Join(parts, "\n")
	if frame {
		content = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1).Width(width + 2).Render(content)
	}
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center,
		lipgloss.NewStyle().MaxHeight(m.height).Render(content))
}

func (m *Model) submitTask(title string, queue func(string) (string, error)) tea.Cmd {
	if m.path == "" {
		id := fmt.Sprintf("demo-%d", 42+len(m.snapshot.Runs))
		m.snapshot.Runs = append(m.snapshot.Runs, daemon.LiveRun{
			Progress:  symphony.Progress{IssueID: id, Title: title, Phase: "queued"},
			UpdatedAt: time.Now(),
		})
		m.setHistory(false)
		m.setRuns(m.snapshot.Runs)
		m.commandStatus = "Queued " + id
		return nil
	}
	m.commandStatus = "Queuing…"
	m.queuePending = true
	return func() tea.Msg {
		id, err := queue(title)
		if err != nil {
			return commandResult{err: err, queued: true}
		}
		return commandResult{status: "Queued " + id, queued: true}
	}
}

func openDirectoryShell(path string) tea.Cmd {
	if !filepath.IsAbs(path) {
		return func() tea.Msg { return commandResult{err: fmt.Errorf("shell requires an absolute path: %s", path)} }
	}
	shell := cmp.Or(os.Getenv("SHELL"), "/bin/sh")
	cmd := exec.Command(shell) // #nosec G204 G702 -- launch the user's configured shell without interpolating arguments.
	cmd.Dir = path
	return tea.ExecProcess(cmd, func(err error) tea.Msg { return commandResult{err: err} })
}

type agentSessionMsg struct {
	thread  string
	details string
	err     error
}

func readAgentSession(ctx context.Context, thread string) tea.Cmd {
	return func() tea.Msg {
		result := agentSessionMsg{thread: thread}
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "showagent", "list", "--json")
		cmd.WaitDelay = time.Second
		output, err := cmd.Output()
		if err != nil {
			result.err = fmt.Errorf("could not list Show Agent sessions: %w", err)
			return result
		}
		var sessions []struct{ ID, Provider, Workspace, Updated string }
		if err := json.Unmarshal(output, &sessions); err != nil {
			result.err = fmt.Errorf("decode Show Agent session list: %w", err)
			return result
		}
		for _, session := range sessions {
			if session.ID != thread {
				continue
			}
			if result.details != "" {
				result.err = errors.New("multiple Show Agent sessions match this run")
				return result
			}
			result.details = cleanTaskText(fmt.Sprintf("Agent session\nProvider: %s\nUpdated: %s\nWorkspace: %s", session.Provider, session.Updated, session.Workspace))
		}
		if result.details == "" {
			result.err = errors.New("no local Show Agent session matches this run")
		}
		return result
	}
}
