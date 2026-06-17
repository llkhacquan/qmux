package main

import (
	"os/exec"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// Helpers carried over from the deleted tcell/render path because the
// bubbletea View() and the dump-render CLI path still need them. Live here
// so render.go / render_beauty.go / input.go can be deleted cleanly.

// Available spinner glyph sets. The active one is picked by
// selectedSpinnerStyle below — hardcoded const, flip + rebuild to switch.
// Old designs are kept so tweaks are reversible without git surgery and
// future styles can drop in as additional indices.
//
//	1 — snowflake/sparkle : ✦✧✶✷✸✹✺✻ (original sidebar look; pairs with
//	                                   the rainbow label shimmer)
//	2 — braille dots      : ⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏ (matches Claude Code's own
//	                                       in-pane spinner; thinner and
//	                                       more "active" looking)
var spinnerStyles = [][]rune{
	nil, // index 0 unused so const values match human-readable IDs
	{'✦', '✧', '✶', '✷', '✸', '✹', '✺', '✻'},
	{'⠋', '⠙', '⠹', '⠸', '⠼', '⠴', '⠦', '⠧', '⠇', '⠏'},
}

// Per-pane state picks the glyph set: focused card uses the snowflake
// sparkle so the eye lands on it; background cards use the thinner
// braille dots that match Claude Code's own in-pane spinner. Both
// share the same spinFrame counter so the cadence stays in sync.
var (
	spinnerFramesActive   = spinnerStyles[1]
	spinnerFramesInactive = spinnerStyles[2]
)

// spinnerFrames is the legacy alias still consumed by the tmux
// window-name spinner (commands.go cmdWindowName) and any other
// non-pane caller. Points at the inactive set so the per-window
// indicator visually matches Claude's own spinner.
var spinnerFrames = spinnerFramesInactive

// beautyBorder is the rounded border glyph table — sourced from lipgloss so
// future style swaps (thick / double / catppuccin) only edit one symbol.
var beautyBorder = lipgloss.RoundedBorder()

// beautyRunes returns the canonical (TL, TR, BL, BR, horiz, vert) tuple
// callers have used in this codebase since the tcell beauty renderer.
func beautyRunes() (topLeft, topRight, botLeft, botRight, horiz, vert rune) {
	return firstRune(beautyBorder.TopLeft),
		firstRune(beautyBorder.TopRight),
		firstRune(beautyBorder.BottomLeft),
		firstRune(beautyBorder.BottomRight),
		firstRune(beautyBorder.Top),
		firstRune(beautyBorder.Left)
}

// firstRune returns the first rune of s, or ' ' if s is empty.
func firstRune(s string) rune {
	for _, r := range s {
		return r
	}
	return ' '
}

// borderRunes is the full glyph table for one box-drawing weight — corners,
// edges, walls, T-junctions. Three preset weights live below; activeBorderSet
// picks which one paints the active card's frame, so swapping styles is a
// single-line change rather than touching every call site.
type borderRunes struct {
	TL, TR, BL, BR rune // corners
	Horiz, Vert    rune // edges
	MidL, MidR     rune // ├-style T-junctions on the kindBorderMid row
}

// thinBorderSet — the default rounded glyphs (╭╮╰╯─│├┤). Used by every
// non-active card; lipgloss.RoundedBorder() owns the actual values, this
// is just a typed mirror so callers can flip sets uniformly.
var thinBorderSet = borderRunes{
	TL: '╭', TR: '╮', BL: '╰', BR: '╯',
	Horiz: '─', Vert: '│',
	MidL: '├', MidR: '┤',
}

// heavyBorderSet — single-line heavy (┏┓┗┛━┃┣┫). Same set the done/working
// march decorator promotes individual cells to via heavyBorderMap; exposed
// here so the active frame can also opt in to heavy as a middle-ground
// weight between thin and double.
var heavyBorderSet = borderRunes{
	TL: '┏', TR: '┓', BL: '┗', BR: '┛',
	Horiz: '━', Vert: '┃',
	MidL: '┣', MidR: '┫',
}

// doubleBorderSet — double-line (╔╗╚╝═║╠╣). Visually the heaviest of the
// three weights; pairs with the blue title banner to make the focused
// card unmistakable from a distance.
var doubleBorderSet = borderRunes{
	TL: '╔', TR: '╗', BL: '╚', BR: '╝',
	Horiz: '═', Vert: '║',
	MidL: '╠', MidR: '╣',
}

// activeBorderSet selects which weight paints the active card. Flip the
// assignment to switch styles globally. The render_decorators.go thicken
// pass uses activeBorderRune() (matching map below) so leftover thin runes
// from applyBorderLabel get rewritten to the chosen weight.
var activeBorderSet = doubleBorderSet

// renderRowsBeauty produces one styled-but-uncolored string per row with
// rounded borders and │ walls. Active cards get heavy box-drawing runes
// + a ▶ anchor on the top-border title. marqueePhase advances one rune per
// 2 spinner ticks (≈300ms) and slides any title that overflows its slot.
// labelReserve[paneID] tells the marquee how many right-side cells to keep
// clear for the verb/done/asking label that applyBorderLabel will inject;
// CLI/test callers without label state pass nil. Caller layers state-
// driven coloring on top (see composeStyledLines for the bubble path).
func renderRowsBeauty(rows []Row, activePaneID, cursorPaneID string, maxWidth, marqueePhase int, labelReserve map[string]int) []string {
	tl, tr, bl, br, h, v := beautyRunes()
	rendered := make([]string, len(rows))
	for i, row := range rows {
		// Active state determined per row so border rows (which don't carry
		// PaneID directly) and body rows (which do) both see the same flag.
		isActive := false
		switch row.Kind {
		case kindBorderTop, kindBorderMid, kindBorderBot:
			isActive = activePaneID != "" && borderRowPaneID(row, i, rows) == activePaneID
		default:
			isActive = activePaneID != "" && row.PaneID == activePaneID
		}

		// Pick the rune set: thin for idle/done/cursor cards, heavy for
		// active. Done's chunky comet also thickens runes but only on
		// the lit sweep cells; here every cell of the active frame goes
		// heavy so the whole card reads thicker.
		ctl, ctr, cbl, cbr, ch, cv := tl, tr, bl, br, h, v
		cmidL, cmidR := '├', '┤'
		if isActive {
			s := activeBorderSet
			ctl, ctr, cbl, cbr, ch, cv = s.TL, s.TR, s.BL, s.BR, s.Horiz, s.Vert
			cmidL, cmidR = s.MidL, s.MidR
		}

		switch row.Kind {
		case kindSpacer:
			rendered[i] = ""
		case kindBorderTop:
			inner := max(0, maxWidth-2)
			text := row.Text
			// ▶ active anchor lives outside the marquee so the focus
			// indicator stays pinned to the left edge instead of scrolling
			// past with the path text.
			anchor := ""
			if text != "" && isActive {
				anchor = " ▶ "
				text = strings.TrimLeft(text, " ")
			}
			if text != "" {
				// Per-pane reserve mirrors the actual label width that
				// applyBorderLabel will inject — sized live from the verb
				// + spinner so a long verb like "combobulating" still
				// gets room (worst-case worst-case const is wrong; live
				// width is the only correct answer).
				pid := borderRowPaneID(row, i, rows)
				reserve := 0
				if labelReserve != nil {
					reserve = labelReserve[pid]
				}
				slot := inner - reserve - lipgloss.Width(anchor)
				if slot < 8 {
					slot = inner - lipgloss.Width(anchor)
				}
				if slot > 0 && lipgloss.Width(text) > slot {
					text = marqueeWindow(text, slot, marqueePhase)
				}
				body := anchor + text
				fill := max(0, inner-lipgloss.Width(body))
				rendered[i] = string(ctl) + body + strings.Repeat(string(ch), fill) + string(ctr)
			} else {
				rendered[i] = string(ctl) + strings.Repeat(string(ch), inner) + string(ctr)
			}
		case kindBorderMid:
			rendered[i] = string(cmidL) + strings.Repeat(string(ch), max(0, maxWidth-2)) + string(cmidR)
		case kindBorderBot:
			rendered[i] = string(cbl) + strings.Repeat(string(ch), max(0, maxWidth-2)) + string(cbr)
		default:
			prefix := " "
			if row.Kind == kindIntent {
				// Cursor-only anchor; active's anchor lives on the top-
				// border title row so the body stays visually clean.
				if row.PaneID == cursorPaneID && row.PaneID != activePaneID {
					prefix = "▸"
				}
			}
			if isCardRow(row.Kind) {
				inner := prefix + row.Text
				innerWidth := max(0, maxWidth-2)
				inner = truncateLine(inner, innerWidth)
				padding := max(0, innerWidth-lipgloss.Width(inner))
				rendered[i] = string(cv) + inner + strings.Repeat(" ", padding) + string(cv)
			} else {
				line := prefix + row.Text
				if maxWidth > 0 {
					line = truncateLine(line, maxWidth)
				}
				rendered[i] = line
			}
		}
	}
	return rendered
}

// marqueeSeparator is the gap drawn between repetitions of the title text
// when it wraps around in the marquee window. Three spaces is enough to
// make the loop boundary visible without inserting a glyph that could be
// mistaken for part of the path.
const marqueeSeparator = "   "

// marqueeWindow returns a slot-rune-wide view of text starting `phase`
// runes in, wrapping through marqueeSeparator. Caller guarantees text
// already exceeds slot — this function just slices.
func marqueeWindow(text string, slot, phase int) string {
	if slot <= 0 {
		return ""
	}
	runes := []rune(text + marqueeSeparator)
	cycle := len(runes)
	off := ((phase % cycle) + cycle) % cycle
	out := make([]rune, slot)
	for i := 0; i < slot; i++ {
		out[i] = runes[(off+i)%cycle]
	}
	return string(out)
}

// switchToPane focuses a tmux pane by session/window/pane triple. Used by
// Enter, mouse click, and the CLI subcommands in commands.go.
func switchToPane(session, windowID, paneID string) {
	exec.Command(cachedLookPath("tmux"), "switch-client", "-t", session).Run()
	exec.Command(cachedLookPath("tmux"), "select-window", "-t", windowID).Run()
	exec.Command(cachedLookPath("tmux"), "select-pane", "-t", paneID).Run()
}

// findPaneIndex returns the index of paneID in paneRows, defaulting to 0.
func findPaneIndex(paneRows []Row, paneID string) int {
	for i, row := range paneRows {
		if row.PaneID == paneID {
			return i
		}
	}
	return 0
}
