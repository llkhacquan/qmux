package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Pre-compiled regex for subcommands.
var (
	paneIDRe         = regexp.MustCompile(`^%\d+$`)
	titleStatusFmtRe = regexp.MustCompile(`:\s*(done|needs-input|error)\s*$`)
)

// cmdToggle implements the toggle-sidebar logic.
func cmdToggle() {
	currentWindow, _ := runTmux("display-message", "-p", "#{window_id}")
	currentWindow = strings.TrimSpace(currentWindow)
	enabled := tmuxOption("@tmux_sidebar_enabled")
	sidebarPanes := listSidebarPanes()

	// Reconcile: enabled but no sidebar panes → disable
	if enabled == "1" && len(sidebarPanes) == 0 {
		runTmux("set-option", "-g", "@tmux_sidebar_enabled", "0")
		if currentWindow != "" {
			clearSidebarWindowStateOptions(currentWindow)
		}
		enabled = "0"
	}

	// If enabled and sidebar exists in this window → close
	if enabled == "1" && currentWindow != "" {
		if len(listSidebarPanesInWindow(currentWindow)) > 0 {
			cmdClose("", currentWindow)
			return
		}
	}

	// Save current pane as main pane (if not a sidebar)
	currentPane, _ := runTmux("display-message", "-p", "#{pane_id}")
	currentPane = strings.TrimSpace(currentPane)
	currentTitle, _ := runTmux("display-message", "-p", "#{pane_title}")
	currentTitle = strings.TrimSpace(currentTitle)
	if currentPane != "" && !sidebarTitles[currentTitle] {
		runTmux("set-option", "-g", "@tmux_sidebar_main_pane", currentPane)
	}

	// Enable and open
	runTmux("set-option", "-g", "@tmux_sidebar_enabled", "1")
	focusOnOpen := tmuxOption("@tmux_sidebar_focus_on_open")
	if currentWindow != "" && optionIsEnabled(focusOnOpen, "1") {
		runTmux("set-option", "-g", sidebarFocusRequestOption(currentWindow), "1")
	}
	cmdEnsure("", currentWindow)
}

