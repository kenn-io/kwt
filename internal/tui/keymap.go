package tui

import "charm.land/bubbles/v2/key"

type keyMap struct {
	Up       key.Binding
	Down     key.Binding
	Top      key.Binding
	Bottom   key.Binding
	Open     key.Binding
	Layout   key.Binding
	New      key.Binding
	Existing key.Binding
	Delete   key.Binding
	Sync     key.Binding
	Shell    key.Binding
	Kill     key.Binding
	Switch   key.Binding
	Project  key.Binding
	Filter   key.Binding
	Refresh  key.Binding
	Help     key.Binding
	Cancel   key.Binding
	Quit     key.Binding
	Yes      key.Binding
	No       key.Binding
}

func newKeyMap() keyMap {
	return keyMap{
		Up:       key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/k", "up")),
		Down:     key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "down")),
		Top:      key.NewBinding(key.WithKeys("g"), key.WithHelp("g", "top")),
		Bottom:   key.NewBinding(key.WithKeys("G"), key.WithHelp("G", "bottom")),
		Open:     key.NewBinding(key.WithKeys("enter"), key.WithHelp("↵", "attach")),
		Layout:   key.NewBinding(key.WithKeys("L"), key.WithHelp("L", "layout")),
		New:      key.NewBinding(key.WithKeys("n"), key.WithHelp("n", "new")),
		Existing: key.NewBinding(key.WithKeys("b"), key.WithHelp("b", "branch")),
		Delete:   key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "delete")),
		Sync:     key.NewBinding(key.WithKeys("s"), key.WithHelp("s", "sync")),
		Shell:    key.NewBinding(key.WithKeys("c"), key.WithHelp("c", "shell")),
		Kill:     key.NewBinding(key.WithKeys("K"), key.WithHelp("K", "kill-ws")),
		Switch:   key.NewBinding(key.WithKeys("P"), key.WithHelp("P", "project")),
		Project:  key.NewBinding(key.WithKeys("p"), key.WithHelp("p", "filter")),
		Filter:   key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "search")),
		Refresh:  key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "refresh")),
		Help:     key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
		Cancel:   key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "cancel")),
		Quit:     key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
		Yes:      key.NewBinding(key.WithKeys("y", "Y"), key.WithHelp("y", "yes")),
		No:       key.NewBinding(key.WithKeys("n", "N"), key.WithHelp("n", "no")),
	}
}
