package main

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

func TestPerimeterIndex(t *testing.T) {
	w, h := 10, 5
	total := perimeterTotal(w, h)
	if total != 26 {
		t.Fatalf("perimeterTotal(10,5) = %d, want 26", total)
	}

	cases := []struct {
		rowOff, col, want int
	}{
		{0, 0, 0},  // top-left corner
		{0, 9, 9},  // top-right corner
		{1, 9, 10}, // first right-wall body row
		{3, 9, 12}, // last right-wall body row
		{4, 9, 13}, // bot-right corner
		{4, 0, 22}, // bot-left corner
		{3, 0, 23}, // first left-wall body row (closest to bottom)
		{1, 0, 25}, // last left-wall body row (closest to top)
		{2, 5, -1}, // interior cell
	}
	for _, c := range cases {
		got := perimeterIndex(c.rowOff, c.col, h, w)
		if got != c.want {
			t.Errorf("perimeterIndex(%d,%d,%d,%d) = %d, want %d",
				c.rowOff, c.col, h, w, got, c.want)
		}
	}

	// Sanity: walking the full perimeter clockwise visits each index
	// exactly once.
	seen := make(map[int]int)
	for rowOff := range h {
		for col := range w {
			p := perimeterIndex(rowOff, col, h, w)
			if p >= 0 {
				seen[p]++
			}
		}
	}
	if len(seen) != total {
		t.Fatalf("expected %d unique perimeter cells, got %d", total, len(seen))
	}
	for p, count := range seen {
		if count != 1 {
			t.Errorf("perimeter index %d visited %d times", p, count)
		}
	}
}

func TestInLitWindow(t *testing.T) {
	total := 26
	segLen := 4

	// Head at 5: lit cells = {5, 4, 3, 2}.
	for _, p := range []int{2, 3, 4, 5} {
		if !inLitWindow(p, 5, total, segLen) {
			t.Errorf("p=%d should be lit at head=5", p)
		}
	}
	for _, p := range []int{0, 1, 6, 25} {
		if inLitWindow(p, 5, total, segLen) {
			t.Errorf("p=%d should NOT be lit at head=5", p)
		}
	}

	// Wrap-around: head at 1 → segment = {1, 0, 25, 24}.
	for _, p := range []int{1, 0, 25, 24} {
		if !inLitWindow(p, 1, total, segLen) {
			t.Errorf("p=%d should be lit at head=1 (wrap)", p)
		}
	}
	for _, p := range []int{2, 23} {
		if inLitWindow(p, 1, total, segLen) {
			t.Errorf("p=%d should NOT be lit at head=1", p)
		}
	}
}

// TestPickMarchStyle covers the per-pane suppression rules:
// needs-input → nil; done → bright yellow comet; working → green;
// active idle → nil (active state moved entirely onto the gold top-
// border title); non-active idle → nil.
func TestPickMarchStyle(t *testing.T) {
	cases := []struct {
		name      string
		pid       string
		wantNil   bool
		wantStyle *lipgloss.Style
	}{
		{"active idle → suppressed", "%1", true, nil},
		{"needs-input → suppressed", "%2", true, nil},
		{"working → green", "%3", false, &styleWorkingMarch},
		{"done → suppressed", "%4", true, nil},
		{"non-active idle → suppressed", "%5", true, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := buildFixtureModel()
			st := buildRenderState(m, m.cursorPaneID)
			got := pickMarchStyle(c.pid, st)
			if c.wantNil {
				if got != nil {
					t.Errorf("pickMarchStyle(%q) = %v, want nil", c.pid, got)
				}
				return
			}
			if got == nil {
				t.Errorf("pickMarchStyle(%q) = nil, want non-nil", c.pid)
				return
			}
			if got != c.wantStyle {
				t.Errorf("pickMarchStyle(%q) returned wrong style pointer", c.pid)
			}
		})
	}
}

// TestMarchActivePhaseGate confirms marchActive returns false during the
// rest portion of the cycle and true during the sweep when at least one
// pane qualifies. Fixture has working (%3) and done (%4) panes, both of
// which still march after the active-idle lavender comet was retired.
func TestMarchActivePhaseGate(t *testing.T) {
	m := buildFixtureModel()

	m.spinFrame = 0 // sweep
	st := buildRenderState(m, m.cursorPaneID)
	if !marchActive(st) {
		t.Errorf("sweep frame: marchActive = false, want true (working+done in fixture)")
	}

	m.spinFrame = marchSweepFrames + 1 // rest portion of cycle
	st = buildRenderState(m, m.cursorPaneID)
	if marchActive(st) {
		t.Errorf("rest frame: marchActive = true, want false")
	}
}

// TestActiveCardHasBlueFrame asserts the active card wears the focus
// colors (lavender banner on top, blue on the rest) and uses double-line
// box-drawing runes for the thicker focused-card silhouette.
func TestActiveCardHasBlueFrame(t *testing.T) {
	m := buildFixtureModel()
	m.spinFrame = marchSweepFrames + 1 // rest frame — no comet noise
	out := m.composeStyledLines()

	// Catppuccin blue (#89B4FA) → truecolor `137;179;250`; lavender
	// (#B4BEFE) → `180;190;254`. The top border row uses the brighter
	// banner variant, sides + bot keep blue.
	const blueRGB = "137;179;250"
	const lavenderRGB = "179;190;254"

	if !strings.Contains(out[1], lavenderRGB) {
		t.Errorf("expected lavender banner on active card top border (row 1), got: %q", out[1])
	}
	if len(out) <= 6 || !strings.Contains(out[6], blueRGB) {
		t.Errorf("expected blue on active card bot border (row 6), got: %q", out[6])
	}
	// Double-line box-drawing: top-left corner should be ╔ (not ╭) on the
	// active card. Same idea on the bot — ╚ instead of ╰. Bumped from
	// heavy single (┏┗) to double (╔╚) for stronger visual emphasis.
	if !strings.Contains(out[1], "╔") {
		t.Errorf("expected double top-left ╔ on active card top border, got: %q", out[1])
	}
	if len(out) > 6 && !strings.Contains(out[6], "╚") {
		t.Errorf("expected double bot-left ╚ on active card bot border, got: %q", out[6])
	}
}
