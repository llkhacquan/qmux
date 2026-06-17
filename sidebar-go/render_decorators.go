package main

import (
	"strconv"

	"github.com/charmbracelet/lipgloss"
)

const (
	doneStaleSecs     = 12 * 3600 // 12h — noticeable border fade
	doneVeryStaleSecs = 24 * 3600 // 1d  — near-dim border
)

// pickDoneStyle returns a progressively dimmer yellow style the longer
// a done session has been sitting idle. <12h = bright, >=12h = muted,
// >=1d = near-gray.
func pickDoneStyle(since, now int64) *lipgloss.Style {
	if since == 0 {
		return &styleBorderYellow
	}
	elapsed := (now - since) / 1000
	switch {
	case elapsed >= doneVeryStaleSecs:
		return &styleBorderYellowVeryStale
	case elapsed >= doneStaleSecs:
		return &styleBorderYellowStale
	}
	return &styleBorderYellow
}

// elapsedBucket returns a compact m/h/d duration token (e.g. "5m", "2h",
// "1d") for the elapsed time between since/now (both unix-millis). Returns
// "" for sub-minute deltas — sub-minute granularity isn't useful in a
// border label that only redraws once per second, and the empty string
// lets callers omit the duration entirely while a state is fresh.
// since == 0 means "no timestamp recorded" → also "".
func elapsedBucket(since, now int64) string {
	if since == 0 {
		return ""
	}
	secs := (now - since) / 1000
	if secs < 60 {
		return ""
	}
	switch {
	case secs < 3600:
		return strconv.FormatInt(secs/60, 10) + "m"
	case secs < 86400:
		return strconv.FormatInt(secs/3600, 10) + "h"
	default:
		return strconv.FormatInt(secs/86400, 10) + "d"
	}
}

// doneAgoLabel formats the running→idle elapsed time as a border label
// like " 5m ago " or " asked 5m ago " when the last assistant message
// was a question. since == 0 falls back to a bare word so the label
// still reads as a state rather than a malformed duration.
func doneAgoLabel(since, now int64, asked bool) string {
	prefix := ""
	bare := " done "
	if asked {
		prefix = "asked "
		bare = " asked "
	}
	if since == 0 {
		return bare
	}
	if dur := elapsedBucket(since, now); dur != "" {
		return " " + prefix + dur + " ago "
	}
	if asked {
		return " just asked "
	}
	return " just now "
}

// Working-label design selector. Hardcoded const, flip + rebuild to
// switch — old design preserved so design tweaks are reversible without
// git surgery.
//
//	1 — classic  : " crafting ⠋ "         (verb + spinner only)
//	2 — duration : " crafting (1m) ⠋ "    (verb + elapsed + spinner;
//	                                        suffix only shows once the
//	                                        running-start timestamp is
//	                                        ≥ 60s old)
const (
	workingLabelClassic  = 1
	workingLabelDuration = 2
)

const selectedWorkingLabelStyle = workingLabelClassic

// workingLabel is the border label for a running pane. The verb +
// spinner are styled as one rainbow run by the caller; we build them
// into a single string here so applyBorderLabel can place the lot
// right-aligned against the border.
func workingLabel(verb string, since, now int64, spin rune) string {
	body := workingVerbToken(verb)
	if selectedWorkingLabelStyle == workingLabelDuration {
		if dur := elapsedBucket(since, now); dur != "" {
			return " " + body + " (" + dur + ") " + string(spin) + " "
		}
	}
	return " " + body + " " + string(spin) + " "
}

// heavyBorderMap maps thin box-drawing runes to their heavy variants.
// Used by the done-card comet to thicken only the cells the sweep is
// currently lighting — the steady portion of the perimeter stays thin.
var heavyBorderMap = map[rune]rune{
	'─': '━',
	'│': '┃',
	'╭': '┏',
	'╮': '┓',
	'╰': '┗',
	'╯': '┛',
	'├': '┣',
	'┤': '┫',
}

// thickenBorderRune returns the heavy variant of a thin box-drawing rune,
// or the rune unchanged when it isn't a thin border glyph.
func thickenBorderRune(r rune) rune {
	if heavy, ok := heavyBorderMap[r]; ok {
		return heavy
	}
	return r
}

// activeBorderMap is a thin→activeBorderSet mapping, rebuilt lazily so a
// future hot-swap of activeBorderSet (e.g. to heavy or thin) takes effect
// without recompiling. Used by the active-frame thicken pass below to
// upgrade stray thin runes left behind by applyBorderLabel.
var activeBorderMap = buildActiveBorderMap()

