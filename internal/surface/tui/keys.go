package tui

import (
	"github.com/charmbracelet/bubbles/key"
)

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
