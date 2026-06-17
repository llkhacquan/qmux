package main

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// Cell + Grid: per-glyph rendering primitives.
//
// renderRowsBeauty produces uncolored rune layouts; rasterize turns those
// into a Grid where each cell carries a styleHandle (pointer-comparable)
// plus optional flags. Decorators mutate cells; serialize emits ANSI
// strings, coalescing runs of cells that share the same (handle, flags).
//
// Handle-based comparison sidesteps lipgloss.Style not being == comparable
// (it carries a map of rules). All canonical styles live in tea_styles.go
// as package-level vars; we hold pointers to them so coalescing is a
// pointer comparison rather than a deep-equal walk.

// CellSlot tags a rune by its structural role inside the rendered card.
// Decorators read this to find walls, status indicators, label slots,
// etc. without re-parsing glyphs.
type CellSlot uint8

const (
	SlotPlain      CellSlot = iota // body interior text
	SlotBorderH                    // ─, top/bot horizontal fill
	SlotCornerTL                   // ╭
	SlotCornerTR                   // ╮
	SlotCornerBL                   // ╰
	SlotCornerBR                   // ╯
	SlotJoinL                      // ├
	SlotJoinR                      // ┤
	SlotBorderV                    // │ left/right wall
	SlotStatusInd                  // last inner col, candidate for ▐/◂
	SlotEmpty                      // spacer rows
	SlotSession                    // session header text
)

// Cell is one terminal cell in the rendered grid.
type Cell struct {
	R      rune
	Style  *lipgloss.Style // pointer into the canonical palette; nil → no styling
	Slot   CellSlot
	Italic bool // search-match overlay; serialize applies via .Italic(true)
}

// Grid is the rasterized representation of all rows. Each entry is a slice
// of cells (one per terminal column). Empty rows (kindSpacer) are
// represented as nil slices and serialized to "".
type Grid struct {
	Cells [][]Cell
	Rows  []Row // mirrors source rows for decorators that look up Kind/PaneID
}

// rasterize turns renderRowsBeauty's output into a Grid. Slot classification
// is purely positional+glyph-based — same set of glyphs render_legacy.go
// emits today (rounded corners, ─, │, ├/┤).
func rasterize(rows []Row, rawLines []string) *Grid {
	g := &Grid{
		Cells: make([][]Cell, len(rows)),
		Rows:  rows,
	}
	for i, line := range rawLines {
		row := rows[i]
		if row.Kind == kindSpacer || line == "" {
			g.Cells[i] = nil
			continue
		}
		runes := []rune(line)
		cells := make([]Cell, len(runes))
		for j, r := range runes {
			cells[j] = Cell{R: r, Slot: classifySlot(row, r, j, len(runes))}
		}
		g.Cells[i] = cells
	}
	return g
}

// classifySlot picks a CellSlot from row kind, glyph identity, and position.
func classifySlot(row Row, r rune, col, totalCols int) CellSlot {
	switch row.Kind {
	case kindSession:
		return SlotSession
	case kindBorderTop:
		switch r {
		case '╭':
			return SlotCornerTL
		case '╮':
			return SlotCornerTR
		case '─':
			return SlotBorderH
		}
		return SlotBorderH // title text glyphs sit on the top border line
	case kindBorderMid:
		switch r {
		case '├':
			return SlotJoinL
		case '┤':
			return SlotJoinR
		}
		return SlotBorderH
	case kindBorderBot:
		switch r {
		case '╰':
			return SlotCornerBL
		case '╯':
			return SlotCornerBR
		}
		return SlotBorderH
	}
	// Card body row: first/last col are walls, second-to-last is the
	// status indicator slot. width is the configured grid width; rune
	// count may differ for narrow rows but card bodies are padded to
	// width by renderRowsBeauty.
	if col == 0 {
		return SlotBorderV
	}
	if col == totalCols-1 {
		return SlotBorderV
	}
	if col == totalCols-2 {
		return SlotStatusInd
	}
	return SlotPlain
}

// serialize collapses the grid back into one styled string per row.
// Adjacent cells with identical (Style pointer, Italic flag) are grouped
// into a single Render call to keep ANSI volume close to today's output.
func serialize(g *Grid) []string {
	out := make([]string, len(g.Cells))
	for i, cells := range g.Cells {
		if cells == nil {
			out[i] = ""
			continue
		}
		out[i] = serializeRow(cells)
	}
	return out
}

func serializeRow(cells []Cell) string {
	var sb, seg strings.Builder
	start := 0
	for i := 1; i <= len(cells); i++ {
		if i == len(cells) || !cellStyleEqual(cells[i-1], cells[i]) {
			seg.Reset()
			for _, c := range cells[start:i] {
				seg.WriteRune(c.R)
			}
			s := seg.String()
			st := cells[start].Style
			if st == nil {
				sb.WriteString(s)
			} else if cells[start].Italic {
				styled := st.Italic(true)
				sb.WriteString(styled.Render(s))
			} else {
				sb.WriteString(st.Render(s))
			}
			start = i
		}
	}
	return sb.String()
}

func cellStyleEqual(a, b Cell) bool {
	return a.Style == b.Style && a.Italic == b.Italic
}

// serializeWithCache serializes the grid, reusing cached strings for rows
// whose cells are identical to the previous frame. On a typical animation
// tick only 2-4 rows change (spinner glyph, march highlight), so this
// skips ~80% of Render calls. Updates *cache in place for the next frame.
func serializeWithCache(g *Grid, cache **cachedGrid) []string {
	out := make([]string, len(g.Cells))
	prev := *cache
	for i, cells := range g.Cells {
		if cells == nil {
			out[i] = ""
			continue
		}
		if prev != nil && i < len(prev.cells) && rowCellsEqual(cells, prev.cells[i]) {
			out[i] = prev.lines[i]
			continue
		}
		out[i] = serializeRow(cells)
	}
	// Snapshot current cells for next frame's comparison. Deep copy so
	// future styleCells mutations don't corrupt the cache.
	cached := &cachedGrid{
		cells: make([][]Cell, len(g.Cells)),
		lines: out,
	}
	for i, cells := range g.Cells {
		if cells == nil {
			continue
		}
		cp := make([]Cell, len(cells))
		copy(cp, cells)
		cached.cells[i] = cp
	}
	*cache = cached
	return out
}

// rowCellsEqual reports whether two cell slices are visually identical
// (same runes, styles, and italic flags). Used by the per-row dirty check.
func rowCellsEqual(a, b []Cell) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].R != b[i].R || a[i].Style != b[i].Style || a[i].Italic != b[i].Italic {
			return false
		}
	}
	return true
}
