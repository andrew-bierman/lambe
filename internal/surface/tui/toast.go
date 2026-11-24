package tui

import (
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// toastTTL is how long a transient message stays in the footer.
const toastTTL = 4 * time.Second

// toast is a transient message shown in the footer, never in the health header.
type toast struct {
	text  string
	err   bool
	until time.Time // zero means it stays until replaced (an operation in flight)
	seq   int
}

// toastExpiredMsg clears the toast with the same seq; older ones are ignored.
type toastExpiredMsg struct{ seq int }

// showToast replaces the current toast. Pending toasts never expire by
// themselves; the command that finishes the operation replaces them.
func (m *Model) showToast(text string, isErr, pending bool) tea.Cmd {
	text = strings.Join(strings.Fields(ansi.Strip(text)), " ")
	m.toastSeq++
	t := toast{text: text, err: isErr, seq: m.toastSeq}
	if pending {
		m.toast = t
		return nil
	}
	t.until = m.clock().Add(toastTTL)
	m.toast = t
	return tea.Tick(toastTTL, func(time.Time) tea.Msg { return toastExpiredMsg{t.seq} })
}

// activeToast reports the toast to draw, if one is live.
func (m *Model) activeToast() (toast, bool) {
	t := m.toast
	if t.text == "" || (!t.until.IsZero() && !m.clock().Before(t.until)) {
		return toast{}, false
	}
	return t, true
}

func (m *Model) renderToast(width int) string {
	t, _ := m.activeToast()
	style := valueStyle
	if t.err {
		style = errorStyle
	}
	return ansi.Truncate(style.Render(t.text), width, "…")
}
