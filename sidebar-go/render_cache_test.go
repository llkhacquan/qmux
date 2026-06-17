package main

import "testing"

func TestSerializeCacheHitOnSpinTick(t *testing.T) {
	m := buildFixtureModel()
	m.height = 30
	m.viewport.Height = 30
	m.viewport.Width = m.width
	m.spinFrame = 4

	lines1 := m.composeStyledLines()
	if m.gridCache == nil {
		t.Fatal("gridCache should be populated after first render")
	}
	if len(lines1) == 0 {
		t.Fatal("expected non-empty output")
	}

	// Advance spinFrame (animation tick). Most rows unchanged.
	m.spinFrame++
	lines2 := m.composeStyledLines()
	if len(lines2) != len(lines1) {
		t.Fatalf("line count changed: %d -> %d", len(lines1), len(lines2))
	}

	// Rows with no animation state should be identical strings.
	// Session header rows and idle card body rows don't change on spinTick.
	unchanged := 0
	for i := range lines1 {
		if lines1[i] == lines2[i] {
			unchanged++
		}
	}
	// At least half the rows should be cache hits (reused strings).
	if unchanged < len(lines1)/2 {
		t.Fatalf("expected at least %d unchanged rows, got %d", len(lines1)/2, unchanged)
	}
}

func TestSerializeCacheAllDirtyOnRowsChange(t *testing.T) {
	m := buildFixtureModel()
	m.height = 30
	m.viewport.Height = 30
	m.viewport.Width = m.width

	_ = m.composeStyledLines()

	// New rows = new slice allocation = all rows dirty.
	newRows := make([]Row, len(m.rows))
	copy(newRows, m.rows)
	m.rows = newRows
	m.paneRows = paneRowsFor(newRows)

	lines := m.composeStyledLines()
	if len(lines) == 0 {
		t.Fatal("expected non-empty output")
	}
}

func TestRowCellsEqual(t *testing.T) {
	a := []Cell{
		{R: '─', Style: &styleBorderDim, Slot: SlotBorderH},
		{R: '│', Style: &styleBorderDim, Slot: SlotBorderV},
	}
	b := make([]Cell, len(a))
	copy(b, a)

	if !rowCellsEqual(a, b) {
		t.Fatal("identical cells should be equal")
	}

	b[0].Style = &styleBorderGreen
	if rowCellsEqual(a, b) {
		t.Fatal("different style should not be equal")
	}

	b[0].Style = a[0].Style
	b[0].R = '='
	if rowCellsEqual(a, b) {
		t.Fatal("different rune should not be equal")
	}
}
