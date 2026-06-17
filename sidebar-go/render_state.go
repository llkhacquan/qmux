package main

import "time"

// renderState aggregates everything decorators need to make styling decisions.
// Built once per composeStyledLines call; passed as &renderState into
// each decorator so they share precomputed maps without re-walking rows.

type renderState struct {
	rows             []Row
	width            int
	activePaneID     string
	cursorPaneID     string
	lastActivePaneID string

	// windowActive[pid] is true for a Claude card that shares the focused
	// tmux window but isn't itself the focused pane (e.g. a console pane in
	// that window holds focus). Gets a quiet-blue frame — present-but-not-
	// focused — below every real status in the border priority order.
	windowActive map[string]bool

	needsInput map[string]bool
	working    map[string]bool
	// done panes are flashed yellow until the user switches to them
	// (cmdOnFocus → clearSharedDone). Kept separate from unread because
	// unread is per-instance + cursor-clearable; this is leader-truth
	// (rows.Status=="done") so the visual stays in sync with the badge.
	done map[string]bool
	// asked is a subset of done — panes where the last assistant message
	// was a question. Border label shows "asked 2m ago" instead of "2m ago".
	asked map[string]bool
	// statusSince is the unix-millis transition timestamp pulled off
	// each Row's StatusSince field. Currently consumed by the done
	// border label to show "Nm ago" relative to nowMillis; reusable by
	// any future state that wants a duration label. Zero value =
	// unknown timestamp; the label falls back to the literal status name.
	statusSince map[string]int64
	nowMillis   int64
	unread      map[string]bool
	verbs       map[string]string
	paneIdx     map[string]int

	blinkOn   bool
	spinFrame int
	// Two glyph sets so consumers can pick by pane focus: snowflake on
	// the active card, braille dots on background cards. Resolved once
	// per render in buildRenderState; both share spinFrame so the
	// cadence stays locked.
	spinGlyphActive   rune
	spinGlyphInactive rune

	searchMatches map[int]bool

	// cardRange[paneID] = {topIdx, botIdx} — the row indexes of the card's
	// top and bottom borders. Used by perimeter-based decorators (active
	// march). botIdx == -1 if no closing border found yet.
	cardRange map[string]cardRange
}

type cardRange struct {
	topIdx int
	botIdx int
}

// buildRenderState pulls everything off the teaModel into a flat struct so
// decorators don't need access to the full model. Order of map population
// matches composeStyledLines today.
func buildRenderState(m *teaModel, cursorID string) *renderState {
	st := &renderState{
		rows:             m.rows,
		width:            m.width,
		activePaneID:     m.activePaneID,
		cursorPaneID:     cursorID,
		lastActivePaneID: m.lastActivePaneID,
		needsInput:       paneSetWithStatus(m.rows, "needs-input"),
		working:          paneSetWithStatus(m.rows, "running"),
		done:             paneSetWithStatus(m.rows, "done"),
		asked:            paneAskedSet(m.rows),
		statusSince:      paneStatusSinceMap(m.rows),
		nowMillis:        time.Now().UnixMilli(),
		unread:           m.unreadPanes,
		verbs:            paneVerbMap(m.rows),
		paneIdx:          paneAutoIndex(m.rows),
		blinkOn:          m.blinkOn,
		spinFrame:         m.spinFrame,
		spinGlyphActive:   m.spinnerFrameActive(),
		spinGlyphInactive: m.spinnerFrameInactive(),
		searchMatches:     m.searchMatches,
	}
	st.windowActive = windowActiveSet(m.rows, m.activeWindowID, m.activePaneID)
	st.cardRange = computeCardRanges(m.rows)
	return st
}

// windowActiveSet collects Claude panes whose window holds focus but which
// aren't the focused pane. Empty when no active window is known (cold start)
// so cards stay dim until the first focus event lands.
func windowActiveSet(rows []Row, activeWindowID, activePaneID string) map[string]bool {
	out := make(map[string]bool)
	if activeWindowID == "" {
		return out
	}
	for _, r := range rows {
		if r.PaneID == "" || r.Window != activeWindowID {
			continue
		}
		if r.PaneID == activePaneID {
			continue
		}
		out[r.PaneID] = true
	}
	return out
}

// computeCardRanges walks rows once, pairing each kindBorderTop with the
// next kindBorderBot that resolves to the same paneID. The pane id of a
// border row is its forward neighbor (top) or backward neighbor (bot) —
// reuses borderRowPaneID so the resolution rule stays in one place.
func computeCardRanges(rows []Row) map[string]cardRange {
	out := make(map[string]cardRange)
	openTop := make(map[string]int) // paneID → topIdx awaiting close
	for i, r := range rows {
		switch r.Kind {
		case kindBorderTop:
			pid := borderRowPaneID(r, i, rows)
			if pid != "" {
				openTop[pid] = i
			}
		case kindBorderBot:
			pid := borderRowPaneID(r, i, rows)
			if pid == "" {
				continue
			}
			top, ok := openTop[pid]
			if !ok {
				continue
			}
			out[pid] = cardRange{topIdx: top, botIdx: i}
			delete(openTop, pid)
		}
	}
	return out
}