// cmdEnsure creates or finds the sidebar pane in the given window.
func cmdEnsure(targetPane, currentWindow string) {
	enabled := tmuxOption("@tmux_sidebar_enabled")
	if enabled != "1" {
		return
	}

	// Resolve target pane and window
	if targetPane == "" && currentWindow == "" {
		out, _ := runTmux("display-message", "-p", "#{pane_id}")
		targetPane = strings.TrimSpace(out)
	}
	if currentWindow == "" && targetPane != "" {
		out, _ := runTmux("display-message", "-p", "-t", targetPane, "#{window_id}")
		currentWindow = strings.TrimSpace(out)
	}
	if currentWindow == "" {
		out, _ := runTmux("display-message", "-p", "#{window_id}")
		currentWindow = strings.TrimSpace(out)
	}
	if currentWindow == "" {
		return
	}

	paneOption := sidebarWindowOption("pane", currentWindow)
	creatingOption := sidebarWindowOption("creating", currentWindow)
	focusOption := sidebarFocusRequestOption(currentWindow)
	ensureLock := fmt.Sprintf("@tmux_sidebar_ensure_%s", windowKeyForID(currentWindow))

	// Acquire lock
	runTmux("wait-for", "-L", ensureLock)
	defer func() {
		runTmux("set-option", "-g", "-u", creatingOption)
		runTmux("set-option", "-g", "-u", focusOption)
		runTmux("wait-for", "-U", ensureLock)
	}()

	// Check if stored sidebar pane still exists in this window
	storedPane := tmuxOption(paneOption)
	if storedPane != "" {
		raw, _ := runTmux("list-panes", "-a", "-F", "#{pane_id}|#{pane_title}|#{window_id}")
		for line := range strings.SplitSeq(raw, "\n") {
			parts := strings.SplitN(line, "|", 3)
			if len(parts) == 3 && parts[0] == storedPane && sidebarTitles[parts[1]] && parts[2] == currentWindow {
				return // Sidebar already exists
			}
		}
		runTmux("set-option", "-g", "-u", paneOption)
	}

	// Check for any existing sidebar pane in this window
	existing := listSidebarPanesInWindow(currentWindow)
	if len(existing) > 0 {
		runTmux("set-option", "-g", paneOption, existing[0].PaneID)
		return
	}

	// `creating==1` should be unreachable here because the only path that
	// sets it (line 152 below) holds the wait-for ensureLock for the
	// duration. Since we just acquired that same lock, any leftover "1"
	// is a sentinel from a previous cmdEnsure process that crashed/was
	// SIGKILL'd between line 152 and the deferred -u at line 95. Clear
	// it so this window can ever ensure again — without this branch the
	// sidebar would be permanently un-creatable in that window until the
	// next tmux server restart.
	if tmuxOption(creatingOption) == "1" {
		fmt.Fprintf(os.Stderr, "[%s] cmdEnsure: stale creating flag for %s (prior crash); clearing\n",
			time.Now().Format("15:04:05"), currentWindow)
		runTmux("set-option", "-g", "-u", creatingOption)
	}

	// Sidebar width from shared state (single source of truth)
	sidebarWidth := strconv.Itoa(configuredSidebarWidth())

	// Resolve current pane
	currentPane := targetPane
	if currentPane == "" {
		raw, _ := runTmux("list-panes", "-t", currentWindow, "-F", "#{pane_id}|#{pane_active}")
		for line := range strings.SplitSeq(raw, "\n") {
			parts := strings.SplitN(line, "|", 2)
			if len(parts) == 2 && parts[1] == "1" {
				currentPane = parts[0]
				break
			}
		}
	}
	if currentPane == "" {
		out, _ := runTmux("display-message", "-p", "#{pane_id}")
		currentPane = strings.TrimSpace(out)
	}

	// Shell wrapper prints a placeholder instantly (shell is EDR-whitelisted,
	// zero delay) then exec's into sidebar-go display (~0.4s EDR scan). The
	// placeholder stays visible until bubbletea enters alt-screen.
	sidebarBin := os.Getenv("HOME") + "/.local/bin/sidebar-go"
	sidebarCmd := fmt.Sprintf("printf '\\033[2m loading…\\033[0m' && exec %s display", sidebarBin)
	focusSidebar := tmuxOption(focusOption)

	// Save window snapshot before split
	saveSidebarWindowSnapshot(currentWindow, currentPane)
	runTmux("set-option", "-g", creatingOption, "1")

	// Split window to create sidebar pane
	args := []string{"split-window", "-h", "-b", "-d", "-f", "-l", sidebarWidth, "-P", "-F", "#{pane_id}"}
	if currentPane != "" {
		args = []string{"split-window", "-t", currentPane, "-h", "-b", "-d", "-f", "-l", sidebarWidth, "-P", "-F", "#{pane_id}", sidebarCmd}
	} else {
		args = append(args, sidebarCmd)
	}

	out, err := runTmux(args...)
	if err != nil {
		return
	}
	sidebarPane := strings.TrimSpace(out)

	// Configure sidebar pane
	runTmux("set-option", "-p", "-t", sidebarPane, "allow-set-title", "off")
	runTmux("select-pane", "-t", sidebarPane, "-T", sidebarPaneTitle)
	runTmux("set-option", "-g", paneOption, sidebarPane)

	// Focus handling
	if focusSidebar == "1" {
		runTmux("select-pane", "-t", sidebarPane)
	} else if currentPane != "" {
		runTmux("select-pane", "-t", currentPane)
	}
}

// cmdClose closes the sidebar and restores window layout.
func cmdClose(_, targetWindow string) {
	enabled := tmuxOption("@tmux_sidebar_enabled")
	if enabled != "1" {
		return
	}

	sidebarPanes := listSidebarPanes()
	runTmux("set-option", "-g", "@tmux_sidebar_enabled", "0")

	if len(sidebarPanes) == 0 {
		if targetWindow != "" {
			restoreSidebarWindowSnapshot(targetWindow)
			runTmux("set-option", "-g", "-u", sidebarWindowOption("pane", targetWindow))
		}
		clearSidebarStateOptions()
		return
	}

	for _, sp := range sidebarPanes {
		runTmux("kill-pane", "-t", sp.PaneID)
		if sp.WindowID != "" {
			restoreSidebarWindowSnapshot(sp.WindowID)
		}
	}

	if targetWindow != "" {
		restoreSidebarWindowSnapshot(targetWindow)
		runTmux("set-option", "-g", "-u", sidebarWindowOption("pane", targetWindow))
	}
	clearSidebarStateOptions()
}

