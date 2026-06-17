package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Row kinds for the flat card layout.
const (
	kindSession    = "session"
	kindIntent     = "intent"
	kindIntentCont = "intent_cont"
	kindLocation   = "location"
	kindGit        = "git"
	kindSpacer     = "spacer"
	kindBorderTop  = "border_top" // ╭───╮
	kindBorderMid  = "border_mid" // ├───┤
	kindBorderBot  = "border_bot" // ╰───╯
	kindPreview    = "preview"    // last output preview line
)

// isCardRow returns true for row kinds that belong to a card (inside borders).
func isCardRow(kind string) bool {
	return kind == kindIntent || kind == kindIntentCont || kind == kindLocation || kind == kindGit || kind == kindPreview
}



// Row represents a single display row in the sidebar.
type Row struct {
	Kind    string `json:"kind"`
	Text    string `json:"text"`
	PaneID  string `json:"pane_id,omitempty"`
	Session string `json:"session,omitempty"`
	Window  string `json:"window,omitempty"`
	Active  bool   `json:"active,omitempty"`
	Status  string `json:"status,omitempty"`
	// Asked is true when status=="done" but the last assistant message
	// was a question. Border label shows "asked 2m ago" instead of "2m ago".
	Asked bool `json:"asked,omitempty"`
	// Verb is Claude's live status word ("Crafting…", "Pondering…", …)
	// captured at load time. Render reads it off the location row to draw
	// the border label.
	Verb string `json:"verb,omitempty"`
	// StatusSince is the unix-millis timestamp when the current Status
	// was first observed, so the border label can show "Nm ago" relative
	// to now. Currently populated for Status=="done" (running→idle
	// transition); the field is general so future states (running start
	// time, needs-input asking duration) can reuse it. Zero when unknown.
	StatusSince int64 `json:"status_since,omitempty"`
}

// lastActiveWindows is set by loadTree to the set of window_ids with
// window_active=1 (one per attached session). The daemon's reloadTree
// uses it to detect and correct stale ActiveWindow after focus-hook races.
var lastActiveWindows map[string]bool

// RawPane holds data from tmux list-panes.
type RawPane struct {
	Session     string
	WindowID    string
	WindowIndex int
	WindowName  string
	PaneID      string
	Command     string
	Title       string
	Active      bool
	Path        string
	State       *PaneState
	App         string
}

func sessionIcon(name string) string {
	if icon, ok := Cfg().Icons.Sessions[name]; ok {
		return icon
	}
	return ""
}

// workspaceIconForPath returns the workspace icon for a given absolute path,
// or empty string if no workspace matches.
func workspaceIconForPath(path string) string {
	if ws := Cfg().MatchWorkspace(path); ws != nil {
		return ws.Icon
	}
	return ""
}

const (
	intentPrefix     = "  󰧑 "
	intentContPrefix = "    "
	previewPrefix    = "  "
)


// paneCapture holds cached capture-pane output for a single pane.
type paneCapture struct {
	short string // -S -6 (status bar)
	long  string // -S -100 (status detection)
}

// capturedPanes is the current batch of captured pane content.
//
// Concurrency: prefetchPaneCaptures replaces the whole map at the top of
// loadTree, then getCapturedPane reads it from the same loadTree pass.
// Two loadTree goroutines can overlap (slow tick + next tick) — without
// the mutex the swap-vs-read pair triggers Go's "concurrent map read and
// map write" fatal trap, identical shape to the claudeSignalsCache crash
// we caught in production. RWMutex: read-heavy on getCapturedPane.
var (
	capturedPanes   map[string]*paneCapture
	capturedPanesMu sync.RWMutex
)

// prefetchPaneCaptures runs capture-pane for all pane IDs in parallel.
// Single capture per pane (-S -100); short view derived by taking last 6 lines.
func prefetchPaneCaptures(paneIDs []string) {
	result := make(map[string]*paneCapture, len(paneIDs))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, id := range paneIDs {
		wg.Add(1)
		pid := id
		safeGo("tree.prefetchPaneCapture", func() {
			defer wg.Done()
			pc := &paneCapture{}
			if out, err := tmuxQuery("capture-pane", "-pt", pid, "-S", "-20", "-J"); err == nil {
				pc.long = out
				pc.short = lastNLines(out, 6)
			}
			mu.Lock()
			result[pid] = pc
			mu.Unlock()
		})
	}
	wg.Wait()
	capturedPanesMu.Lock()
	capturedPanes = result
	capturedPanesMu.Unlock()
}

