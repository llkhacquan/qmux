package main

import "github.com/charmbracelet/lipgloss"

// Marching perimeter highlight.
//
// Cadence is heart-beat: a short bright segment sweeps clockwise around
// a card's border for ~1s, then rests for ~2s, repeating on a 3s cycle.
//
// Three flavors:
//   - Active card (idle): lavender comet — signals "this is the focused
//     card" without competing with state-driven colors.
//   - Working card: rainbow comet that shimmers in sync with the
//     "working" border label, so the comet and label feel like one
//     effect even though they're painted independently.
//   - Done card: bright yellow comet on top of the steady yellow border;
//     additionally thickens its lit cells (heavy box-drawing) so the
//     sweep feels like a chunky pill of light circling a thin frame.
//
// Suppressed for needs-input (already blinks peach). The decorator runs
// AFTER styleCells, so it only overrides cells on each card's perimeter.

const (
	// 150 ms per spin frame × 40 = 6.0 s total cycle. 20 frames ≈ 3.0 s
	// for the sweep around the perimeter, leaving 20 frames ≈ 3.0 s of
	// rest before the next loop.
	marchCycleFrames = 40
	marchSweepFrames = 20

	// 16-cell trail painted symmetrically as five bands
	// (low / high / peak / high / low). 16 gives a ~3-cell peak which
	// reads as a chunky pill against the typical 25-wide card without
	// wrapping past half the smallest perimeter.
	marchSegmentLen = 16
)

// cometBand maps a trail offset (0 = leading edge, segLen-1 = tail) to
// an intensity band: 0=low, 1=high, 2=peak. Symmetric around the center
// so the comet has a fade on both ends and a bright core in the middle.
//
// For segLen=10 the partition is offset/2 → quintile, peak=middle:
//
//	off 0,1 → low | 2,3 → high | 4,5 → peak | 6,7 → high | 8,9 → low
func cometBand(offset, segLen int) int {
	if segLen <= 0 || offset < 0 || offset >= segLen {
		return 0
	}
	q := offset * 5 / segLen
	switch q {
	case 2:
		return 2
	case 1, 3:
		return 1
	}
	return 0
}

// pickMarchStyle returns the comet color for a given pane, or nil to skip.
// Priority: needs-input suppresses (peach blink owns the animation),
// done gets a bright-yellow comet that thickens its lit cells, working
// gets a solid green sweep. Active idle no longer marches — the active
// card identifies itself by its gold top-border title alone, so its
// frame matches idle cards.
func pickMarchStyle(pid string, st *renderState) *lipgloss.Style {
	if pid == "" || st == nil {
		return nil
	}
	if _, ok := st.cardRange[pid]; !ok {
		return nil
	}
	if st.needsInput[pid] {
		return nil
	}
	if st.working[pid] {
		return &styleWorkingMarch
	}
	return nil
}

// marchActive reports whether the march decorator would paint any cells
// this frame. Used by the spin-tick refresh gate so we don't repaint
// during the rest portion of the cycle when nothing changes.
func marchActive(st *renderState) bool {
	if st == nil {
		return false
	}
	if marchPhase(st.spinFrame) < 0 {
		return false
	}
	for pid := range st.cardRange {
		if pickMarchStyle(pid, st) != nil {
			return true
		}
	}
	return false
}

// shouldRefreshForMarch is called from spinTickMsg to decide whether the
// model needs a repaint this frame. Repaints during the entire sweep PLUS
// one frame past the sweep end (so the highlight clears) — not during the
// rest portion of the cycle.
func (m *teaModel) shouldRefreshForMarch() bool {
	prev := m.spinFrame - 1
	if prev < 0 {
		prev = -prev
	}
	prevPhase := prev % marchCycleFrames
	curPhase := m.spinFrame % marchCycleFrames
	if curPhase >= marchSweepFrames && prevPhase >= marchSweepFrames {
		return false
	}
	// Only done + working panes march now (active idle dropped its
	// lavender comet when the gold-title redesign moved the active
	// state onto the top border). Both states animate every sweep
	// frame so any pane in those states forces a repaint.
	if hasStatus(m.rows, "done") || hasStatus(m.rows, "running") {
		return true
	}
	return false
}

// marchPhase returns the sweep position [0, sweepFrames) when the cycle
// is in its "sweep" portion, or -1 when at rest.
func marchPhase(spinFrame int) int {
	if spinFrame < 0 {
		spinFrame = -spinFrame
	}
	p := spinFrame % marchCycleFrames
	if p < marchSweepFrames {
		return p
	}
	return -1
}

// cometStyles holds the three intensity bands used by the comet trail
// plus a flag controlling whether lit cells get heavy box-drawing runes
// swapped in. Derived from a single peak style so callers only manage
// one color.
type cometStyles struct {
	low, high, peak *lipgloss.Style
	thicken         bool
}