// trackFocus records a focus change: active-pane highlight, cursor, the
// @tmux_sidebar_main_pane option, and the per-pane state file. Shared by the
// forked `on-focus` subcommand (cmdOnFocus) and the daemon's fork-free
// focus.sock handler, so both paths track identically.
//
// Returns whether the caller should still run cmdEnsure (lazy sidebar create).
// It mirrors the original cmdOnFocus control flow exactly: false on a dedup
// early-out or a focus into a non-claude pane (no new sidebar warranted), true
// otherwise. The daemon ignores the return — it never creates panes.
func trackFocus(paneID, windowID string) (runEnsure bool) {
	// Tmux can fire more than one focus hook for a single switch (e.g.
	// after-select-window also nudges after-select-pane). Early-out when the
	// stored main pane already matches — the first signal did the work and the
	// rest would write identical values.
	//
	// BUT only when this window already has a sidebar; otherwise cmdToggle's
	// "enable globally, lazy-create per window" flow breaks: switching back to a
	// window whose main pane is already the stored main_pane would skip
	// cmdEnsure and the sidebar would never appear.
	if paneID != "" && tmuxOption("@tmux_sidebar_main_pane") == paneID {
		if windowID == "" || len(listSidebarPanesInWindow(windowID)) > 0 {
			return false
		}
	}

	if paneID != "" {
		s := readSharedState()
		// Is the focused pane a tracked claude/codex card? Rows membership is
		// authoritative (title-agnostic). On cold start the daemon hasn't
		// populated rows yet — fall back to the title heuristic so first-focus
		// still tracks before rows exist.
		isCard := claudePaneInRows(s.Rows, paneID)
		if len(s.Rows) == 0 {
			t, _ := runTmux("display-message", "-p", "-t", paneID, "#{pane_title}")
			isCard = !sidebarTitles[strings.TrimSpace(t)]
		}

		if !isCard {
			// Focused pane isn't a claude card (sidebar, editor, shell, ...).
			// Same-window early-return: skip the heavy list-panes path when
			// the user is just moving into the sidebar/editor within the
			// same window. BUT only when Active actually lives in this
			// window - a stale Active from a previous session would
			// otherwise never get corrected (reloadTree self-heals on
			// the next tick, but this avoids the 1s gap).
			if windowID == "" || (windowID == s.ActiveWindow && paneInWindow(s.Rows, s.Active, windowID)) {
				debugLog("trackFocus: same-window early-return pane=%s win=%s activeWin=%s", paneID, windowID, s.ActiveWindow)
				return false
			}
			// prefix+<n> switched windows and landed on a non-claude pane (tmux
			// remembers the sidebar/editor as this window's active pane). Move
			// the active highlight to this window's claude card so it follows
			// the switch; if the window has none, still advance active_window
			// so the in-window frame tracks the user's current window.
			if mp := claudePaneInWindow(s.Rows, windowID); mp != "" {
				runTmux("set-option", "-g", "@tmux_sidebar_main_pane", mp)
				writeSharedCursorActive(mp, mp, windowID)
			} else {
				writeSharedCursorActive(s.Cursor, s.Active, windowID)
			}
			return false
		}

		runTmux("set-option", "-g", "@tmux_sidebar_main_pane", paneID)
		// Sync cursor + active to focused pane — sidebar instantly follows session switches.
		// writeSharedCursorActive auto-promotes the prior Active to LastActive,
		// which drives both the ⏎ border glyph and the ◂ body glyph + powers cmdSwitchLast.
		writeSharedCursorActive(paneID, paneID, windowID)
		// NOTE: deliberately NOT calling clearSharedDone(paneID) here.
		// "Done" should persist until the user actually engages — i.e.
		// submits a new prompt — not merely glances at the pane. Leader
		// drops the flag when status flips back to running/needs-input
		// (tree.go), so typing a new prompt clears it the next tick.
		// Switching panes to read output keeps the yellow flashing as
		// an acknowledgement nag.
		// clearSharedDone(paneID)

		// Handle pane state on focus
		if paneIDRe.MatchString(paneID) {
			dir := stateDir()
			stateFile := filepath.Join(dir, "pane-"+paneID+".json")
			if _, err := os.Stat(stateFile); err == nil {
				clearTerminalPaneState(stateFile)
			} else {
				title, _ := runTmux("display-message", "-p", "-t", paneID, "#{pane_title}")
				if titleStatusFmtRe.MatchString(strings.TrimSpace(title)) {
					// Create initial state file
					os.MkdirAll(dir, 0o755)
					state := fmt.Sprintf(`{"pane_id":"%s","app":"claude","status":"idle","updated_at":%d}`, paneID, time.Now().Unix())
					tmp := stateFile + ".tmp"
					os.WriteFile(tmp, []byte(state), 0o644)
					os.Rename(tmp, stateFile)
				}
			}
		}
	}
	return true
}

