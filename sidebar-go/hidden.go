package main

import (
	"sort"
	"strings"
)

// Per-workspace (tmux session) hide. Hidden workspaces vanish from the sidebar
// view only — their Claude panes, the daemon refresh, and every hook keep
// running untouched. State persists in the global tmux option below and rides
// the daemon snapshot (sharedState.HiddenSessions) so the whole sidebar fleet
// converges. Toggled from the right-click workspace menu (see tea_view.go).
const hiddenSessionsOption = "@tmux_sidebar_hidden_sessions"

// workspaceItem is one row in the right-click show/hide menu.
type workspaceItem struct {
	Name   string
	Count  int // visible cards (one per Claude pane) in this session
	Hidden bool
}

// loadHiddenSessions reads the persisted hidden set from the tmux option.
// One tmux fork — call at startup only, never on the hot path.
func loadHiddenSessions() map[string]bool {
	return hiddenCSVToSet(tmuxOption(hiddenSessionsOption))
}

// writeHiddenOption persists the hidden set to the global tmux option. An empty
// set unsets the option so a stale value never lingers.
func writeHiddenOption(list []string) {
	val := strings.Join(list, ",")
	if val == "" {
		runTmux("set-option", "-g", "-u", hiddenSessionsOption)
		return
	}
	runTmux("set-option", "-g", hiddenSessionsOption, val)
}

// seedHiddenSessions copies the persisted tmux option into shared state at
// daemon boot so the first snapshot already carries the hidden set. Later
// toggles update both (see applyIntent actionToggleHidden).
func seedHiddenSessions() {
	withSharedStateLock(func() {
		s := readSharedState()
		s.HiddenSessions = sortedHiddenSlice(loadHiddenSessions())
		writeSharedState(s)
	})
}

func hiddenCSVToSet(raw string) map[string]bool {
	set := make(map[string]bool)
	for name := range strings.SplitSeq(raw, ",") {
		if n := strings.TrimSpace(name); n != "" {
			set[n] = true
		}
	}
	return set
}

func hiddenSliceToSet(list []string) map[string]bool {
	set := make(map[string]bool, len(list))
	for _, n := range list {
		if n != "" {
			set[n] = true
		}
	}
	return set
}

func sortedHiddenSlice(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// filterHiddenRows drops every card belonging to a hidden session. A card spans
// kindBorderTop..kindBorderBot; the border rows carry no Session, so we resolve
// the card's session from its first content row and skip the whole span when
// hidden. Rows outside a card (none today, but be defensive) pass through.
func filterHiddenRows(rows []Row, hidden map[string]bool) []Row {
	if len(hidden) == 0 {
		return rows
	}
	out := make([]Row, 0, len(rows))
	for i := 0; i < len(rows); {
		if rows[i].Kind != kindBorderTop {
			// A standalone session header (none emitted today) for a hidden
			// session is dropped; everything else passes.
			if !(rows[i].Kind == kindSession && hidden[rows[i].Session]) {
				out = append(out, rows[i])
			}
			i++
			continue
		}
		// Card: scan to its closing border, learning the session on the way.
		j, sess := i, ""
		for j < len(rows) {
			if sess == "" && rows[j].Session != "" {
				sess = rows[j].Session
			}
			if rows[j].Kind == kindBorderBot {
				break
			}
			j++
		}
		if j >= len(rows) {
			j = len(rows) - 1 // malformed tail: treat rest as one card
		}
		if !hidden[sess] {
			out = append(out, rows[i:j+1]...)
		}
		i = j + 1
	}
	return out
}

// summarizeWorkspaces builds the menu list from the UNFILTERED rows so hidden
// sessions still appear (with Hidden=true) and can be toggled back on. One
// kindIntent row is emitted per pane, so counting them counts cards. Order
// follows first appearance, which already reflects the session sort order.
func summarizeWorkspaces(rows []Row, hidden map[string]bool) []workspaceItem {
	var order []string
	counts := make(map[string]int)
	for _, r := range rows {
		if r.Kind != kindIntent || r.Session == "" {
			continue
		}
		if _, seen := counts[r.Session]; !seen {
			order = append(order, r.Session)
		}
		counts[r.Session]++
	}
	out := make([]workspaceItem, 0, len(order))
	for _, name := range order {
		out = append(out, workspaceItem{Name: name, Count: counts[name], Hidden: hidden[name]})
	}
	return out
}
