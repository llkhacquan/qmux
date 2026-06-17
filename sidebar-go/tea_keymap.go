package main

import "github.com/charmbracelet/bubbles/key"

// Key bindings for the sidebar. Centralized so a help overlay (future phase)
// can list them and so user-overridable bindings (later) only touch one map.
type keyMap struct {
	Up        key.Binding
	Down      key.Binding
	Top       key.Binding // gg
	Bottom    key.Binding // G
	HalfUp    key.Binding
	HalfDown  key.Binding
	Enter     key.Binding
	Quit      key.Binding
	FocusMain key.Binding
	Search    key.Binding
	NextMatch key.Binding
	PrevMatch key.Binding
	// SwitchLast toggles to the last-active Claude pane. Bound to a reserved
	// key (not a human-friendly one) because it's driven by the
	// sidebar-switch-last script send-keys'ing it in — the fork-free path for
	// leader+leader / prefix+Tab. Humans never press it directly.
	SwitchLast key.Binding
}

func defaultKeyMap() keyMap {
	return keyMap{
		Up:        key.NewBinding(key.WithKeys("k", "up")),
		Down:      key.NewBinding(key.WithKeys("j", "down")),
		Top:       key.NewBinding(key.WithKeys("g")), // gg handled via pending state
		Bottom:    key.NewBinding(key.WithKeys("G")),
		HalfUp:    key.NewBinding(key.WithKeys("ctrl+u")),
		HalfDown:  key.NewBinding(key.WithKeys("ctrl+d")),
		Enter:     key.NewBinding(key.WithKeys("enter")),
		Quit:      key.NewBinding(key.WithKeys("q", "ctrl+c")),
		FocusMain: key.NewBinding(key.WithKeys("ctrl+l", "esc")),
		Search:    key.NewBinding(key.WithKeys("/")),
		NextMatch: key.NewBinding(key.WithKeys("n")),
		PrevMatch: key.NewBinding(key.WithKeys("N")),
		// Backtick: reserved channel for the sidebar-switch-last script.
		SwitchLast: key.NewBinding(key.WithKeys("`")),
	}
}