func buildActiveBorderMap() map[rune]rune {
	s := activeBorderSet
	return map[rune]rune{
		'─': s.Horiz,
		'│': s.Vert,
		'╭': s.TL,
		'╮': s.TR,
		'╰': s.BL,
		'╯': s.BR,
		'├': s.MidL,
		'┤': s.MidR,
	}
}

// activeBorderRune returns the active-set variant of a thin box-drawing
// rune. Empty/non-border runes pass through unchanged.
func activeBorderRune(r rune) rune {
	if d, ok := activeBorderMap[r]; ok {
		return d
	}
	return r
}

// isBorderFillRune reports whether r is a horizontal/corner/T-junction
// border glyph (thin, heavy, or double). Used by border-label injection
// to find the end of actual title text — fill runes don't count as
// title. Missing the double-line set silently dropped the verb label
// on every active card because the ═ fill cells got counted as title
// text and pushed titleEnd past labelStart.
func isBorderFillRune(r rune) bool {
	switch r {
	case '─', '╭', '╮', '╰', '╯', '├', '┤':
		return true
	case '━', '┏', '┓', '┗', '┛', '┣', '┫':
		return true
	case '═', '╔', '╗', '╚', '╝', '╠', '╣':
		return true
	}
	return false
}

// styleCells walks the grid and assigns Style pointers + Italic flags to
// every cell. Mirrors the legacy composeBorderRow/composeCardBodyRow logic
// but operates on the cell grid so per-cell effects (like the marching
// perimeter highlight) can be layered as separate decorators.
//
// Order of operations per row:
//  1. Pick base border/body style.
//  2. Apply rune mutations (footer digit, status indicator, label glyphs).
//  3. Override styles for label segments / status indicator / ⏎ marker.
//  4. Search-italic flag on matching body cells.
//
// Decorators that come AFTER this (e.g. activeMarch in a future phase)
// can re-style perimeter cells without disturbing the rune contents.
func styleCells(g *Grid, st *renderState) {
	for i, row := range st.rows {
		cells := g.Cells[i]
		if cells == nil {
			continue
		}
		switch row.Kind {
		case kindSession:
			styleSessionRow(cells)
		case kindBorderTop, kindBorderMid, kindBorderBot:
			styleBorderRow(st, i, row, cells)
		default:
			styleBodyRow(st, i, row, cells)
		}
	}
}

func styleSessionRow(cells []Cell) {
	for k := range cells {
		cells[k].Style = &styleSession
	}
}

// styleBorderRow paints a border (top/mid/bot) row. State priority:
// needs-input(blink) > active > window-active > cursor > dim. Top/mid borders
// may also carry a right-aligned status label injected via applyBorderLabel.
func styleBorderRow(st *renderState, idx int, row Row, cells []Cell) {
	pid := borderRowPaneID(row, idx, st.rows)

	base := pickBorderStyle(pid, st, row.Kind == kindBorderTop)
	for k := range cells {
		cells[k].Style = base
	}

	// Bot border carries an auto-numbered footer at column 2 (after the
	// corner glyph + space). Only paint when index ≤ 9 and there's room.
	if row.Kind == kindBorderBot && pid != "" && len(cells) > 4 {
		if n, ok := st.paneIdx[pid]; ok && n <= 9 && len(cells) > 2 {
			cells[2].R = rune('0' + n)
		}
	}

	// Top/mid border may carry a status label. The same priority order
	// the legacy uses (needs > working > unread).
	if row.Kind == kindBorderTop || row.Kind == kindBorderMid {
		switch {
		case pid != "" && st.needsInput[pid]:
			applyBorderLabel(cells, " asking ", '?', &stylePeachBold, base)
		case pid != "" && st.working[pid]:
			// Active card gets the sparkle glyph; background cards get
			// the dots. Same spinFrame so phases stay aligned across the
			// fleet — only the rune table differs.
			spin := st.spinGlyphInactive
			if pid == st.activePaneID {
				spin = st.spinGlyphActive
			}
			label := workingLabel(st.verbs[pid], st.statusSince[pid], st.nowMillis, spin)
			// Rainbow palette cycles only on the focused working card so the
			// shimmer reads as "this is the running pane you're watching".
			// Background working panes stay solid green to match their frame.
			labelStyle := &styleBorderGreen
			if pid == st.activePaneID {
				labelStyle = rainbowStylePtr(st.spinFrame)
			}
			applyBorderLabel(cells, label, '─', labelStyle, base)
		case pid != "" && st.done[pid]:
			doneStyle := pickDoneStyle(st.statusSince[pid], st.nowMillis)
			applyBorderLabel(cells, doneAgoLabel(st.statusSince[pid], st.nowMillis, st.asked[pid]), '─', doneStyle, base)
		}
	}

	// ⏎ marker on top border: keep the base border style on the rest of
	// the row and only recolor the ⏎ rune itself. (An older pass cleared
	// the whole row to nil here, which painted the last-active card's top
	// border in the terminal's default fg — visibly brighter than every
	// other card's dim border.)
	if row.Kind == kindBorderTop {
		for k, c := range cells {
			if c.R == '⏎' {
				cells[k].Style = &styleEnterMark
			}
		}

		// ▶ active-anchor: when needs-input's peach blink owns the row,
		// the ▶ would blend into the peach base. Force-render in active-
		// blue so the marker still pops. Active alone (no needs blink)
		// keeps the banner bg on the ▶ — overwriting it with plain
		// styleActiveTitle would punch a hole through the tab.
		if pid != "" && pid == st.activePaneID && st.needsInput[pid] && st.blinkOn {
			for k, c := range cells {
				if c.R == '▶' {
					cells[k].Style = &styleActiveTitle
				}
			}
		}
	}

	// Active card frame uses activeBorderSet (currently double-line) —
	// but applyBorderLabel above writes a thin `─` at the icon slot and
	// overwrites label runes, leaving thin glyphs behind. Upgrade them
	// via activeBorderRune so the whole frame matches the chosen weight.
	// No-op for non-active rows.
	if pid != "" && pid == st.activePaneID {
		for k, c := range cells {
			cells[k].R = activeBorderRune(c.R)
		}
	}
}

