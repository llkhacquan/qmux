package main

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// Menu layout: line 0 is the title, line 1 the separator, workspace rows start
// at line 2. menuRowToWorkspace inverts that for click hit-testing; clicks on
// the title/separator/footer fall outside [0,len) and are inert.
const menuHeaderLines = 2

func menuRowToWorkspace(y int) int { return y - menuHeaderLines }

// menuView renders the right-click show/hide workspace menu as a full-pane
// takeover (simplest, and a 25-col sidebar has no room to overlay). Visible
// workspaces show a green ✓; hidden ones are dimmed with no check. The trailing
// count is the number of Claude cards in that session.
func (m teaModel) menuView() string {
	h := m.viewport.Height
	if h <= 0 {
		h = m.height
	}
	if h <= 0 || m.width == 0 {
		return ""
	}
	w := m.width
	dim := lipgloss.NewStyle().Foreground(colorDim)
	sep := dim.Render(strings.Repeat("─", w))
	lines := []string{
		lipgloss.NewStyle().Foreground(colorLavender).Bold(true).Render(" Workspaces"),
		sep,
	}
	if len(m.workspaces) == 0 {
		lines = append(lines, dim.Render(" (no claude panes)"))
	}
	for _, ws := range m.workspaces {
		lines = append(lines, menuWorkspaceLine(ws, w))
	}
	lines = append(lines, sep, dim.Render(" right-click / esc: close"))

	// Pad/clip to the pane height so the render is stable frame-to-frame.
	for len(lines) < h {
		lines = append(lines, "")
	}
	if len(lines) > h {
		lines = lines[:h]
	}
	return strings.Join(lines, "\n")
}

// menuWorkspaceLine formats one workspace row, width-padded so the card count
// sits flush right. Whole-row color encodes state (green = shown, dim = hidden)
// which keeps the width math simple — one style per line, no mid-row segments.
func menuWorkspaceLine(ws workspaceItem, w int) string {
	check := "  "
	if !ws.Hidden {
		check = "✓ "
	}
	label := " " + check + ws.Name
	count := fmt.Sprintf("%d", ws.Count)
	gap := w - lipgloss.Width(label) - lipgloss.Width(count) - 1
	if gap < 1 {
		gap = 1
	}
	text := label + strings.Repeat(" ", gap) + count + " "
	style := lipgloss.NewStyle().Foreground(colorGreen)
	if ws.Hidden {
		style = lipgloss.NewStyle().Foreground(colorDim)
	}
	return style.Render(text)
}

// allHiddenView fills the pane when every workspace is hidden, so the sidebar
// isn't a blank rectangle with no way back.
func (m teaModel) allHiddenView() string {
	h := m.viewport.Height
	if h <= 0 {
		h = m.height
	}
	dim := lipgloss.NewStyle().Foreground(colorDim)
	lines := []string{
		dim.Render(" all workspaces hidden"),
		dim.Render(" right-click to show"),
	}
	for len(lines) < h {
		lines = append(lines, "")
	}
	if len(lines) > h && h > 0 {
		lines = lines[:h]
	}
	return strings.Join(lines, "\n")
}

// View renders the current model state. Bypasses viewport.View() to avoid
// its per-frame lipgloss.Render wrapping (ANSI width measurement on every
// visible line). Instead we directly slice the pre-styled lines using the
// viewport's YOffset for O(visible) string joins, no ANSI parsing.
func (m teaModel) View() string {
	if m.width == 0 {
		return ""
	}
	if m.menuOpen {
		return m.menuView()
	}
	if len(m.rows) == 0 {
		// Thin client with no daemon snapshot yet: show an animated spinner
		// during the cold-boot daemon wait instead of a blank pane (the TUI now
		// starts before the daemon connects). Falls through to the plain
		// placeholder once a snapshot has arrived (rows can still be empty when
		// there are zero Claude panes).
		if m.clientMode && !m.gotSnapshot {
			return m.connectingView()
		}
		// Cards exist but every workspace is hidden: point the user at the
		// right-click menu instead of a blank pane.
		if len(m.workspaces) > 0 {
			return m.allHiddenView()
		}
		return "loading…"
	}
	// Slice visible lines directly from cached styled output.
	body := m.visibleBody()
	tail := ""
	if footer := formatUsageFooter(m.width); footer != "" {
		tail = "\n" + styleFooter.Render(footer)
	}
	if m.searchActive || m.searchInput.Value() != "" {
		return body + tail + "\n" + m.searchInput.View()
	}
	return body + tail
}

