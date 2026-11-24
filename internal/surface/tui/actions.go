package tui

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// maxCopyBytes caps the OSC 52 payload; many terminals drop sequences whose
// base64 exceeds roughly 75 KB, so a long output is copied from its tail.
const maxCopyBytes = 48 * 1024

// yankTimeout is how long the key after y is awaited before the prefix lapses.
const yankTimeout = 3 * time.Second

// yankExpiredMsg ends the pending y prefix with the same seq; older ones are ignored.
type yankExpiredMsg struct{ seq int }

// actions is everything the copy and open keys do outside the TUI. Tests
// inject a fake so no clipboard write, process or editor is ever started.
type actions interface {
	// Copy puts text on the system clipboard.
	Copy(text string) error
	// Open shows a directory in the desktop file manager.
	Open(dir string) error
	// Edit opens dir in the user's editor, suspending the TUI meanwhile.
	Edit(dir string) tea.Cmd
}

// systemActions is the real implementation, used only after a key press.
type systemActions struct {
	out    io.Writer           // terminal for OSC 52; os.Stdout in production
	getenv func(string) string // environment lookup; os.Getenv in production
}

func (a systemActions) Copy(text string) error {
	if len(text) > maxCopyBytes {
		text = text[len(text)-maxCopyBytes:]
	}
	_, err := io.WriteString(a.out, a.osc52(text))
	return err
}

// osc52 is the clipboard-write sequence for text. Inside tmux ($TMUX set) it
// is wrapped in tmux's DCS passthrough, with every ESC doubled, so tmux
// forwards it to the outer terminal; that needs `set -g allow-passthrough on`
// (tmux 3.3+). shortcut: GNU screen's passthrough is not wrapped.
func (a systemActions) osc52(text string) string {
	seq := "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(text)) + "\a"
	getenv := a.getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	if getenv("TMUX") == "" {
		return seq
	}
	return "\x1bPtmux;" + strings.ReplaceAll(seq, "\x1b", "\x1b\x1b") + "\x1b\\"
}

func (systemActions) Open(dir string) error {
	opener := "xdg-open"
	if runtime.GOOS == "darwin" {
		opener = "open"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// The path is the only argument and is passed as one argv element.
	return exec.CommandContext(ctx, opener, dir).Run() // #nosec G204 -- fixed opener, user-triggered, single path argument.
}

func (systemActions) Edit(dir string) tea.Cmd {
	fields := strings.Fields(os.Getenv("EDITOR"))
	if len(fields) == 0 {
		return func() tea.Msg { return commandResult{err: errors.New("set $EDITOR to open a workspace in an editor")} }
	}
	cmd := exec.Command(fields[0], append(fields[1:], dir)...) // #nosec G204 G702 -- the user's own $EDITOR, started by a key press.
	cmd.Dir = dir
	return tea.ExecProcess(cmd, func(err error) tea.Msg { return commandResult{err: err} })
}

// workspaceDir validates a run's workspace before it is handed to an opener.
func workspaceDir(path string) (string, error) {
	if path == "" {
		return "", errors.New("this run has no workspace")
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("workspace is not an absolute path: %s", path)
	}
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		return "", fmt.Errorf("workspace is not available: %s", path)
	}
	return path, nil
}

// copyCmd copies text and reports what was copied as a toast.
func (m *Model) copyCmd(what, text string) tea.Cmd {
	if strings.TrimSpace(text) == "" {
		return m.showToast("Nothing to copy: "+what+" is empty", true, false)
	}
	a := m.acts
	return func() tea.Msg {
		if err := a.Copy(text); err != nil {
			return commandResult{err: fmt.Errorf("copy failed: %w", err)}
		}
		return commandResult{status: "Copied " + what}
	}
}

func (m *Model) copyID() tea.Cmd {
	run, ok := m.selectedRun()
	if !ok {
		return m.showToast("No run selected", true, false)
	}
	return m.copyCmd(run.IssueID, run.IssueID)
}

func (m *Model) copyWorkspace() tea.Cmd {
	run, ok := m.selectedRun()
	if !ok {
		return m.showToast("No run selected", true, false)
	}
	if run.Workspace == "" {
		return m.showToast(run.IssueID+" has no workspace", true, false)
	}
	return m.copyCmd("workspace path", run.Workspace)
}

func (m *Model) copyOutput() tea.Cmd {
	run, ok := m.selectedRun()
	if !ok {
		return m.showToast("No run selected", true, false)
	}
	return m.copyCmd("output of "+run.IssueID, cleanTaskText(run.Tail))
}

func (m *Model) openWorkspace(editor bool) tea.Cmd {
	run, ok := m.selectedRun()
	if !ok {
		return m.showToast("No run selected", true, false)
	}
	dir, err := workspaceDir(run.Workspace)
	if err != nil {
		return m.showToast(err.Error(), true, false)
	}
	a := m.acts
	if editor {
		return a.Edit(dir)
	}
	return func() tea.Msg {
		if err := a.Open(dir); err != nil {
			return commandResult{err: fmt.Errorf("open failed: %w", err)}
		}
		return commandResult{status: "Opened " + run.IssueID + " workspace"}
	}
}

// handleYank consumes the key after y: i or y copies the issue ID, w the
// workspace path, o the last output. Anything else cancels.
func (m *Model) handleYank(msg tea.KeyMsg) tea.Cmd {
	m.yank = false
	m.toast = toast{}
	switch msg.String() {
	case "y", "i":
		return m.copyID()
	case "w":
		return m.copyWorkspace()
	case "o":
		return m.copyOutput()
	}
	return nil
}