// pickBorderStyle returns the base style pointer for a border row given
// the pane id and global state. Active wears its full bold-blue frame
// on every border row (top/mid/bot) — combined with the heavy box-
// drawing runes from renderRowsBeauty this gives the active card a
// thicker, brighter outline than any other state. Done/needs-input/
// cursor still color the entire frame when the pane is NOT active.
//
// State priority: needs-input(blink) > active > window-active > done >
// working > cursor > dim.
// Active sits above done so a freshly-finished card the user already
// focused reads as a normal active card; the " done " label + ✅ body
// badge keep the finished signal without repainting the frame yellow.
// Working+active is the only combo that shimmers: frame, verb label,
// and comet all share rainbowStylePtr(spinFrame) so they cycle in
// lockstep. Working-only panes get a steady green frame (label + comet
// also collapse to green) so they read as "running but unfocused"
// without painting attention.
//
// isTopBorder picks the banner variant for the active card's top row only,
// so the focused card reads as a colored tab/header against its still-blue
// frame. Mid/bot/sides keep styleActiveTitle (blue fg, no bg) so the
// emphasis lives in the title row alone.
func pickBorderStyle(pid string, st *renderState, isTopBorder bool) *lipgloss.Style {
	isActive := pid != "" && pid == st.activePaneID
	switch {
	case pid != "" && st.needsInput[pid] && st.blinkOn:
		return &styleBorderPeach
	case isActive && st.working[pid]:
		return rainbowStylePtr(st.spinFrame)
	case isActive:
		if isTopBorder {
			return &styleActiveTitleBanner
		}
		return &styleActiveTitle
	case pid != "" && st.windowActive[pid]:
		// A Claude sharing the focused window reads as "still the session
		// you're working with" even after you tab to its console — so it
		// outranks done/working/cursor, mirroring how true-active does. Quiet
		// (non-bold) blue on the whole frame; weaker than active's bold +
		// double-line. The working/done label still rides along via
		// styleBorderRow to keep the status visible.
		return &styleWindowActiveTitle
	case pid != "" && st.done[pid]:
		return pickDoneStyle(st.statusSince[pid], st.nowMillis)
	case pid != "" && st.working[pid]:
		return &styleBorderGreen
	case pid != "" && pid == st.cursorPaneID:
		return &styleBorderCursor
	}
	return &styleBorderDim
}

// applyBorderLabel writes the label/icon onto cells [labelStart..iconPos]
// and assigns segment styles. iconPos = w-3 (matches legacy injectBorderLabel).
// When icon='─' the icon cell falls back to borderStyle so the working
// label's trailing dash blends into the border line.
func applyBorderLabel(cells []Cell, label string, icon rune, labelStyle, borderStyle *lipgloss.Style) {
	w := len(cells)
	if w < 4 {
		return
	}

	// titleEnd tracks the last non-border-glyph rune in the original line —
	// when the label would overlap the title, fall back to icon-only mode
	// so the title stays readable. Heavy variants (━┏┓┗┛┣┫) are also
	// excluded — active cards use heavy box-drawing for the frame, and
	// without them in the skip list every ━ fill cell counted as title
	// text, pushing titleEnd to the right edge and silently suppressing
	// the working/done label on every active pane.
	titleEnd := 0
	for i := range w {
		ch := cells[i].R
		if !isBorderFillRune(ch) {
			titleEnd = i + 1
		}
	}

	iconPos := w - 3
	labelRunes := []rune(label)
	labelStart := iconPos - len(labelRunes)
	hasLabel := labelStart > titleEnd+1

	if hasLabel {
		for i, lr := range labelRunes {
			pos := labelStart + i
			cells[pos].R = lr
			cells[pos].Style = labelStyle
		}
	}
	cells[iconPos].R = icon
	if icon == '─' {
		cells[iconPos].Style = borderStyle
	} else {
		cells[iconPos].Style = labelStyle
	}
}