// cmdOnFocus handles pane focus changes — tracks active pane and (lazily)
// ensures the sidebar exists. The tracking half lives in trackFocus, shared
// with the daemon's fork-free focus.sock handler; cmdOnFocus adds the ensure +
// width-sync that only the forked hook path performs.
func cmdOnFocus(paneID, windowID string) {
	enabled := tmuxOption("@tmux_sidebar_enabled")
	runEnsure := trackFocus(paneID, windowID)
	if enabled != "1" {
		return
	}
	if runEnsure {
		cmdEnsure(paneID, windowID)
	}
	// Width-sync runs on EVERY focus hook, not just when trackFocus reports a
	// change. A terminal/client resize in another session reflows this window's
	// sidebar; switching back here fires the hook with an unchanged active pane
	// (trackFocus dedups → false), yet the width still needs correcting. Gating
	// the resize on runEnsure was the drift bug.
	syncSidebarWidth(windowID)
}

// syncSidebarWidth resizes windowID's sidebar pane back to the configured
// width when it has drifted (terminal resize, layout change). Shared by the
// forked on-focus path and the daemon's fork-free focus.sock handler. Uses
// tmuxQuery so the daemon rides its control conn (zero fork) while a forked
// subcommand falls back to a one-shot. Targets the pane by id (-t), not the
// client, so it is safe to run from the detached daemon.
func syncSidebarWidth(windowID string) {
	if windowID == "" {
		return
	}
	sidebars := listSidebarPanesInWindow(windowID)
	if len(sidebars) == 0 {
		return
	}
	targetWidth := configuredSidebarWidth()
	cur, err := tmuxQuery("display-message", "-t", sidebars[0].PaneID, "-p", "#{pane_width}")
	if err != nil {
		return
	}
	if cw, _ := strconv.Atoi(strings.TrimSpace(cur)); cw > 0 && cw != targetWidth {
		tmuxQuery("resize-pane", "-t", sidebars[0].PaneID, "-x", strconv.Itoa(targetWidth))
	}
}

// prevWindowWidths caches window widths between syncAllSidebarWidths calls.
// Used to distinguish terminal resize (window width changed) from user drag
// (window width stable, sidebar width changed). Only an active window's
// sidebar can "adopt" a new width; all others snap back.
var prevWindowWidths = map[string]int{}

// syncAllSidebarWidths re-pins every sidebar pane to the configured width,
// with one exception: if an active window's sidebar drifted while the window
// width stayed stable, that's a user border-drag - adopt it as the new
// configured width and propagate to all other windows.
//
// Terminal resize changes window widths; pane-border drag does not. This is
// the discriminator - no hooks or socat needed.
//
// Active window is derived from tmux directly (window_active + session_attached)
// rather than shared state, which can be stale when the focused pane isn't a
// Claude card.
func syncAllSidebarWidths() {
	target := configuredSidebarWidth()
	raw, err := tmuxQuery("list-panes", "-a", "-F",
		"#{pane_id}|#{pane_title}|#{pane_width}|#{window_id}|#{window_width}|#{window_active}|#{session_attached}")
	if err != nil {
		return
	}

	curWindowWidths := map[string]int{}
	activeWindows := map[string]bool{}
	nonSidebarCount := map[string]int{}
	type sidebarInfo struct {
		paneID   string
		width    int
		windowID string
	}
	var sidebars []sidebarInfo

	for line := range strings.SplitSeq(raw, "\n") {
		parts := strings.SplitN(line, "|", 7)
		if len(parts) < 7 {
			continue
		}
		wID := parts[3]
		if ww, _ := strconv.Atoi(parts[4]); ww > 0 {
			curWindowWidths[wID] = ww
		}
		if parts[5] == "1" && parts[6] != "0" {
			activeWindows[wID] = true
		}
		if !sidebarTitles[parts[1]] {
			nonSidebarCount[wID]++
			continue
		}
		if w, _ := strconv.Atoi(parts[2]); w > 0 {
			sidebars = append(sidebars, sidebarInfo{parts[0], w, wID})
		}
	}

	for _, sb := range sidebars {
		// orphan: no non-sidebar panes left in this window
		if nonSidebarCount[sb.windowID] == 0 {
			debugLog("syncAllSidebarWidths: orphan kill %s in window %s", sb.paneID, sb.windowID)
			tmuxQuery("kill-pane", "-t", sb.paneID)
			clearSidebarWindowStateOptions(sb.windowID)
			continue
		}
		if sb.width == target {
			continue
		}
		windowStable := prevWindowWidths[sb.windowID] == curWindowWidths[sb.windowID] && prevWindowWidths[sb.windowID] > 0
		if activeWindows[sb.windowID] && windowStable && sb.width >= 10 {
			debugLog("syncAllSidebarWidths: adopt %d (was %d) from active %s pane %s",
				sb.width, target, sb.windowID, sb.paneID)
			saveSidebarWidth(sb.width)
			target = sb.width
			continue
		}
		tmuxQuery("resize-pane", "-t", sb.paneID, "-x", strconv.Itoa(target))
	}

	prevWindowWidths = curWindowWidths
}