func lastNLines(s string, n int) string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}

// getCapturedPane returns cached capture for a pane, or empty.
func getCapturedPane(paneID string) *paneCapture {
	capturedPanesMu.RLock()
	defer capturedPanesMu.RUnlock()
	if capturedPanes == nil {
		return nil
	}
	return capturedPanes[paneID]
}

// captureStatusBar extracts Claude Code's status bar lines from pane bottom.
// Returns lines like model info, path+branch, diff stats.
func captureStatusBar(paneID string) []string {
	var out string
	if pc := getCapturedPane(paneID); pc != nil {
		out = pc.short
	} else {
		var err error
		out, err = tmuxQuery("capture-pane", "-pt", paneID, "-S", "-6", "-J")
		if err != nil {
			return nil
		}
	}
	if out == "" {
		return nil
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")

	var result []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Keep status bar lines (model only) — path in border title
		isStatusBar := strings.Contains(line, "context)") // model line
		if isStatusBar {
			// Model line: keep only model name (e.g. "🤖 Opus 4.6")
			if strings.Contains(line, "context)") {
				// Find model name between 🤖 and "("
				if start := strings.Index(line, "🤖"); start >= 0 {
					rest := line[start:]
					if paren := strings.Index(rest, "("); paren > 0 {
						line = strings.TrimSpace(rest[:paren])
					}
				}
			}
			result = append(result, line)
		}
	}
	return result
}

// truncatePath shortens a path from the left to fit maxLen.
// ~/work/org/repo/worktree/fast → …/repo/worktree/fast
func truncatePath(path string, maxLen int) string {
	parts := strings.Split(path, "/")

	// Collapse workspace prefix to its icon.
	// path may be tilde-prefixed (~/work/...) so resolve before matching.
	absPath := resolvePath(expandHome(path))
	if ws := Cfg().MatchWorkspace(absPath); ws != nil && ws.Icon != "" {
		prefix := resolvePath(expandHome(ws.Path))
		if after, ok := strings.CutPrefix(absPath, prefix); ok {
			trimmed := strings.TrimPrefix(after, "/")
			if trimmed != "" {
				parts = append([]string{ws.Icon}, strings.Split(trimmed, "/")...)
			} else {
				parts = []string{ws.Icon}
			}
		}
	}
	// Always collapse /worktree/ to a git-branch icon
	for i, p := range parts {
		if p == "worktree" {
			parts[i] = "\ue725" // nerd font: git-branch
		}
	}
	collapsed := strings.Join(parts, "/")
	if len([]rune(collapsed)) <= maxLen {
		return collapsed
	}

	// Try removing leading segments until it fits
	for i := 1; i < len(parts)-1; i++ {
		candidate := "…/" + strings.Join(parts[i+1:], "/")
		if len([]rune(candidate)) <= maxLen {
			return candidate
		}
	}
	// Last resort: just the last segment
	last := parts[len(parts)-1]
	if len([]rune(last))+2 <= maxLen {
		return "…/" + last
	}
	return last
}

// wrapText word-wraps plain text to fit a given width.
func wrapText(text string, maxWidth int) []string {
	if maxWidth <= 0 {
		return []string{text}
	}
	words := strings.Fields(text)
	var lines []string
	var current string
	for _, word := range words {
		if current == "" {
			current = word
		} else if len([]rune(current))+1+len([]rune(word)) <= maxWidth {
			current += " " + word
		} else {
			lines = append(lines, current)
			current = word
		}
	}
	if current != "" {
		lines = append(lines, current)
	}
	return lines
}

// wrapIntent word-wraps intent text to fit sidebar width.
func wrapIntent(intent string, sidebarWidth int) []string {
	firstMax := sidebarWidth - len([]rune(intentPrefix))
	contMax := sidebarWidth - len([]rune(intentContPrefix))
	if firstMax <= 0 || contMax <= 0 {
		return []string{intentPrefix + intent}
	}

	words := strings.Fields(intent)
	var lines []string
	var current string
	maxLen := firstMax

	for _, word := range words {
		if current == "" {
			current = word
		} else if len([]rune(current))+1+len([]rune(word)) <= maxLen {
			current = current + " " + word
		} else {
			prefix := intentPrefix
			if len(lines) > 0 {
				prefix = intentContPrefix
			}
			lines = append(lines, prefix+current)
			current = word
			maxLen = contMax
		}
	}
	if current != "" {
		prefix := intentPrefix
		if len(lines) > 0 {
			prefix = intentContPrefix
		}
		lines = append(lines, prefix+current)
	}
	if len(lines) == 0 {
		return []string{intentPrefix + intent}
	}
	return lines
}

