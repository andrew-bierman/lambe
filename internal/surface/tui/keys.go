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

	Quit    key.Binding
	Focus   key.Binding
	OpenRun key.Binding
	Back    key.Binding
	Help    key.Binding
	Filter  key.Binding
	// FilterApply and FilterCancel only apply while the filter prompt is open.
	FilterApply  key.Binding
	FilterCancel key.Binding
	Views        key.Binding
	Close        key.Binding
	History      key.Binding
	Attention    key.Binding
	Commands     key.Binding
	ScrollUp     key.Binding
	ScrollDn     key.Binding
	PageUp       key.Binding
	PageDown     key.Binding
	GoTop        key.Binding
	GoBottom     key.Binding
	ListUp       key.Binding
	ListDown     key.Binding
}

func defaultKeyMap(focus *focusArea) keyMap {
	return keyMap{
		focus: focus,
		Quit: key.NewBinding(
			key.WithKeys("q", "ctrl+c"),
			key.WithHelp("q", "back out of details, then quit"),
		),
		Focus: key.NewBinding(
			key.WithKeys("tab"),
			key.WithHelp("tab", "switch list / details"),
		),
		OpenRun: key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "open details")),
		Back:    key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "back to runs")),
		Help: key.NewBinding(
			key.WithKeys("?"),
			key.WithHelp("?", "this help"),
		),
		FilterApply:  key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "apply filter")),
		FilterCancel: key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "clear filter")),
		Views:        key.NewBinding(key.WithKeys("1", "2", "3"), key.WithHelp("1-3", "queue / attn / history")),
		Close:        key.NewBinding(key.WithKeys("esc", "q"), key.WithHelp("esc", "close help, palette or chart")),
		Commands:     key.NewBinding(key.WithKeys(":", "ctrl+p"), key.WithHelp(": / ctrl+p", "command palette")),
		Attention:    key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "attention view")),
		History:      key.NewBinding(key.WithKeys("h"), key.WithHelp("h", "history view")),
		Filter: key.NewBinding(
			key.WithKeys("/"),
			key.WithHelp("/", "filter runs"),
		),
		ScrollUp: key.NewBinding(
			key.WithKeys("up", "k"),
			key.WithHelp("k/↑", "scroll up"),
		),
		ScrollDn: key.NewBinding(
			key.WithKeys("down", "j"),
			key.WithHelp("j/↓", "scroll down"),
		),
		PageUp: key.NewBinding(
			key.WithKeys("pgup", "K", "u", "ctrl+u"),
			key.WithHelp("u/pgup", "half page up"),
		),
		PageDown: key.NewBinding(
			key.WithKeys("pgdown", "J", "d", "ctrl+d"),
			key.WithHelp("d/pgdn", "half page down"),
		),
		GoTop: key.NewBinding(
			key.WithKeys("home", "g"),
			key.WithHelp("g", "top"),
		),
		GoBottom: key.NewBinding(
			key.WithKeys("end", "G"),
			key.WithHelp("G", "bottom / latest output"),
		),
		ListUp: key.NewBinding(
			key.WithKeys("up", "k"),
			key.WithHelp("k/↑", "previous run"),
		),
		ListDown: key.NewBinding(
			key.WithKeys("down", "j"),
			key.WithHelp("j/↓", "next run"),
		),
	}
}