// cmdOnExit handles pane exits — cleans up if a sidebar pane was killed,
// and kills orphaned sidebar panes whose last non-sidebar sibling just died.
//
// `pane-exited` fires while the dying pane may still appear in list-panes,
// and `#{hook_window}` is often empty (the pane is already detached). To
// avoid retrying for tmux to settle, we exclude `dyingPane` from the count
// when deciding "are there only sidebar panes left in this window?".
func cmdOnExit(dyingPane, windowID string) {
	enabled := tmuxOption("@tmux_sidebar_enabled")
	if enabled == "1" && dyingPane != "" && windowID != "" {
		trackedOption := sidebarWindowOption("pane", windowID)
		if tmuxOption(trackedOption) == dyingPane {
			// SCOPE: clean up ONLY this window, then respawn the sidebar
			// in place. Previously this branch disabled the global flag
			// and killed every sidebar in every window — so one process
			// panicking in one window took down the entire fleet and
			// required a manual prefix+T to recover.
			//
			// Respawn instead: cmdEnsure detects the stale option, clears
			// it, and splits a fresh sidebar pane. Closes the "watchdog"
			// gap where a sidebar dying in a window the user is sitting
			// in (no client-attached/select-window event) would leave a
			// dead-pointer @tmux_sidebar_pane_w<window> until manual
			// toggle.
			fmt.Fprintf(os.Stderr, "[%s] cmdOnExit: registered sidebar %s died in %s, respawning\n",
				time.Now().Format("15:04:05"), dyingPane, windowID)
			clearSidebarWindowStateOptions(windowID)
			cmdEnsure("", windowID)
			return
		}
	}

	// Single-shot orphan walk across all windows that host a sidebar.
	// dyingPane is subtracted from each window's pane census so we never
	// have to wait for tmux to drop the just-exited pane from list-panes.
	//
	// Exception: respawn-pane -k triggers pane-exited for the OLD process
	// but immediately starts a new one. If the pane is alive (pane_dead=0),
	// it was respawned — include it in the census so we don't false-positive
	// orphan-kill the sidebar.
	//
	// tmux 3.6 sometimes fires pane-exited with empty hook_pane (observed
	// in bursts during session/window teardown, and on natural process
	// exits like `claude` ending). Without a concrete pane to subtract we
	// used to skip — but that meant single-claude-pane windows kept a
	// dangling sidebar forever when the only event we got had empty
	// hook_pane. Sleep briefly so tmux drops the dying pane from
	// list-panes, then walk: the pane-title census is the actual signal,
	// and dying panes are gone by the time we read.
	excludePane := dyingPane
	if dyingPane == "" {
		fmt.Fprintf(os.Stderr, "[%s] cmdOnExit: empty dyingPane, deferring orphan walk\n", time.Now().Format("15:04:05"))
		time.Sleep(300 * time.Millisecond)
	} else {
		dead, err := runTmux("display-message", "-t", dyingPane, "-p", "#{pane_dead}")
		if err == nil && strings.TrimSpace(dead) == "0" {
			excludePane = ""
			fmt.Fprintf(os.Stderr, "[%s] cmdOnExit: dyingPane %s still alive (respawned), including in census\n",
				time.Now().Format("15:04:05"), dyingPane)
		}
	}
	raw, err := runTmux("list-panes", "-a", "-F", "#{pane_id}|#{pane_title}|#{window_id}")
	if err != nil {
		return
	}
	type winCensus struct{ sidebar, nonSidebar int }
	census := map[string]*winCensus{}
	sidebarsByWindow := map[string][]string{}
	for line := range strings.SplitSeq(raw, "\n") {
		parts := strings.SplitN(line, "|", 3)
		if len(parts) < 3 || parts[0] == excludePane {
			continue
		}
		w := parts[2]
		if census[w] == nil {
			census[w] = &winCensus{}
		}
		if sidebarTitles[parts[1]] {
			census[w].sidebar++
			sidebarsByWindow[w] = append(sidebarsByWindow[w], parts[0])
		} else {
			census[w].nonSidebar++
		}
	}
	for w, c := range census {
		if c.sidebar == 0 || c.nonSidebar > 0 {
			continue
		}
		fmt.Fprintf(os.Stderr, "[%s] cmdOnExit: orphan kill window=%s sidebars=%v dyingPane=%s\n",
			time.Now().Format("15:04:05"), w, sidebarsByWindow[w], dyingPane)
		for _, pid := range sidebarsByWindow[w] {
			runTmux("kill-pane", "-t", pid)
		}
		clearSidebarWindowStateOptions(w)
	}
}

