package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/llkhacquan/qmux/sidebar-go/internal/sidebarstate"
)

const sidebarPaneTitle = "Sidebar"

// windowKeyForID converts @123 → w123.
func windowKeyForID(windowID string) string {
	return strings.Replace(windowID, "@", "w", 1)
}

// sidebarWindowOption builds a per-window option name.
func sidebarWindowOption(suffix, windowID string) string {
	return fmt.Sprintf("@tmux_sidebar_%s_%s", suffix, windowKeyForID(windowID))
}

// sidebarFocusRequestOption builds the focus request option name.
func sidebarFocusRequestOption(windowID string) string {
	return sidebarWindowOption("focus", windowID)
}

// optionIsEnabled checks if a tmux option value is truthy.
func optionIsEnabled(value, defaultValue string) bool {
	if value == "" {
		value = defaultValue
	}
	switch strings.ToLower(value) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// SidebarPane represents a sidebar pane found via list-panes.
type SidebarPane struct {
	PaneID   string
	WindowID string
}

// listSidebarPanes returns all panes with sidebar titles. Uses tmuxQuery so
// the daemon's focus path (syncSidebarWidth) rides the control conn with no
// fork; subcommand callers transparently fall back to a one-shot.
func listSidebarPanes() []SidebarPane {
	raw, err := tmuxQuery("list-panes", "-a", "-F", "#{pane_id}|#{pane_title}|#{window_id}")
	if err != nil {
		return nil
	}
	var result []SidebarPane
	for line := range strings.SplitSeq(raw, "\n") {
		parts := strings.SplitN(line, "|", 3)
		if len(parts) < 3 {
			continue
		}
		if sidebarTitles[parts[1]] {
			result = append(result, SidebarPane{PaneID: parts[0], WindowID: parts[2]})
		}
	}
	return result
}

// listSidebarPanesInWindow returns sidebar panes in a specific window.
func listSidebarPanesInWindow(windowID string) []SidebarPane {
	var result []SidebarPane
	for _, sp := range listSidebarPanes() {
		if sp.WindowID == windowID {
			result = append(result, sp)
		}
	}
	return result
}

// claudePaneInRows reports whether paneID is a tracked claude/codex card.
// Membership in the daemon's rows is authoritative for "is this a claude
// pane" - unlike the pane title, which the app can rewrite and which races
// sidebar setup.
func claudePaneInRows(rows []Row, paneID string) bool {
	if paneID == "" {
		return false
	}
	for _, r := range rows {
		if r.PaneID == paneID && (r.Kind == kindIntent || r.Kind == kindLocation) {
			return true
		}
	}
	return false
}

// attachedTerminalWindow returns the window_id the real attached terminal is
// viewing — the authoritative "window the user is looking at". Control-mode
// clients (the sidebar's own tmux -C conns + orphans from prior re-execs) are
// excluded: a focus hook firing in a control client's context would otherwise
// pin ActiveWindow to a window the user isn't viewing. That window stays
// window_active in its own session, so the per-session membership check can't
// tell it apart from the right one and never corrects it — the visible sidebar
// then sees windowActive=false and freezes. Most-recently-active wins when
// several real terminals are attached. "" when none are (all-detached).
func attachedTerminalWindow() string {
	raw, err := tmuxQuery("list-clients", "-F",
		"#{client_activity}|#{client_control_mode}|#{window_id}|#{client_flags}")
	if err != nil {
		return ""
	}
	return parseAttachedTerminalWindow(raw)
}

// parseAttachedTerminalWindow picks the most-recently-active real terminal's
// window from `list-clients` output formatted as
// "activity|control_mode|window_id|flags" per line. Pure for testability.
func parseAttachedTerminalWindow(raw string) string {
	best := ""
	var bestActivity int64 = -1
	for line := range strings.SplitSeq(raw, "\n") {
		parts := strings.SplitN(line, "|", 4)
		if len(parts) < 4 {
			continue
		}
		if parts[1] != "0" || !strings.Contains(parts[3], "attached") {
			continue // control-mode client, or not attached
		}
		act, _ := strconv.ParseInt(parts[0], 10, 64)
		if act > bestActivity {
			bestActivity = act
			best = parts[2]
		}
	}
	return best
}

// paneInWindow reports whether paneID has a card in the given window.
func paneInWindow(rows []Row, paneID, windowID string) bool {
	if paneID == "" || windowID == "" {
		return false
	}
	for _, r := range rows {
		if r.PaneID == paneID && r.Window == windowID {
			return true
		}
	}
	return false
}

// claudePaneInWindow returns the claude/codex card to treat as active for a
// window — the tmux-active one if present, else the first card in row order
// (stable list-panes ordering). "" when the window holds no claude card.
func claudePaneInWindow(rows []Row, windowID string) string {
	if windowID == "" {
		return ""
	}
	first := ""
	for _, r := range rows {
		if r.Window != windowID || r.PaneID == "" {
			continue
		}
		if r.Kind != kindIntent && r.Kind != kindLocation {
			continue
		}
		if r.Active {
			return r.PaneID
		}
		if first == "" {
			first = r.PaneID
		}
	}
	return first
}

// windowNonSidebarPanesCSV returns sorted CSV of non-sidebar pane IDs in a window.
func windowNonSidebarPanesCSV(windowID string) string {
	raw, err := runTmux("list-panes", "-a", "-F", "#{pane_id}|#{pane_title}|#{window_id}")
	if err != nil {
		return ""
	}
	var panes []string
	for line := range strings.SplitSeq(raw, "\n") {
		parts := strings.SplitN(line, "|", 3)
		if len(parts) < 3 {
			continue
		}
		if parts[2] == windowID && !sidebarTitles[parts[1]] {
			panes = append(panes, parts[0])
		}
	}
	sort.Strings(panes)
	return strings.Join(panes, ",")
}

// saveSidebarWindowSnapshot saves window layout and pane list for restoration.
func saveSidebarWindowSnapshot(windowID, targetPane string) {
	layoutOption := sidebarWindowOption("layout", windowID)
	panesOption := sidebarWindowOption("panes", windowID)

	layoutTarget := windowID
	if targetPane != "" {
		layoutTarget = targetPane
	}

	layout, err := runTmux("display-message", "-p", "-t", layoutTarget, "#{window_layout}")
	if err == nil && strings.TrimSpace(layout) != "" {
		runTmux("set-option", "-g", layoutOption, strings.TrimSpace(layout))
	} else {
		runTmux("set-option", "-g", "-u", layoutOption)
	}

	panes := windowNonSidebarPanesCSV(windowID)
	if panes != "" {
		runTmux("set-option", "-g", panesOption, panes)
	} else {
		runTmux("set-option", "-g", "-u", panesOption)
	}
}

// clearSidebarWindowSnapshot removes saved layout/panes for a window.
func clearSidebarWindowSnapshot(windowID string) {
	runTmux("set-option", "-g", "-u", sidebarWindowOption("layout", windowID))
	runTmux("set-option", "-g", "-u", sidebarWindowOption("panes", windowID))
}

// restoreSidebarWindowSnapshot restores layout if panes haven't changed.
func restoreSidebarWindowSnapshot(windowID string) {
	layoutOption := sidebarWindowOption("layout", windowID)
	panesOption := sidebarWindowOption("panes", windowID)

	savedLayout := tmuxOption(layoutOption)
	savedPanes := tmuxOption(panesOption)
	currentPanes := windowNonSidebarPanesCSV(windowID)

	if savedLayout != "" && savedPanes != "" && currentPanes == savedPanes {
		runTmux("select-layout", "-t", windowID, savedLayout)
	}
	clearSidebarWindowSnapshot(windowID)
}

// clearSidebarStateOptions removes all per-window sidebar state options.
func clearSidebarStateOptions() {
	raw, err := runTmux("show-options", "-g")
	if err != nil {
		return
	}
	re := regexp.MustCompile(`^(@tmux_sidebar_(?:pane|creating|layout|panes|focus)_w\S+)`)
	for line := range strings.SplitSeq(raw, "\n") {
		if m := re.FindStringSubmatch(line); m != nil {
			runTmux("set-option", "-g", "-u", m[1])
		}
	}
}

// clearSidebarWindowStateOptions removes state options for a specific window.
func clearSidebarWindowStateOptions(windowID string) {
	runTmux("set-option", "-g", "-u", sidebarWindowOption("pane", windowID))
	runTmux("set-option", "-g", "-u", sidebarWindowOption("creating", windowID))
	runTmux("set-option", "-g", "-u", sidebarWindowOption("focus", windowID))
	clearSidebarWindowSnapshot(windowID)
}

// sharedState holds all data synced across sidebar instances via shared file.
type sharedState struct {
	Cursor string `json:"cursor"`
	Active string `json:"active"`
	// ActiveWindow is the tmux window_id of the focused pane. Lets the
	// sidebar quiet-blue a Claude card that shares the live window even when
	// the focused pane is a non-Claude console (so the card isn't Active but
	// still sits in the window the user is working in).
	ActiveWindow string          `json:"active_window,omitempty"`
	LastActive   string          `json:"last_active,omitempty"`
	Rows         []Row           `json:"rows,omitempty"`
	PaneRows     []Row           `json:"pane_rows,omitempty"`
	Scrolloff    int             `json:"scrolloff"`
	UnreadPanes  map[string]bool `json:"unread,omitempty"`
	// DonePanes tracks panes that just transitioned running→idle and haven't
	// been visited yet. Set by the leader on transition; cleared by
	// cmdOnFocus when the user actually switches to the pane. Lives in
	// shared state (not just the leader's memory) so the sticky bit
	// survives leader switches and so the focus hook — which runs as a
	// separate sidebar-go invocation — can clear it directly.
	// Value is the unix-millis timestamp of the transition; used by the
	// "Nm ago" border label. Map presence still means "this pane is done".
	DonePanes map[string]int64 `json:"done_panes,omitempty"`
	// AskedPanes tracks panes where the last assistant message was a
	// question. Subset of DonePanes — both maps have the same timestamp.
	// The border label shows "asked 2m ago" instead of "done 2m ago".
	// Cleared alongside DonePanes by cmdOnFocus / active-state transitions.
	AskedPanes map[string]int64 `json:"asked_panes,omitempty"`
	// RunningPanes tracks the running-start unix-millis timestamp per
	// pane so the working border label can show the elapsed duration
	// ("crafting (2m) ⠋"). Set on first observation of Status=="running"
	// and cleared the moment the pane leaves running — flicker (running
	// → "" → running) intentionally resets the clock; debounce later if
	// it shows up in practice. Mirrors DonePanes' map-presence semantics.
	RunningPanes map[string]int64 `json:"running_panes,omitempty"`
	// ViewYOffset / ViewPinned: scroll position shared across sidebar
	// instances so switching tmux session/window doesn't snap the user to
	// a different viewable region. Only meaningful while ViewPinned=true
	// (user actively wheel-scrolled / Ctrl+D/U); a normal cursor nav
	// flips ViewPinned=false and every instance resumes cursor-tracking.
	ViewYOffset int  `json:"view_y_offset,omitempty"`
	ViewPinned  bool `json:"view_pinned,omitempty"`
	// HiddenSessions are tmux sessions the user hid from the sidebar via the
	// right-click workspace menu. View-only: filtered out of each instance's
	// rendered rows, but their panes/daemon refresh/hooks keep running. Seeded
	// from @tmux_sidebar_hidden_sessions at daemon boot; toggles update both.
	HiddenSessions []string `json:"hidden_sessions,omitempty"`
	Timestamp      int64    `json:"ts"`
}

// readSharedState reads full shared state from disk.
func readSharedState() sharedState {
	data, err := sidebarstate.ReadRaw()
	if err != nil {
		return sharedState{}
	}
	var s sharedState
	if json.Unmarshal(data, &s) != nil {
		return sharedState{}
	}
	return s
}

// writeSharedState atomically writes full shared state and fans out a UDS
// doorbell to every peer listener. The doorbell is what makes fsnotify the
// *fallback* path: every existing writer (writeSharedCursorActive,
// publishRowsCmd, persistDonePanes, ...) goes through here, so peers wake
// in <1ms instead of waiting for kqueue (which drops bursty events on macOS).
// See docs/ipc-uds-notify.md.
func writeSharedState(s sharedState) {
	s.Timestamp = time.Now().UnixMilli()
	data, err := json.Marshal(s)
	if err != nil {
		return
	}
	_ = sidebarstate.WriteRaw(data)
}

// withSharedStateLock serializes read-modify-write of shared-state across
// every sidebar-* process AND every goroutine within one process. Without
// this, three known RMW sites (writeSharedCursorActive, persistDonePanes,
// publishRowsCmd) interleave and clobber each other — leader publishes
// fresh Rows, persistDonePanes reads pre-publish state, persistDonePanes
// writes back with stale Rows + new DonePanes, leader's fresh Rows lost.
func withSharedStateLock(fn func()) { sidebarstate.WithLock(fn) }

// writeSharedCursorActive writes cursor + active without overwriting rows.
// Skips the write entirely when both fields already match — every write
// triggers fsnotify in every peer sidebar, which wakes their Update loop.
// During a pane switch, tmux fires 5+ focus hooks; without this dedupe each
// hook would write identical values and storm the watcher channel.
//
// Also tracks LastActive: when active genuinely transitions (old != new and
// old non-empty), the prior active is promoted to LastActive. This is the
// single source of truth for the "◂ last active" hint — peer sidebars no
// longer derive it from local active-pane transitions, which used to diverge
// across instances depending on startup order and missed updates.
// activeWindow is the focused pane's tmux window_id; "" leaves the stored
// value untouched (callers reacting to a sidebar nav action don't know it).
func writeSharedCursorActive(cursor, active, activeWindow string) {
	withSharedStateLock(func() {
		s := readSharedState()
		winSame := activeWindow == "" || s.ActiveWindow == activeWindow
		if s.Cursor == cursor && s.Active == active && winSame {
			return
		}
		if active != s.Active && s.Active != "" {
			s.LastActive = s.Active
		}
		s.Cursor = cursor
		s.Active = active
		if activeWindow != "" {
			s.ActiveWindow = activeWindow
		}
		writeSharedState(s)
	})
}

// runningSubagents returns the types of subagents currently in flight for
// the given tmux pane (i.e. the parent claude session living in that pane).
// Reads sidebar-bg-track.cjs's per-pane ledger at
// ~/.local/state/tmux-sidebar/bg-{paneID}.json. Empty/missing → nil.
//
// We deliberately don't read $TMPDIR/ck-session-{sid}.json's
// statusline.agents — that field is built from the transcript by
// transcript-parser.cjs, which marks an agent "completed" the moment a
// tool_result block is emitted for the spawn. Background subagents emit
// tool_result instantly (the spawn confirmation), so the running window
// is microseconds and the field is effectively a historical log, not a
// live in-flight set. The dedicated SubagentStart/Stop hook
// (sidebar-bg-track.cjs) is the only reliable real-time source.
//
// Keyed by tmux pane id rather than session id because the hook gets
// $TMUX_PANE from the parent claude's process tree — that's the most
// stable identifier; a single pane may even host multiple claude
// sessions over time and they all share the same paneID.
func runningSubagents(paneID string) []string {
	if paneID == "" {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(stateDir(), "bg-"+paneID+".json"))
	if err != nil {
		return nil
	}
	var s struct {
		Agents []struct {
			Type string `json:"type"`
		} `json:"agents"`
	}
	if json.Unmarshal(data, &s) != nil {
		return nil
	}
	var out []string
	for _, a := range s.Agents {
		if a.Type != "" {
			out = append(out, a.Type)
		}
	}
	return out
}

// clearSharedUnread removes a pane from the shared unread set.
func clearSharedUnread(paneID string) {
	s := readSharedState()
	if len(s.UnreadPanes) > 0 {
		delete(s.UnreadPanes, paneID)
		writeSharedState(s)
	}
}

// clearSharedDone removes a pane from the shared done set. Currently
// unwired — cmdOnFocus used to call this so focus dropped the badge,
// but the spec changed: "done" persists until the user engages (sends a
// new prompt), which the leader detects via status→running and clears
// in tree.go. Kept around as the obvious hook point if a manual-dismiss
// keybind ever lands.
func clearSharedDone(paneID string) {
	s := readSharedState()
	if _, ok := s.DonePanes[paneID]; !ok {
		return
	}
	delete(s.DonePanes, paneID)
	delete(s.AskedPanes, paneID)
	writeSharedState(s)
}

// activePaneID returns the currently-active main pane, preferring shared
// state over the legacy tmux option. The UDS doorbell makes shared state
// the source of truth on the focus path; the option lookup is kept as a
// cold-start fallback (cmdToggle and the various user-action writers still
// keep it populated, so it works before the first focus event lands).
//
// Hot path was a tmux fork (~50ms); now it's a single file read (~1ms).
// Across the five reader sites that used to call this every render tick,
// that's the difference between sidebar refresh feeling instant and lagging.
func activePaneID() string {
	if a := readSharedState().Active; a != "" {
		return a
	}
	return tmuxOption("@tmux_sidebar_main_pane")
}

// Convenience wrappers
func readCursorFile() string { return readSharedState().Cursor }
func writeCursorFile(paneID string) {
	writeSharedCursorActive(paneID, readSharedState().Active, "")
}

// writeSharedView persists the viewport scroll position so peer sidebar
// instances render the same viewable region after a session/window switch.
// RMW under the same flock as cursor/active to avoid clobbering parallel
// writers; skips the write entirely if both fields already match (every
// wheel tick fires this — without dedupe we'd storm the doorbell with
// no-op fan-outs).
func writeSharedView(yOffset int, pinned bool) {
	withSharedStateLock(func() {
		s := readSharedState()
		// Offset only matters while pinned — normalize unpinned to 0 so a
		// stale offset doesn't get re-applied if a peer toggles back on.
		if !pinned {
			yOffset = 0
		}
		if s.ViewYOffset == yOffset && s.ViewPinned == pinned {
			return
		}
		s.ViewYOffset = yOffset
		s.ViewPinned = pinned
		writeSharedState(s)
	})
}