// deriveCometStyles softens the peak style into a 3-band gradient.
// Default (lavender comet): peak keeps color+bold, high drops bold, low
// adds Faint for a barely-visible leading/trailing fade. Done comet: low
// collapses to high — Faint would render dimmer than the steady yellow
// border, painting dark holes around the bright peak instead of a clean
// glow. Returned styles are fresh copies so paintMarchPerimeter can
// safely take addresses.
func deriveCometStyles(peak *lipgloss.Style, thicken bool) cometStyles {
	high := peak.Bold(false)
	low := high
	if !thicken {
		low = high.Faint(true)
	}
	return cometStyles{low: &low, high: &high, peak: peak, thicken: thicken}
}

// rainbowCometStyles picks the bold rainbow color for the current frame
// (same index the verb label uses) and softens it through the standard
// 3-band fade. Whole comet shows one color per frame and cycles in
// lockstep with the verb-label palette.
func rainbowCometStyles(spinFrame int) cometStyles {
	return deriveCometStyles(rainbowMarchStylePtr(spinFrame), false)
}

// decorateMarch overlays the heart-beat highlight on every eligible
// card's perimeter. No-op during the rest portion of the cycle.
func decorateMarch(g *Grid, st *renderState) {
	if st == nil {
		return
	}
	phase := marchPhase(st.spinFrame)
	if phase < 0 {
		return
	}
	for pid, cr := range st.cardRange {
		peak := pickMarchStyle(pid, st)
		if peak == nil {
			continue
		}
		var styles cometStyles
		switch {
		case st.done[pid]:
			// Done comet thickens its lit cells (heavy box drawing) on
			// top of the steady yellow border.
			styles = deriveCometStyles(peak, true)
		case st.working[pid]:
			styles = rainbowCometStyles(st.spinFrame)
		default:
			styles = deriveCometStyles(peak, false)
		}
		paintMarchPerimeter(g, cr, phase, styles)
	}
}

// paintMarchPerimeter walks one card's perimeter and overrides each cell
// inside the lit window with the band-appropriate style. Body rows are
// padded to the same width as the top border by renderRowsBeauty, so
// the wall column is always w-1 across the card.
func paintMarchPerimeter(g *Grid, cr cardRange, phase int, styles cometStyles) {
	topIdx, botIdx := cr.topIdx, cr.botIdx
	if topIdx < 0 || botIdx <= topIdx {
		return
	}
	if topIdx >= len(g.Cells) || botIdx >= len(g.Cells) {
		return
	}
	w := len(g.Cells[topIdx])
	if w < 4 {
		return
	}
	h := botIdx - topIdx + 1
	if h < 2 {
		return
	}

	total := perimeterTotal(w, h)
	headPos := phase * total / marchSweepFrames

	for rowOff := range h {
		idx := topIdx + rowOff
		cells := g.Cells[idx]
		if cells == nil {
			continue
		}
		for col := range cells {
			p := perimeterIndex(rowOff, col, h, w)
			if p < 0 {
				continue
			}
			// offset = how far this cell trails behind the head, modulo
			// the perimeter length. 0 = head, segLen-1 = tail.
			offset := (headPos - p + total) % total
			if offset >= marchSegmentLen {
				continue
			}
			switch cometBand(offset, marchSegmentLen) {
			case 2:
				cells[col].Style = styles.peak
			case 1:
				cells[col].Style = styles.high
			default:
				cells[col].Style = styles.low
			}
			if styles.thicken {
				cells[col].R = thickenBorderRune(cells[col].R)
			}
		}
	}
}

// perimeterTotal returns the count of cells around a w×h card border.
// Top + right + bottom + left = 2w + 2(h-2) = 2w + 2h - 4.
func perimeterTotal(w, h int) int { return 2*w + 2*h - 4 }

// perimeterIndex maps card-relative (rowOff, col) coordinates to a
// clockwise position 0..total-1 starting at the top-left corner.
// Returns -1 for interior cells (not on the perimeter).
//
// Layout per quadrant:
//
//	top    (rowOff = 0)        : col 0..w-1            → 0..w-1
//	right  (col = w-1, mid)    : rowOff 1..h-2         → w..w+h-3
//	bottom (rowOff = h-1)      : col w-1..0            → w+h-2..2w+h-3
//	left   (col = 0, mid)      : rowOff h-2..1         → 2w+h-2..2w+2h-5
func perimeterIndex(rowOff, col, h, w int) int {
	if rowOff < 0 || rowOff >= h || col < 0 || col >= w {
		return -1
	}
	if rowOff == 0 {
		return col
	}
	if rowOff == h-1 {
		return w + (h - 2) + (w - 1 - col)
	}
	if col == w-1 {
		return w + (rowOff - 1)
	}
	if col == 0 {
		return 2*w + 2*h - 4 - rowOff
	}
	return -1
}

// inLitWindow reports whether perimeter index p falls inside the trailing
// segment of length segLen ending at headPos (modular).
func inLitWindow(p, headPos, total, segLen int) bool {
	if total <= 0 {
		return false
	}
	diff := (headPos - p + total) % total
	return diff < segLen
}