// loadTree loads all Claude panes and builds flat card rows.
func loadTree() []Row {
	raw, err := tmuxQuery(
		"list-panes", "-a", "-F",
		"#{session_name}|#{window_id}|#{window_index}|#{window_name}|#{pane_id}|#{pane_current_command}|#{pane_title}|#{pane_active}|#{pane_current_path}|#{window_active}",
	)
	if err != nil {
		return nil
	}

	livePanes := make(map[string]bool)
	var allPanes []RawPane
	activeWindows := make(map[string]bool)

	for line := range strings.SplitSeq(raw, "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "|", 11)
		if len(parts) < 8 {
			continue
		}
		panePath := ""
		if len(parts) > 8 {
			panePath = parts[8]
		}
		winIdx, _ := strconv.Atoi(parts[2])
		paneID := parts[4]
		paneActive := parts[7] == "1"
		windowActive := len(parts) > 9 && parts[9] == "1"
		livePanes[paneID] = true

		if windowActive {
			activeWindows[parts[1]] = true
		}

		// Skip sidebar panes
		if sidebarTitles[parts[6]] {
			continue
		}

		allPanes = append(allPanes, RawPane{
			Session:     parts[0],
			WindowID:    parts[1],
			WindowIndex: winIdx,
			WindowName:  parts[3],
			PaneID:      paneID,
			Command:     parts[5],
			Title:       parts[6],
			Active:      paneActive,
			Path:        panePath,
		})
	}
	lastActiveWindows = activeWindows

	// Load pane state files
	paneStates := loadPaneStates(livePanes)

	// Filter to Claude/Codex panes only
	var filtered []RawPane
	for i := range allPanes {
		state := paneStates[allPanes[i].PaneID]
		app := liveAgentApp(allPanes[i].Command, allPanes[i].Title, state)
		if app == "claude" || app == "codex" {
			allPanes[i].State = state
			allPanes[i].App = app
			filtered = append(filtered, allPanes[i])
		}
	}

	// Capture is about to be re-prefetched, so any cached terminal
	// signals from the previous pass are stale.
	resetClaudeSignalsCache()

	// Prefetch git info + pane captures in parallel
	paths := make([]string, len(filtered))
	paneIDs := make([]string, len(filtered))
	for i, p := range filtered {
		paths[i] = p.Path
		paneIDs[i] = p.PaneID
	}
	// Run git info, git-status, and capture-pane prefetches concurrently.
	// safeGo: a panic in any prefetch (e.g. cache map race) would otherwise
	// kill the whole sidebar process — bubbletea's Cmd recover doesn't
	// reach inner goroutines spawned from a Cmd. Recovery + log instead.
	var prefetchWg sync.WaitGroup
	prefetchWg.Add(3)
	safeGo("tree.prefetchGitInfo", func() { defer prefetchWg.Done(); prefetchGitInfo(paths) })
	safeGo("tree.prefetchGitStatusCounts", func() {
		defer prefetchWg.Done()
		dirtyPaths := filterDirtyGitPaths(paths)
		if len(dirtyPaths) > 0 {
			prefetchGitStatusCounts(dirtyPaths)
		}
	})
	safeGo("tree.prefetchPaneCaptures", func() { defer prefetchWg.Done(); prefetchPaneCaptures(paneIDs) })
	prefetchWg.Wait()

	// GH PR prefetch: fire-and-forget, reads from disk cache to avoid duplicate API calls.
	// Disabled: causes gh API rate limits with many panes.
	const enableGHPR = false
	if enableGHPR {
		safeGo("tree.prefetchGHPR", func() {
			var refs []BranchRef
			for _, p := range paths {
				root := gitRootFor(p)
				_, branch := gitInfo(p)
				if root != "" && branch != "" {
					refs = append(refs, BranchRef{Root: root, Branch: branch})
				}
			}
			prefetchGHPR(refs)
		})
	}

	// Sort by session order, then window index
	order := configuredSessionOrder()
	orderMap := make(map[string]int, len(order))
	for i, name := range order {
		orderMap[name] = i
	}
	fallback := len(order)
	slices.SortFunc(filtered, func(a, b RawPane) int {
		ai, aok := orderMap[a.Session]
		if !aok {
			ai = fallback
		}
		bi, bok := orderMap[b.Session]
		if !bok {
			bi = fallback
		}
		if ai != bi {
			return ai - bi
		}
		if a.Session != b.Session {
			return strings.Compare(a.Session, b.Session)
		}
		return a.WindowIndex - b.WindowIndex
	})

	// Build card rows — each card is an independent box
	// Use actual pane width if available, fall back to configured width
	sidebarWidth := configuredSidebarWidth() - 1
	if paneID := tmuxPaneID(); paneID != "" {
		if out, err := tmuxQuery("display-message", "-p", "-t", paneID, "#{pane_width}"); err == nil {
			if pw, err := strconv.Atoi(strings.TrimSpace(out)); err == nil && pw > 0 {
				sidebarWidth = pw
			}
		}
	}
	var rows []Row

	// Read last Claude pane for "(last)" indicator
	// ⏎ glyph marks the previously-active Claude pane. Reads
	// sharedState.LastActive (same source as the ◂ body glyph) so the two
	// markers stay in sync. The old @tmux_sidebar_last_claude_pane option
	// drifted because focusCursorPaneCmd's early-return guard prevented
	// cmdOnFocus from updating it on cursor-driven Enters.
	ss := readSharedState()
	lastClaudePane := ss.LastActive

	// Sticky "done" plumbing.
	//
	// prevStatus comes from previously-published rows so we can spot the
	// running→idle edge. Sourcing prev from shared rows (instead of a
	// per-process map) keeps detection alive across leader switches —
	// the perf change moved loadTree to whichever sidebar's window is
	// active, so per-process state would reset on every window-switch.
	//
	// donePanes is the actual sticky set, also in shared state. It's
	// cleared by cmdOnFocus (the focus hook runs as a separate sidebar-go
	// invocation, so it needs a shared mutation point — not a leader
	// in-memory map). This separation is what makes the badge survive
	// the user being on the pane during the transition: pane.Active is
	// no longer treated as an auto-clear.
	prevStatus := map[string]string{}
	for _, r := range ss.Rows {
		if r.PaneID != "" && (r.Kind == kindLocation || r.Kind == kindIntent) && r.Status != "" {
			prevStatus[r.PaneID] = r.Status
		}
	}
	donePanes := ss.DonePanes
	if donePanes == nil {
		donePanes = map[string]int64{}
	}
	askedPanes := ss.AskedPanes
	if askedPanes == nil {
		askedPanes = map[string]int64{}
	}
	runningPanes := ss.RunningPanes
	if runningPanes == nil {
		runningPanes = map[string]int64{}
	}

	for _, pane := range filtered {
		// Top border with path (shortened with ~ then truncated to fit).
		// Prefer Claude's project directory (from statusline JSON) over
		// tmux pane_current_path - the shell CWD can differ when claude
		// was launched from a different repo than it's working in.
		effectivePath := pane.Path
		if cw := globalCtxWatcher.Load(); cw != nil {
			if cc := cw.ForPane(pane.PaneID); cc != nil && cc.Cwd != "" {
				effectivePath = cc.Cwd
			}
		}
		_, branch := gitInfo(effectivePath)
		displayPath := effectivePath
		if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(displayPath, home) {
			displayPath = "~" + displayPath[len(home):]
		}
		borderLabel := ""
		if displayPath != "" {
			// Only show marker for "last claude" pane; no folder icon.
			prefix := ""
			if pane.PaneID == lastClaudePane {
				prefix = "⏎ "
			}
			// Pass a generous cap so truncatePath keeps its collapse rules
			// (~/work/<org>→icon, /worktree→branch glyph, …/ shortening)
			// but stops short of pre-trimming the branch tail. The
			// renderRowsBeauty marquee handles overflow at draw time so
			// long branch names don't permanently mask the verb label.
			maxPathLen := sidebarWidth * 4
			displayPath = truncatePath(displayPath, maxPathLen)
			borderLabel = " " + prefix + displayPath + " "
		}
		rows = append(rows, Row{Kind: kindBorderTop, Text: borderLabel})

		// Compute status up front — used for the location badge below and
		// to swap the intent line for a whimsical verb while running.
		status := effectivePaneStatus(pane.PaneID, pane.Command, pane.Title, pane.State)

		// Sticky "done" — when a pane transitions running→idle we flag it
		// so the user notices the turn finished (✅ badge + yellow band).
		// The flag clears on cmdOnFocus when the user actually switches
		// to the pane. Active claude states (running/needs-input/error)
		// also clear it — claude resumed, no point flagging "done".
		// The map value is the transition timestamp (unix-millis), used
		// by the border label to render "Nm ago" relative to now.
		prev := prevStatus[pane.PaneID]
		if prev == "running" && status == "" {
			now := time.Now().UnixMilli()
			donePanes[pane.PaneID] = now
			// Check transcript: if the last assistant message was a
			// question, mark as "asked" so the border label shows
			// "asked 2m ago" instead of "done 2m ago".
			if cw := globalCtxWatcher.Load(); cw != nil {
				if cc := cw.ForPane(pane.PaneID); cc != nil {
					tp := transcriptPath(pane.Path, cc.SessionID)
					if tp != "" {
						if text := lastAssistantText(tp); text != "" && looksLikeQuestion(text) {
							askedPanes[pane.PaneID] = now
						}
					}
				}
			}
		}
		if status == "running" || status == "needs-input" || status == "error" {
			delete(donePanes, pane.PaneID)
			delete(askedPanes, pane.PaneID)
		}
		if status == "" {
			if _, isDone := donePanes[pane.PaneID]; isDone {
				status = "done"
			}
		}

		// Surface Claude's live status verb ("Crafting…", "Pondering…",
		// "Combobulating…") when the pane is mid-response. Try first
		// regardless of status: newer Claude versions stopped printing
		// "esc to interrupt" so the legacy running-detection paths miss
		// the active state, but the verb-with-tokens line is itself a
		// reliable "actively generating" signal — promote to running
		// whenever we find a verb.
		verb := extractClaudeWorkingVerb(pane.PaneID)
		if verb != "" && status == "" {
			status = "running"
		}

		// Sticky running-start timestamp — set on first observation,
		// cleared the moment the pane leaves running. Drives the
		// "(2m)" suffix on the working border label.
		if status == "running" {
			if _, ok := runningPanes[pane.PaneID]; !ok {
				runningPanes[pane.PaneID] = time.Now().UnixMilli()
			}
		} else {
			delete(runningPanes, pane.PaneID)
		}
		// Intent line shows the user's actual task. Three sources tried in
		// order, first non-empty wins:
		//  1. AI-summarized title (intent-title.py UserPromptSubmit hook)
		//     — refreshed on every prompt, so it tracks the live task.
		//  2. Claude Code's pane_title — frozen at session boot, goes stale
		//     after /clear but is the only signal when the hook is off.
		//  3. Literal "Claude Code" — last resort.
		// We need the Claude session_id to look up (1); the ContextWatcher's
		// per-pane ck-context already carries that mapping.
		intent := ""
		if cw := globalCtxWatcher.Load(); cw != nil {
			if cc := cw.ForPane(pane.PaneID); cc != nil {
				intent = cw.TitleForSession(cc.SessionID)
			}
		}
		if intent == "" {
			intent = extractClaudeIntent(pane.Title)
		}
		if intent == "" {
			intent = Cfg().Agent.Name
		}
		intentLines := wrapIntent(intent, sidebarWidth)
		for i, lineText := range intentLines {
			kind := kindIntent
			if i > 0 {
				kind = kindIntentCont
			}
			rows = append(rows, Row{
				Kind:    kind,
				Text:    lineText,
				PaneID:  pane.PaneID,
				Session: pane.Session,
				Window:  pane.WindowID,
			})
		}

		cardCfg := Cfg().Card

		var gitParts []string
		if cardCfg.ShowGit {
			if branch != "" {
				gitParts = append(gitParts, "🌿 "+branch)
			}
			changed, unpushed := gitStatusCounts(effectivePath)
			if changed > 0 {
				gitParts = append(gitParts, fmt.Sprintf("%d changed", changed))
			}
			if unpushed > 0 {
				gitParts = append(gitParts, fmt.Sprintf("%d unpushed", unpushed))
			}
		}

		var prParts []string
		if cardCfg.ShowPR {
			if pr := ghPRInfo(gitRootFor(effectivePath), branch); pr != nil {
				prText := fmt.Sprintf("#%d", pr.Number)
				if pr.AllPass {
					prText += " ✓"
				} else if len(pr.Failed) > 0 {
					for _, name := range pr.Failed {
						prText += " " + name + "✗"
					}
				} else if len(pr.Pending) > 0 {
					names := strings.Join(pr.Pending, ", ")
					prText += fmt.Sprintf(" %d running (%s)", len(pr.Pending), names)
				}
				switch pr.Review {
				case "approved":
					prText += " 👍"
				case "changes_requested":
					prText += " ✏️"
				case "review_required":
					prText += " 👀"
				}
				prParts = append(prParts, prText)
			}
		}

		for _, lineParts := range [][]string{gitParts, prParts} {
			if len(lineParts) == 0 {
				continue
			}
			infoText := strings.Join(lineParts, " · ")
			for _, iLine := range wrapText(infoText, sidebarWidth-4) {
				rows = append(rows, Row{
					Kind:    kindPreview,
					Text:    previewPrefix + iLine,
					PaneID:  pane.PaneID,
					Session: pane.Session,
					Window:  pane.WindowID,
				})
			}
		}

		if cw := globalCtxWatcher.Load(); cw != nil {
			if cc := cw.ForPane(pane.PaneID); cc != nil {
				if cardCfg.ShowContext {
					ctxLine := formatContextLine(cc)
					rows = append(rows, Row{
						Kind:    kindPreview,
						Text:    previewPrefix + ctxLine,
						PaneID:  pane.PaneID,
						Session: pane.Session,
						Window:  pane.WindowID,
					})
				}

				if cardCfg.ShowIntent && status == "running" {
					if in := cw.IntentForSession(cc.SessionID); in != nil {
						actionLine := formatIntentLine(in, sidebarWidth-5)
						rows = append(rows, Row{
							Kind:    kindPreview,
							Text:    previewPrefix + actionLine,
							PaneID:  pane.PaneID,
							Session: pane.Session,
							Window:  pane.WindowID,
						})
					}
				}

				if cardCfg.ShowShells {
					if shells := extractClaudeShells(pane.PaneID); shells > 0 {
						noun := "shells"
						if shells == 1 {
							noun = "shell"
						}
						badge := fmt.Sprintf("🐚 %d %s", shells, noun)
						rows = append(rows, Row{
							Kind:    kindPreview,
							Text:    previewPrefix + badge,
							PaneID:  pane.PaneID,
							Session: pane.Session,
							Window:  pane.WindowID,
						})
					}
				}

				if cardCfg.ShowSubagents {
					if agents := runningSubagents(pane.PaneID); len(agents) > 0 {
						badge := fmt.Sprintf("🤖 %d: %s", len(agents), strings.Join(agents, ", "))
						for _, line := range wrapText(badge, sidebarWidth-4) {
							rows = append(rows, Row{
								Kind:    kindPreview,
								Text:    previewPrefix + line,
								PaneID:  pane.PaneID,
								Session: pane.Session,
								Window:  pane.WindowID,
							})
						}
					}
				}
			}
		}

		var statusSince int64
		switch status {
		case "done":
			statusSince = donePanes[pane.PaneID]
		case "running":
			statusSince = runningPanes[pane.PaneID]
		}
		_, isAsked := askedPanes[pane.PaneID]

		// Attach status metadata to the first intent row (always present)
		// so border rendering sees it regardless of which optional lines
		// are enabled. Scan backward from current position to find it.
		for j := len(rows) - 1; j >= 0; j-- {
			if rows[j].PaneID == pane.PaneID && rows[j].Kind == kindIntent {
				rows[j].Active = pane.Active
				rows[j].Status = status
				rows[j].Asked = isAsked && status == "done"
				rows[j].Verb = verb
				rows[j].StatusSince = statusSince
				break
			}
		}

		if cardCfg.ShowLocation {
			icon := sessionIcon(pane.Session)
			bdg := badgeForStatus(status)
			location := fmt.Sprintf("  %s %s:%s", icon, pane.Session, pane.WindowName)
			if bdg != "" {
				location += "  " + bdg
			}
			rows = append(rows, Row{
				Kind:    kindLocation,
				Text:    location,
				PaneID:  pane.PaneID,
				Session: pane.Session,
				Window:  pane.WindowID,
			})
		}

		// Bottom border
		rows = append(rows, Row{Kind: kindBorderBot})
	}

	// Persist DonePanes/RunningPanes mutations. publishRowsCmd writes Rows
	// separately; keeping these as a one-off write here means we don't
	// have to thread the sets through rowsLoadedMsg. One lock + one write
	// covers both maps — every shared-state write storms fsnotify on peer
	// sidebars, so coalescing them halves the wakeup count.
	persistStickyTimestamps(donePanes, askedPanes, runningPanes)

	return rows
}

