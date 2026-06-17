package main

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// composeStyledLines turns the model's row list into the final styled strings
// that go into the viewport. Pipeline:
//
//	rows → renderRowsBeauty → rasterize → styleCells → decorateMarch → serialize.
//
// Serialize (lipgloss.Style.Render per style-run segment) is 83% of the cost.
// Per-row dirty tracking skips serialize for rows whose cells haven't changed
// since the last frame. On a typical animation tick (spinner glyph change),
// only 2-4 out of 25 rows are dirty, cutting frame cost by ~70-80%.
func (m *teaModel) composeStyledLines() []string {
	cursorID := ""
	if m.cursorVisible {
		cursorID = m.cursorPaneID
	}
	st := buildRenderState(m, cursorID)
	reserve := buildLabelReserves(st)
	raw := renderRowsBeauty(m.rows, m.activePaneID, cursorID, m.width, m.spinFrame/2, reserve)
	g := rasterize(m.rows, raw)
	styleCells(g, st)
	decorateMarch(g, st)
	return serializeWithCache(g, &m.gridCache)
}

// cachedGrid stores the previous frame's styled cells and serialized strings.
// serializeWithCache compares each row's cells against this to skip Render
// calls on unchanged rows (the game-engine dirty-flag pattern).
type cachedGrid struct {
	cells [][]Cell   // previous frame's styled cells (after styleCells+decorateMarch)
	lines []string   // previous frame's serialized output
}

// buildLabelReserves returns the cell width applyBorderLabel will consume
// on the right side of each pane's top border. Mirrors render_decorators.go's
// state priority (needs > working > done) and label formatting so the title
// marquee can stop exactly where the label will start.
//
// Reserve = label rune width + 4: 3 cells beyond labelStart cover the icon
// at iconPos = w-3 plus the corner gap, and the +1 enforces the strict
// `titleEnd+1 < labelStart` gate inside applyBorderLabel.
func buildLabelReserves(st *renderState) map[string]int {
	out := make(map[string]int)
	for pid := range st.paneIdx {
		var label string
		switch {
		case st.needsInput[pid]:
			label = " asking "
		case st.working[pid]:
			// Mirror styleBorderRow's active/inactive split so reserve
			// widths match the glyph the decorator will actually paint.
			spin := st.spinGlyphInactive
			if pid == st.activePaneID {
				spin = st.spinGlyphActive
			}
			label = workingLabel(st.verbs[pid], st.statusSince[pid], st.nowMillis, spin)
		case st.done[pid]:
			label = doneAgoLabel(st.statusSince[pid], st.nowMillis, st.asked[pid])
		}
		if label != "" {
			out[pid] = lipgloss.Width(label) + 4
		}
	}
	return out
}

// borderRowPaneID resolves which pane a border row belongs to by scanning
// its neighbors. Top/mid borders look forward; bottom borders look back.
func borderRowPaneID(row Row, idx int, rows []Row) string {
	if row.Kind == kindBorderTop || row.Kind == kindBorderMid {
		for j := idx + 1; j < len(rows); j++ {
			if rows[j].PaneID != "" {
				return rows[j].PaneID
			}
			if !isCardRow(rows[j].Kind) {
				return ""
			}
		}
	} else if row.Kind == kindBorderBot {
		for j := idx - 1; j >= 0; j-- {
			if rows[j].PaneID != "" {
				return rows[j].PaneID
			}
			if !isCardRow(rows[j].Kind) {
				return ""
			}
		}
	}
	return ""
}

// hasStatus reports whether any row carries the given Status — fast path
// for tick handlers that want to skip work when nothing is animating.
func hasStatus(rows []Row, status string) bool {
	for _, r := range rows {
		if r.Status == status {
			return true
		}
	}
	return false
}

// paneSetWithStatus collects pane IDs whose location row carries the given
// status. Drives needsInput/working overlays.
func paneSetWithStatus(rows []Row, status string) map[string]bool {
	out := make(map[string]bool)
	for _, r := range rows {
		if r.Status == status && r.PaneID != "" {
			out[r.PaneID] = true
		}
	}
	return out
}

// paneStatusSinceMap collects the per-pane StatusSince timestamps so
// duration labels (e.g. done's "Nm ago") can compute "elapsed since the
// status was set" without each decorator re-walking rows. Skips entries
// with StatusSince == 0 (unknown) so callers can use a simple presence
// check before reading.
func paneStatusSinceMap(rows []Row) map[string]int64 {
	out := make(map[string]int64)
	for _, r := range rows {
		if r.PaneID != "" && r.StatusSince != 0 {
			out[r.PaneID] = r.StatusSince
		}
	}
	return out
}

// paneVerbMap collects the live status verb per pane (set by tree.go on the
// location row). Used by composeBorderRow to label running cards with the
// actual verb Claude is showing instead of a generic "working".
func paneVerbMap(rows []Row) map[string]string {
	out := make(map[string]string)
	for _, r := range rows {
		if r.Verb != "" && r.PaneID != "" {
			out[r.PaneID] = r.Verb
		}
	}
	return out
}

// paneAskedSet collects pane IDs whose location row has Asked=true.
func paneAskedSet(rows []Row) map[string]bool {
	out := make(map[string]bool)
	for _, r := range rows {
		if r.Asked && r.PaneID != "" {
			out[r.PaneID] = true
		}
	}
	return out
}

// workingVerbToken returns just the verb word (lowercased, no "…", no
// surrounding spaces) — "Crafting…" → "crafting". Falls back to "working"
// when no verb was captured. Used as a building block by both
// workingBorderLabel and the duration-aware variant below.
func workingVerbToken(verb string) string {
	if verb == "" {
		return "working"
	}
	v := strings.TrimSuffix(verb, "…")
	v = strings.ToLower(v)
	if v == "" {
		return "working"
	}
	return v
}

// workingBorderLabel formats a Claude verb for the border label: lowercased
// and stripped of the trailing "…" so it sits cleanly between border glyphs
// (e.g. "Crafting…" → " crafting "). Falls back to " working " when no verb
// was captured.
func workingBorderLabel(verb string) string {
	return " " + workingVerbToken(verb) + " "
}

// paneAutoIndex assigns 1-based ordinals to panes in row order — the same
// numbers used by `goto N` and shown on the bottom-border footer.
func paneAutoIndex(rows []Row) map[string]int {
	out := make(map[string]int)
	idx := 1
	for _, r := range rows {
		if r.PaneID == "" {
			continue
		}
		if _, seen := out[r.PaneID]; !seen {
			out[r.PaneID] = idx
			idx++
		}
	}
	return out
}