// styleBodyRow paints a card body row (intent/intent_cont/location/git/preview).
// Walls get state-driven colors; the inner body picks a style based on
// kind+active+cursor. Status indicator at col w-2 may be overwritten with
// ▐ (needs/work/unread) or ◂ (last-active intent) and recolored.
func styleBodyRow(st *renderState, idx int, row Row, cells []Cell) {
	w := len(cells)
	if w == 0 {
		return
	}

	isActive := row.PaneID != "" && row.PaneID == st.activePaneID
	isCursor := row.PaneID != "" && row.PaneID == st.cursorPaneID
	isNeeds := row.PaneID != "" && st.needsInput[row.PaneID]
	isWork := row.PaneID != "" && st.working[row.PaneID]
	isUnread := row.PaneID != "" && st.done[row.PaneID]
	isWinActive := row.PaneID != "" && st.windowActive[row.PaneID]
	isLast := row.PaneID != "" && row.PaneID == st.lastActivePaneID && !isActive
	isMatch := st.searchMatches[idx]

	body := bodyStylePtr(row, isActive, isCursor)

	if w < 2 {
		// No walls — apply body style across the whole row, italic if matched.
		for k := range cells {
			cells[k].Style = body
			cells[k].Italic = isMatch
		}
		return
	}

	// Side walls must mirror pickBorderStyle's priority chain exactly so
	// top/bot borders and side walls never show different state colors.
	// Active+working is the only combo that shimmers (rainbow per spin
	// frame). Active alone keeps bold blue. Done > working > cursor > dim.
	pickWall := func() *lipgloss.Style {
		switch {
		case isNeeds && st.blinkOn:
			return &styleBorderPeach
		case isActive && isWork:
			return rainbowStylePtr(st.spinFrame)
		case isActive:
			return &styleActiveTitle
		case isWinActive:
			return &styleWindowActiveTitle
		case isUnread:
			return pickDoneStyle(st.statusSince[row.PaneID], st.nowMillis)
		case isWork:
			return &styleBorderGreen
		case isCursor:
			return &styleBorderCursor
		}
		return &styleBorderDim
	}
	leftStyle := pickWall()
	rightStyle := leftStyle

	cells[0].Style = leftStyle
	cells[w-1].Style = rightStyle

	// Inner body cells. running/needs-input animate elsewhere (rainbow
	// border label, peach blink) — they don't need an inline indicator.
	for k := 1; k < w-1; k++ {
		cells[k].Style = body
		if isMatch {
			cells[k].Italic = true
		}
	}

	// Last-active hint glyph on the intent row. Not a state indicator —
	// it's a history breadcrumb showing which pane the user came from,
	// so it stays even though the ▐ status indicator was retired.
	if isLast && row.Kind == kindIntent {
		statusIdx := w - 2
		cells[statusIdx].R = '◂'
		cells[statusIdx].Style = &styleBorderDim
		cells[statusIdx].Italic = false
	}

}

// bodyStylePtr returns the style pointer for a body cell of the given kind
// + active/cursor state. Mirrors styleForRow but as pointer-stable styles
// so the cell-grid coalescer can group runs.
func bodyStylePtr(row Row, isActive, isCursor bool) *lipgloss.Style {
	switch row.Kind {
	case kindSession:
		return &styleSession
	case kindIntent, kindIntentCont:
		switch {
		case isActive:
			return &styleSession
		case isCursor:
			return &styleBorderCursor
		}
		return &styleIntentDim
	case kindLocation:
		if isCursor && !isActive {
			return &styleCursorBody
		}
		return &styleLocation
	case kindGit:
		if isCursor && !isActive {
			return &styleCursorBody
		}
		return &styleGit
	case kindPreview:
		if isCursor && !isActive {
			return &styleCursorPreview
		}
		return &stylePreview
	}
	if isCursor && !isActive {
		return &styleBorderCursor
	}
	return &styleIntent
}