// persistStickyTimestamps writes the leader's done + asked + running
// timestamp maps to shared state in one RMW. Skips the write when no map
// changed — every shared-state write fans out a fsnotify+UDS doorbell
// to every peer sidebar, so the common "nothing transitioned this tick"
// case must be a no-op or peers churn pointlessly.
func persistStickyTimestamps(done, asked, running map[string]int64) {
	withSharedStateLock(func() {
		s := readSharedState()
		doneChanged := !mapEqualInt64(s.DonePanes, done)
		askedChanged := !mapEqualInt64(s.AskedPanes, asked)
		runChanged := !mapEqualInt64(s.RunningPanes, running)
		if !doneChanged && !askedChanged && !runChanged {
			return
		}
		if doneChanged {
			s.DonePanes = done
		}
		if askedChanged {
			s.AskedPanes = asked
		}
		if runChanged {
			s.RunningPanes = running
		}
		writeSharedState(s)
	})
}

func mapEqualInt64(a, b map[string]int64) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// loadTreeFast returns Claude panes in sidebar order without git/terminal capture.
// Used for fast operations like switch-next/prev where we only need pane IDs.
func loadTreeFast() []Row {
	raw, err := runTmux(
		"list-panes", "-a", "-F",
		"#{session_name}|#{window_id}|#{window_index}|#{window_name}|#{pane_id}|#{pane_current_command}|#{pane_title}|#{pane_active}",
	)
	if err != nil {
		return nil
	}

	var allPanes []RawPane
	for line := range strings.SplitSeq(raw, "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "|", 8)
		if len(parts) < 8 {
			continue
		}
		if sidebarTitles[parts[6]] {
			continue
		}
		winIdx, _ := strconv.Atoi(parts[2])
		allPanes = append(allPanes, RawPane{
			Session:     parts[0],
			WindowID:    parts[1],
			WindowIndex: winIdx,
			WindowName:  parts[3],
			PaneID:      parts[4],
			Command:     parts[5],
			Title:       parts[6],
			Active:      parts[7] == "1",
		})
	}

	// Filter to Claude/Codex only (no state files — just command/title detection)
	var filtered []RawPane
	for _, pane := range allPanes {
		app := liveAgentApp(pane.Command, pane.Title, nil)
		if app == "claude" || app == "codex" {
			filtered = append(filtered, pane)
		}
	}

	// Sort by session order
	order := configuredSessionOrder()
	orderMap := make(map[string]int, len(order))
	for i, name := range order {
		orderMap[name] = i
	}
	fallback := len(order)
	slices.SortFunc(filtered, func(a, b RawPane) int {
		ai, aok := orderMap[a.Session]
		if !aok { ai = fallback }
		bi, bok := orderMap[b.Session]
		if !bok { bi = fallback }
		if ai != bi { return ai - bi }
		if a.Session != b.Session { return strings.Compare(a.Session, b.Session) }
		return a.WindowIndex - b.WindowIndex
	})

	// Return minimal rows with pane ID, session, window
	var rows []Row
	for _, pane := range filtered {
		rows = append(rows, Row{
			Kind:    kindIntent,
			PaneID:  pane.PaneID,
			Session: pane.Session,
			Window:  pane.WindowID,
		})
	}
	return rows
}

