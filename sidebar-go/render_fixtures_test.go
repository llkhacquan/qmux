package main

import (
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// Force truecolor in tests so styling actually emits ANSI codes — without
// this, lipgloss detects the test runner's stdout is not a TTY and strips
// every Render() call back to plain text. That hides colour-only bugs
// behind byte-equal assertions on rune content.
func init() { lipgloss.SetColorProfile(termenv.TrueColor) }

// buildFixtureModel constructs a teaModel populated with rows + state
// covering the major decorator branches in one frame: active idle,
// needs-input (blink), running (rainbow comet), done (yellow comet),
// cursor-only.
func buildFixtureModel() *teaModel {
	rows := []Row{
		{Kind: kindSession, Text: " session-A "},
		{Kind: kindBorderTop, Text: " card-1 ", PaneID: ""},
		{Kind: kindIntent, Text: "intent-1", PaneID: "%1", Session: "s1", Window: "1"},
		{Kind: kindLocation, Text: " ~/work/foo", PaneID: "%1"},
		{Kind: kindGit, Text: " main · clean", PaneID: "%1"},
		{Kind: kindPreview, Text: " > running test", PaneID: "%1"},
		{Kind: kindBorderBot, PaneID: ""},

		{Kind: kindBorderTop, Text: " card-2 "},
		{Kind: kindIntent, Text: "intent-2-needs-input", PaneID: "%2", Status: "needs-input"},
		{Kind: kindLocation, Text: " ~/work/bar", PaneID: "%2", Status: "needs-input"},
		{Kind: kindBorderBot},

		{Kind: kindBorderTop, Text: " card-3 "},
		{Kind: kindIntent, Text: "intent-3-working", PaneID: "%3", Status: "running", Verb: "Crafting…"},
		{Kind: kindLocation, Text: " ~/work/baz", PaneID: "%3", Status: "running"},
		{Kind: kindBorderBot},

		{Kind: kindBorderTop, Text: " card-4 "},
		{Kind: kindIntent, Text: "intent-4-done", PaneID: "%4", Status: "done"},
		{Kind: kindLocation, Text: " ~/work/qux", PaneID: "%4", Status: "done"},
		{Kind: kindBorderBot},

		{Kind: kindSpacer},

		{Kind: kindBorderTop, Text: " card-5-cursor "},
		{Kind: kindIntent, Text: "intent-5-cursor-only", PaneID: "%5"},
		{Kind: kindLocation, Text: " ~/work/cur", PaneID: "%5"},
		{Kind: kindGit, Text: " feat · 2 changed", PaneID: "%5"},
		{Kind: kindBorderBot},
	}
	m := newTeaModel()
	m.rows = rows
	m.paneRows = paneRowsFor(rows)
	m.width = 40
	m.activePaneID = "%1"
	m.cursorPaneID = "%5"
	m.cursorVisible = true
	m.lastActivePaneID = "%4"
	m.unreadPanes = map[string]bool{"%4": true}
	m.searchMatches = map[int]bool{3: true}
	m.spinFrame = 3
	m.blinkOn = true
	return &m
}