// connectingView renders a centered, rainbow-shimmer spinner shown while a thin
// client waits for its first daemon snapshot on cold boot. Recomputed every
// spinTick (View runs each frame), so the glyph animates without touching the
// styledLines path. Mirrors the working-verb border treatment so the wait reads
// as the same UI, not an error.
func (m teaModel) connectingView() string {
	h := m.viewport.Height
	if h <= 0 {
		h = m.height
	}
	if h <= 0 || m.width == 0 {
		return ""
	}
	glyph := lipgloss.NewStyle().
		Foreground(rainbowColors[m.spinFrame%len(rainbowColors)]).
		Render(string(m.spinnerFrameActive()))
	label := lipgloss.NewStyle().Foreground(colorLavender).Render(" waiting for daemon…")
	msg := glyph + label

	pad := (m.width - lipgloss.Width(msg)) / 2
	if pad < 0 {
		pad = 0
	}
	line := strings.Repeat(" ", pad) + msg

	lines := make([]string, h)
	lines[h/2] = line
	return strings.Join(lines, "\n")
}

// visibleBody returns the viewport-clipped portion of styledLines, padded
// to viewport.Height with empty lines so the terminal output is stable.
func (m teaModel) visibleBody() string {
	h := m.viewport.Height
	if h <= 0 || len(m.styledLines) == 0 {
		return ""
	}
	top := m.viewport.YOffset
	if top < 0 {
		top = 0
	}
	bottom := top + h
	if bottom > len(m.styledLines) {
		bottom = len(m.styledLines)
	}
	if top >= len(m.styledLines) {
		return strings.Repeat("\n", h-1)
	}
	visible := m.styledLines[top:bottom]
	// Pad to viewport height so the footer doesn't shift.
	if len(visible) < h {
		pad := make([]string, h-len(visible))
		visible = append(visible, pad...)
	}
	return strings.Join(visible, "\n")
}

// refreshContent re-renders the row strings and adjusts the viewport offset.
// Stores styled lines in m.styledLines; View() slices them directly,
// bypassing viewport.View()'s per-frame lipgloss wrapping.
func (m *teaModel) refreshContent() {
	if m.width == 0 {
		return
	}
	out := m.composeStyledLines()
	m.styledLines = out

	// Sync viewport's internal line count so its scroll math stays correct
	// (ScrollUp/Down, YOffset clamping). We still need SetContent for that,
	// but we can skip the expensive findLongestLineWidth by using a compact
	// line-count-only content string when nothing changed structurally.
	m.viewport.SetContent(strings.Join(out, "\n"))

	if m.viewportPinned {
		maxOffset := len(out) - m.viewport.Height
		if maxOffset <= 0 {
			m.viewportPinned = false
			m.pinnedOffset = 0
			m.viewport.SetYOffset(0)
		} else if m.pinnedOffset > maxOffset {
			m.pinnedOffset = maxOffset
			m.viewport.SetYOffset(maxOffset)
		} else {
			m.viewport.SetYOffset(m.pinnedOffset)
		}
	} else {
		m.viewport.SetYOffset(0)
	}
	m.ensureCursorVisible()
}

// ensureCursorVisible scrolls the viewport so the row owning cursorPaneID is
// in the visible window. Mirrors ensureVisible() from the tcell path but is
// expressed against viewport.YOffset instead of a manual scrollOffset int.
func (m *teaModel) ensureCursorVisible() {
	if m.cursorPaneID == "" || m.viewport.Height <= 0 {
		return
	}
	// User-driven scroll (wheel / Ctrl+D / Ctrl+U) pins the viewport so the
	// next periodic refresh doesn't yank it back to the cursor. Any cursor
	// nav (handleKey paths, mouse click, search) clears the pin so cursor
	// tracking resumes immediately.
	if m.viewportPinned {
		return
	}
	idx := findCursorRowIndex(m.rows, m.cursorPaneID)
	if idx < 0 {
		return
	}
	top := m.viewport.YOffset
	bottom := top + m.viewport.Height - 1
	// Only scroll when the cursor row falls entirely outside the viewport.
	// We deliberately ignore scrolloff here — applying it on every passive
	// refresh (1s tick) would scroll the viewport down whenever the cursor
	// sits within `scrolloff` rows of the top, hiding the first card's
	// border. Keypress nav still uses the keys' own scroll commands which
	// pin and adjust offset directly.
	if idx < top {
		m.viewport.SetYOffset(idx)
	} else if idx > bottom {
		m.viewport.SetYOffset(idx - m.viewport.Height + 1)
	}
}

// findCursorRowIndex returns the row index whose PaneID matches cursorPaneID,
// preferring kindIntent rows so the cursor "lands" on the visible card title
// instead of a continuation/location row.
func findCursorRowIndex(rows []Row, cursorPaneID string) int {
	fallback := -1
	for i, r := range rows {
		if r.PaneID != cursorPaneID {
			continue
		}
		if r.Kind == kindIntent {
			return i
		}
		if fallback < 0 {
			fallback = i
		}
	}
	return fallback
}