// cmdFocusSidebar toggles focus between sidebar and main pane.
func cmdFocusSidebar() {
	currentTitle, _ := runTmux("display-message", "-p", "#{pane_title}")
	currentTitle = strings.TrimSpace(currentTitle)
	currentWindow, _ := runTmux("display-message", "-p", "#{window_id}")
	currentWindow = strings.TrimSpace(currentWindow)
	if currentWindow == "" {
		return
	}

	// If currently in sidebar → focus main pane
	if sidebarTitles[currentTitle] {
		mainPane := activePaneID()
		if mainPane != "" {
			mainWindow, _ := runTmux("display-message", "-p", "-t", mainPane, "#{window_id}")
			if strings.TrimSpace(mainWindow) == currentWindow {
				runTmux("select-pane", "-t", mainPane)
				return
			}
		}
		// Fallback: first non-sidebar pane in this window
		raw, _ := runTmux("list-panes", "-t", currentWindow, "-F", "#{pane_id}|#{pane_title}")
		for line := range strings.SplitSeq(raw, "\n") {
			parts := strings.SplitN(line, "|", 2)
			if len(parts) == 2 && !sidebarTitles[parts[1]] {
				runTmux("select-pane", "-t", parts[0])
				return
			}
		}
		return
	}

	// If in main pane → focus sidebar
	sidebars := listSidebarPanesInWindow(currentWindow)
	if len(sidebars) > 0 {
		runTmux("select-pane", "-t", sidebars[0].PaneID)
		return
	}

	// No sidebar yet → open with focus
	runTmux("set-option", "-g", sidebarFocusRequestOption(currentWindow), "1")
	cmdToggle()
}

// cmdInit sets up pane border format wrapping.
func cmdInit() {
	baseOption := "@tmux_sidebar_base_pane_border_format"
	titlePattern := "^(Sidebar|tmux-sidebar)$"
	wrappedFormat := fmt.Sprintf("#{?#{m/r:%s,#{pane_title}},#{pane_title},#{E:%s}}", titlePattern, baseOption)

	currentFormat := tmuxOption("pane-border-format")
	if currentFormat != wrappedFormat {
		runTmux("set-option", "-g", baseOption, currentFormat)
		runTmux("set-option", "-g", "pane-border-format", wrappedFormat)
	}

	// Post-restore recovery: detect panes titled "Sidebar" that aren't
	// running sidebar-go (zombie panes left by tmux-resurrect). Kill them
	// and re-open proper sidebars.
	raw, _ := runTmux("list-panes", "-a", "-F", "#{pane_id}|#{pane_title}|#{window_id}|#{pane_current_command}")
	windowsNeedingSidebar := map[string]bool{}
	for line := range strings.SplitSeq(raw, "\n") {
		parts := strings.SplitN(line, "|", 4)
		if len(parts) < 4 {
			continue
		}
		if !sidebarTitles[parts[1]] {
			continue
		}
		if parts[3] == "sidebar-go" {
			continue
		}
		// Zombie: titled Sidebar but running something else (bash/zsh after restore)
		runTmux("kill-pane", "-t", parts[0])
		windowsNeedingSidebar[parts[2]] = true
	}
	if len(windowsNeedingSidebar) == 0 {
		return
	}
	runTmux("set-option", "-g", "@tmux_sidebar_enabled", "1")
	for windowID := range windowsNeedingSidebar {
		cmdEnsure("", windowID)
	}
}

// cmdNotify signals all sidebar processes to refresh.
func cmdNotify() {
	enabled := tmuxOption("@tmux_sidebar_enabled")
	if enabled != "1" {
		return
	}
}

