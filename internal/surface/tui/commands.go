package tui

import (
	"cmp"
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
)

type paletteCommand struct {
	label string
	run   func() tea.Cmd
}

type commandPalette struct {
	input     textinput.Model
	commands  []paletteCommand
	selected  int
	queueForm bool
}

type commandResult struct {
	err    error
	status string
	queued bool
}

func (p *commandPalette) matches() []paletteCommand {
	query := strings.ToLower(strings.TrimSpace(p.input.Value()))
	var matches []paletteCommand
	for _, command := range p.commands {
		if strings.Contains(strings.ToLower(command.label), query) {
			matches = append(matches, command)
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
	p.commands = []paletteCommand{
		{"Show run queue", func() tea.Cmd { m.setHistory(false); m.setFocus(focusList); return nil }},
		{"Show attempt history", func() tea.Cmd { m.setHistory(true); m.setFocus(focusList); return nil }},
		{"Show activity chart", func() tea.Cmd { m.showActivity = true; return nil }},
	}
	if run, ok := m.selectedRun(); ok {
		p.commands = append(p.commands,
			paletteCommand{"Inspect selected run", func() tea.Cmd { m.setFocus(focusDetail); return nil }},
			paletteCommand{"Jump to latest output", func() tea.Cmd {
				m.setFocus(focusDetail)
				m.detail.GotoBottom()
				return nil
			}},
		)
		if m.path != "" && run.Workspace != "" {
			p.commands = append(p.commands, paletteCommand{"Open run workspace shell", func() tea.Cmd {
				return openDirectoryShell(run.Workspace)
			}})
		}
	}
	if m.trackerPath != "" {
		p.commands = append(p.commands, paletteCommand{"Open Beads tracker shell", func() tea.Cmd {
			return openDirectoryShell(m.trackerPath)
		}})
	}
	if !m.queuePending && (m.queueTask != nil || m.path == "") {
		p.commands = append(p.commands, paletteCommand{"Queue an agent task", func() tea.Cmd {
			p.queueForm = true
			p.input.SetValue("")
			p.input.Placeholder = "Task title"
			p.input.CharLimit = 160
			m.palette = p
			return nil
		}})
	}
	m.palette = p
	return p.input.Focus()
}

func (m *Model) updatePalette(msg tea.KeyMsg) tea.Cmd {
	p := m.palette
	if p.queueForm && msg.Type == tea.KeyEnter {
		title := strings.TrimSpace(p.input.Value())
		if title == "" {
			return nil
		}
		m.palette = nil
		return m.submitTask(title)
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
		if start+i == p.selected {
			lines = append(lines, headerStyle.Width(width).MaxWidth(width).Render("› "+command.label))
		} else {
			lines = append(lines, "  "+command.label)
		}
	}
	lines = append(lines, dimStyle.Render("↑↓ select   Enter run   Esc close"))
	if p.queueForm {
		lines = []string{
			headerStyle.Width(width).MaxWidth(width).Render("Queue an agent task"),
			p.input.View(),
			dimStyle.Render("Enter queue   Esc cancel"),
		}
	}
	return m.centerPanel(lines, width, frame)
}

func (m *Model) centerPanel(lines []string, width int, frame bool) string {
	for i, line := range lines {
		lines[i] = ansi.Truncate(line, width, "…")
	}
	content := strings.Join(lines, "\n")
	if frame {
		content = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1).Width(width + 2).Render(content)
	}
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center,
		lipgloss.NewStyle().MaxHeight(m.height).Render(content))
}

func (m *Model) submitTask(title string) tea.Cmd {
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
		id, err := m.queueTask(title)
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