// loadPaneStates reads pane state JSON files, cleaning up stale ones.
func loadPaneStates(livePanes map[string]bool) map[string]*PaneState {
	states := make(map[string]*PaneState)
	dir := stateDir()
	entries, err := filepath.Glob(filepath.Join(dir, "pane-*.json"))
	if err != nil {
		return states
	}
	for _, path := range entries {
		base := filepath.Base(path)
		pid := strings.TrimPrefix(strings.TrimSuffix(base, ".json"), "pane-")
		if !livePanes[pid] {
			os.Remove(path)
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var s PaneState
		if json.Unmarshal(data, &s) == nil {
			states[pid] = &s
		}
	}
	return states
}

// truncateLine truncates a line to width, adding "…" if needed.
func truncateLine(line string, width int) string {
	if width <= 0 {
		return ""
	}
	runes := []rune(line)
	if len(runes) <= width {
		return line
	}
	if width == 1 {
		return "…"
	}
	return string(runes[:width-1]) + "…"
}


// dumpRender outputs the non-interactive text render to stdout.
func dumpRender() {
	// Init context watcher for one-shot read (no persistent watch needed)
	notify := make(chan struct{}, 1)
	if cw, err := NewContextWatcher(notify); err == nil {
		globalCtxWatcher.Store(cw)
		defer cw.Stop()
	}
	rows := loadTree()
	selected := activePaneID()
	width := configuredSidebarWidth() - 1
	// Beauty is the only renderer post-bubbletea. The classic ASCII path was
	// dropped along with tcell — anyone still depending on it should pipe
	// through `sed` to remap glyphs.
	lines := renderRowsBeauty(rows, selected, selected, width, 0, nil)
	fmt.Println(strings.Join(lines, "\n"))
}

// paneRowsFor returns one row per unique pane (first row with pane_id).
func paneRowsFor(rows []Row) []Row {
	seen := make(map[string]bool)
	var result []Row
	for _, row := range rows {
		if row.PaneID != "" && !seen[row.PaneID] {
			result = append(result, row)
			seen[row.PaneID] = true
		}
	}
	return result
}

// reconcileSelectedPane ensures the selected pane still exists.
func reconcileSelectedPane(selectedPaneID string, paneRows []Row) string {
	if len(paneRows) == 0 {
		return ""
	}
	for _, row := range paneRows {
		if row.PaneID == selectedPaneID {
			return selectedPaneID
		}
	}
	// Try to find a pane in the same window
	if strings.HasPrefix(selectedPaneID, "%") {
		windowID, err := tmuxQuery("display-message", "-p", "-t", selectedPaneID, "#{window_id}")
		if err == nil {
			windowID = strings.TrimSpace(windowID)
			for _, row := range paneRows {
				if row.Window == windowID {
					return row.PaneID
				}
			}
		}
	}
	return paneRows[0].PaneID
}

// findSelectedRowIndex returns the index of the intent row for the selected pane.
func findSelectedRowIndex(rows []Row, selectedPaneID string) int {
	for i, row := range rows {
		if row.PaneID == selectedPaneID && row.Kind == kindIntent {
			return i
		}
	}
	// Fallback: any row with matching pane_id
	for i, row := range rows {
		if row.PaneID == selectedPaneID {
			return i
		}
	}
	return -1
}

// findSearchMatches returns row indices matching the query (case-insensitive).
func findSearchMatches(rows []Row, query string) map[int]bool {
	if query == "" {
		return nil
	}
	q := strings.ToLower(query)
	matches := make(map[int]bool)
	for i, row := range rows {
		if strings.Contains(strings.ToLower(row.Text), q) {
			matches[i] = true
		}
	}
	return matches
}

// nextSearchMatch navigates to the next/prev pane matching the search.
func nextSearchMatch(rows []Row, selectedPaneID string, matches map[int]bool, direction int) string {
	type target struct {
		idx    int
		paneID string
	}
	seenPanes := make(map[string]bool)
	var targets []target
	sorted := make([]int, 0, len(matches))
	for i := range matches {
		sorted = append(sorted, i)
	}
	slices.Sort(sorted)

	for _, i := range sorted {
		row := rows[i]
		paneID := row.PaneID
		if paneID == "" {
			// Look forward for a pane_id
			for j := i + 1; j < len(rows); j++ {
				if rows[j].PaneID != "" {
					paneID = rows[j].PaneID
					break
				}
			}
		}
		if paneID != "" && !seenPanes[paneID] {
			targets = append(targets, target{i, paneID})
			seenPanes[paneID] = true
		}
	}
	if len(targets) == 0 {
		return selectedPaneID
	}

	currentIdx := -1
	for i, row := range rows {
		if row.PaneID == selectedPaneID {
			currentIdx = i
			break
		}
	}

	if direction == 1 {
		for _, t := range targets {
			if t.idx > currentIdx {
				return t.paneID
			}
		}
		return targets[0].paneID
	}
	for i := len(targets) - 1; i >= 0; i-- {
		if targets[i].idx < currentIdx {
			return targets[i].paneID
		}
	}
	return targets[len(targets)-1].paneID
}

// ensureVisible adjusts scroll_offset so row_index is visible.
func ensureVisible(rowIndex, scrollOffset, visibleLines, scrolloff int) int {
	if rowIndex < 0 || visibleLines <= 0 {
		return 0
	}
	margin := min(scrolloff, max(0, (visibleLines-1)/2))
	if rowIndex < scrollOffset+margin {
		return max(0, rowIndex-margin)
	}
	if rowIndex >= scrollOffset+visibleLines-margin {
		return rowIndex - visibleLines + 1 + margin
	}
	return scrollOffset
}