// cmdSwitchLast switches to the last Claude pane the user was at.
// Reads sharedState.LastActive and pre-flushes the {Active, LastActive}
// swap to disk *before* invoking the tmux switch. Rationale: the
// focus-hook pipeline that normally promotes Active→LastActive
// (cmdOnFocus → writeSharedCursorActive) runs async — for rapid
// leader+leader presses, the second press would otherwise see stale
// state ({Active=A, Last=B} after we already moved to B), conclude
// LastActive == currentPane, and fall through to cmdSwitchNext —
// which silently breaks the toggle. Writing the swapped state
// synchronously here makes back-to-back presses deterministic
// regardless of focus-hook latency.
//
// flock guards against concurrent invocations: tmux fires every
// `run-shell -b` as a separate process, so rapid leader+leader+leader+
// leader (Sofle thumb-combo, two F12+F12 pairs in <50ms) spawns two
// procs that read identical pre-swap state, both compute target=B,
// both switch to B — never returning to A. Holding LOCK_EX across the
// read/write/switch makes the second proc see the first proc's swap,
// so its target becomes the originally-current pane and the toggle
// behaves like a true a↔b ping-pong even under input bursts.
func cmdSwitchLast() {
	lockPath := filepath.Join(stateDir(), "switch-last.lock")
	if lf, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644); err == nil {
		defer lf.Close()
		if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX); err == nil {
			defer syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)
		}
	}

	s := readSharedState()

	// Cold start / degenerate: no remembered previous pane → behave like
	// switch-next so the first press still does something useful.
	if s.LastActive == "" || s.LastActive == s.Active {
		cmdSwitchNext()
		return
	}

	// Authoritative live pane set in ONE cheap tmux call. `list-panes -a`
	// just formats strings server-side (no per-pane subprocess, ~instant
	// even with dozens of panes), so it is far cheaper than loadTreeFast
	// (git + capture per pane). We deliberately avoid the leader-cached
	// PaneRows: right after a session is killed the cache still lists the
	// dead pane until the next publish tick — exactly when switch-last is
	// pressed. A dead pane is simply absent here, so membership doubles as
	// the liveness check and hands back the session/window to switch to.
	type paneLoc struct{ session, window string }
	live := map[string]paneLoc{}
	if out, err := runTmux("list-panes", "-a", "-F", "#{pane_id}|#{session_name}|#{window_id}"); err == nil {
		for line := range strings.SplitSeq(out, "\n") {
			if f := strings.Split(line, "|"); len(f) == 3 {
				live[f[0]] = paneLoc{f[1], f[2]}
			}
		}
	}

	target, ok := live[s.LastActive]
	if !ok {
		// The pane we'd toggle back to was killed → stay put rather than
		// jumping to an arbitrary next pane.
		return
	}
	targetPane := s.LastActive

	// Pre-flush: swap Active ↔ LastActive synchronously so a rapid second
	// press reads the correct target before the async focus hook runs.
	// Remember the current pane as the next LastActive only if it's still
	// alive — killing the *current* session leaves s.Active pointing at a
	// dead pane, and storing that would corrupt the next toggle.
	prior := s.Active
	s.Active = targetPane
	if _, alive := live[prior]; alive {
		s.LastActive = prior
	} else {
		s.LastActive = ""
	}
	s.Cursor = targetPane
	writeSharedState(s)

	// Mirror to tmux option so other call sites (cmdOnFocus dedup,
	// switchByOffset cursor) stay coherent with what we just wrote.
	runTmux("set-option", "-g", "@tmux_sidebar_main_pane", targetPane)
	switchToPane(target.session, target.window, targetPane)
}

// cmdSwitchNext switches to the next Claude pane in sidebar order.
func cmdSwitchNext() {
	switchByOffset(1)
}

// cmdSwitchPrev switches to the previous Claude pane in sidebar order.
func cmdSwitchPrev() {
	switchByOffset(-1)
}

// cmdFocusNav focuses the sidebar pane and sends an arrow key to navigate.
// direction: "up" or "down"
func cmdFocusNav(direction string) {
	// Find sidebar pane in current window
	currentWindow, _ := runTmux("display-message", "-p", "#{window_id}")
	currentWindow = strings.TrimSpace(currentWindow)
	sidebars := listSidebarPanesInWindow(currentWindow)
	if len(sidebars) == 0 {
		// No sidebar — just switch directly
		if direction == "up" {
			cmdSwitchPrev()
		} else {
			cmdSwitchNext()
		}
		return
	}

	sidebarPane := sidebars[0].PaneID
	// Focus sidebar pane
	runTmux("select-pane", "-t", sidebarPane)
	// Send arrow key to sidebar's interactive TUI
	if direction == "up" {
		runTmux("send-keys", "-t", sidebarPane, "Up")
	} else {
		runTmux("send-keys", "-t", sidebarPane, "Down")
	}
}

// switchByOffset moves ±N in the Claude pane list (wraps around).
func switchByOffset(offset int) {
	panes := claudePaneList()
	if len(panes) == 0 {
		return
	}
	current := activePaneID()

	// Find current index
	idx := -1
	for i, p := range panes {
		if p.PaneID == current {
			idx = i
			break
		}
	}

	// Calculate next index (wrap around)
	var next int
	if idx < 0 {
		next = 0
	} else {
		next = (idx + offset + len(panes)) % len(panes)
	}

	target := panes[next]
	runTmux("set-option", "-g", "@tmux_sidebar_main_pane", target.PaneID)
	// switchToPane fires tmux focus hooks → cmdOnFocus → writeSharedCursorActive
	// auto-promotes `current` to sharedState.LastActive. No manual bookkeeping.
	switchToPane(target.Session, target.Window, target.PaneID)
}

// claudePaneList returns all Claude panes in sidebar display order.
// Uses a fast path — skips git fetching and terminal capture since
// we only need pane IDs and session/window info for switching.
func claudePaneList() []Row {
	rows := loadTreeFast()
	return paneRowsFor(rows)
}

// clearTerminalPaneState resets needs-input/done status to idle.
func clearTerminalPaneState(stateFile string) {
	data, err := os.ReadFile(stateFile)
	if err != nil {
		return
	}
	var state map[string]any
	if json.Unmarshal(data, &state) != nil {
		return
	}
	status, _ := state["status"].(string)
	if status != "needs-input" && status != "done" {
		return
	}
	state["status"] = "idle"
	newData, err := json.Marshal(state)
	if err != nil {
		return
	}
	tmp := stateFile + ".tmp"
	if os.WriteFile(tmp, newData, 0o644) == nil {
		os.Rename(tmp, stateFile)
	}
}

// cmdJumpTo jumps to the Nth Claude pane in sidebar order (1-9).
func cmdJumpTo(slot string) {
	if slot == "" || slot < "1" || slot > "9" {
		return
	}
	n := int(slot[0] - '1') // '1'→0, '2'→1, etc.
	panes := claudePaneList()
	if n >= len(panes) {
		return
	}
	target := panes[n]
	runTmux("set-option", "-g", "@tmux_sidebar_main_pane", target.PaneID)
	switchToPane(target.Session, target.Window, target.PaneID)
}

// cmdWindowName outputs a formatted window name with Claude status spinner.
// Called by tmux status bar: sidebar-go window-name <path> <window_id>
func cmdWindowName(path, windowID string) {
	// Just the folder name — session context provides the rest
	name := filepath.Base(path)

	// Check shared state for Claude status in this window
	if windowID == "" {
		fmt.Print(name)
		return
	}
	ss := readSharedState()
	if ss.Timestamp == 0 || time.Now().UnixMilli()-ss.Timestamp > 5000 {
		// Shared state is stale (>5s) — no status indicator
		fmt.Print(name)
		return
	}

	// Find worst status and intent among Claude panes in this window
	// Priority: needs-input > running > idle
	worstStatus := ""
	intent := ""
	for _, row := range ss.Rows {
		if row.Window != windowID {
			continue
		}
		// Collect status from location rows
		if row.Kind == kindLocation || row.Kind == kindIntent {
			switch row.Status {
			case "needs-input":
				worstStatus = "needs-input"
			case "running":
				if worstStatus != "needs-input" {
					worstStatus = "running"
				}
			}
		}
		// Collect intent from intent rows (first one wins)
		if row.Kind == kindIntent && intent == "" {
			raw := strings.TrimSpace(row.Text)
			// Strip the icon prefix "  󰧑 "
			if idx := strings.Index(raw, " "); idx >= 0 {
				raw = strings.TrimSpace(raw[idx+1:])
			}
			if raw != "" && raw != "Claude Code" {
				intent = raw
			}
		}
	}

	// Truncate intent to keep tab compact
	if len([]rune(intent)) > 20 {
		intent = string([]rune(intent)[:19]) + "…"
	}

	// Build output: status + name + intent
	prefix := ""
	switch worstStatus {
	case "running":
		// Snowflake / sparkle set — same family the sidebar's working
		// border label cycles through (spinnerFrames in tea_legacy.go),
		// so the window-tab indicator and the sidebar card share a
		// visual vocabulary.
		frame := spinnerFrames[(time.Now().UnixMilli()/500)%int64(len(spinnerFrames))]
		prefix = string(frame) + " "
	case "needs-input":
		prefix = "❗"
	}

	if intent != "" {
		fmt.Printf("%s%s · %s", prefix, name, intent)
	} else {
		fmt.Printf("%s%s", prefix, name)
	}
}
