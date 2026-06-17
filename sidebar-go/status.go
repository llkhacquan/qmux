package main

import (
	"maps"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

var (
	semverRe       = regexp.MustCompile(`^\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.-]+)?$`)
	titleStatusRe  = regexp.MustCompile(`:\s*([a-z_-]+)\s*$`)
	brailleStartRe = regexp.MustCompile(`^[\x{2800}-\x{28FF}]`)
	// Strip braille spinners and decorative symbols from intent
	intentPrefixRe = regexp.MustCompile(`^[\x{2800}-\x{28FF}✳\s]+`)
	codexRunningRe = regexp.MustCompile(`(?m)^\s*[•·]\s+Working \([^)]*esc to interrupt\)\s*$`)
	// Claude footer shells indicator. Examples:
	//   "  -- INSERT -- ⏵⏵ auto mode on · 4 shells"
	//   "  -- NORMAL -- ⏵⏵ accept edits on · 1 shell"
	// Anchored to the "· N shell(s)" suffix; the leading mode/edit text
	// varies between Claude versions and modes. Bounded to 4 digits — any
	// shell count we'd ever realistically render fits.
	shellsLineRe = regexp.MustCompile(`·\s+(\d{1,4})\s+shells?\b`)
)

// nonAgentCommands are shell/editor commands that are never Claude/Codex.
var nonAgentCommands = map[string]bool{
	"": true, "ash": true, "bash": true, "fish": true, "htop": true,
	"ksh": true, "less": true, "nano": true, "nvim": true, "sh": true,
	"ssh": true, "tail": true, "tmux": true, "top": true, "vi": true,
	"vim": true, "yazi": true, "zsh": true,
}

// cfgBadges returns badge defaults from the config file.
func cfgBadges() map[string]string {
	b := Cfg().Badges
	return map[string]string{
		"running":     b.Running,
		"needs-input": b.NeedsInput,
		"done":        b.Done,
		"error":       b.Error,
	}
}

// badgeOptionKeys maps status to tmux option name.
var badgeOptionKeys = map[string]string{
	"running":     "@tmux_sidebar_badge_running",
	"needs-input": "@tmux_sidebar_badge_needs_input",
	"done":        "@tmux_sidebar_badge_done",
	"error":       "@tmux_sidebar_badge_error",
}

// badgeCache is built once from tmux options, then read-only. sync.Once
// guarantees a single initializer wins under concurrent loadTree goroutines
// — without it, two simultaneous first-callers both see nil, both `make`
// + write, and trip Go's concurrent-map-write fatal trap.
var (
	badgeCache     map[string]string
	badgeCacheOnce sync.Once
)

// badgeForStatus returns the emoji badge for a status string.
func badgeForStatus(status string) string {
	badgeCacheOnce.Do(func() {
		badgeCache = make(map[string]string)
		maps.Copy(badgeCache, cfgBadges())
		for status, opt := range badgeOptionKeys {
			if custom := tmuxOption(opt); custom != "" {
				badgeCache[status] = custom
			}
		}
	})
	return badgeCache[status]
}

// normalizeToken lowercases and strips leading path components.
func normalizeToken(value string) string {
	token := strings.TrimSpace(strings.ToLower(value))
	if i := strings.LastIndex(token, "/"); i >= 0 {
		token = token[i+1:]
	}
	return token
}

func looksLikeClaude(value string) bool {
	token := normalizeToken(value)
	if token == "claude" || strings.HasPrefix(token, "claude-") || strings.HasPrefix(token, "claude_") {
		return true
	}
	return agentDetectRegexp().MatchString(value)
}

func looksLikeCodex(value string) bool {
	return strings.HasPrefix(normalizeToken(value), "codex")
}

func looksLikeSemver(value string) bool {
	return semverRe.MatchString(normalizeToken(value))
}

func shouldPreserveLiveLabel(command, title string) bool {
	cmd := normalizeToken(command)
	ttl := normalizeToken(title)
	return (cmd != "" && nonAgentCommands[cmd]) || (ttl != "" && nonAgentCommands[ttl])
}

// PaneState represents a pane's state JSON file content.
type PaneState struct {
	App    string `json:"app"`
	Status string `json:"status"`
}

// stateAgentApp checks state file to determine agent app.
func stateAgentApp(command, title string, state *PaneState) string {
	if state == nil {
		return ""
	}
	app := strings.TrimSpace(strings.ToLower(state.App))
	status := strings.TrimSpace(strings.ToLower(state.Status))
	if app != "claude" && app != "codex" {
		return ""
	}
	if shouldPreserveLiveLabel(command, title) {
		return ""
	}
	if app == "claude" && (looksLikeSemver(command) || looksLikeSemver(title)) {
		return "claude"
	}
	if status != "" && status != "idle" {
		return app
	}
	return ""
}

// liveAgentApp detects if a pane is running claude or codex.
func liveAgentApp(command, title string, state *PaneState) string {
	if looksLikeCodex(command) || looksLikeCodex(title) {
		return "codex"
	}
	if looksLikeClaude(command) || looksLikeClaude(title) {
		return "claude"
	}
	if looksLikeSemver(command) && !shouldPreserveLiveLabel(command, title) {
		return "claude"
	}
	return stateAgentApp(command, title, state)
}

// claudeTitleStatus extracts status from pane title suffix.
func claudeTitleStatus(title string) string {
	trimmed := strings.TrimSpace(strings.ToLower(title))
	if m := titleStatusRe.FindStringSubmatch(trimmed); m != nil {
		switch m[1] {
		case "done", "error", "needs-input", "running":
			return m[1]
		}
		return ""
	}
	if brailleStartRe.MatchString(strings.TrimSpace(title)) {
		return "running"
	}
	return ""
}

// Terminal-based status detection for Claude Code panes.
//
// Claude Code CLI doesn't expose a status API, so we infer state by scanning
// the terminal content via `tmux capture-pane`. Uses structural detection
// (inspired by github.com/nielsgroen/claude-tmux) — checks UI structure
// first (prompt markers, borders), then falls back to content patterns.
//
// Detection hierarchy:
//   1. Check structural markers: ❯ (prompt) + ─ (border above prompt)
//   2. If prompt found + "ctrl+c to interrupt" → RUNNING
//   3. If prompt found + permission/question patterns → NEEDS-INPUT
//   4. If prompt found + no patterns → IDLE (no badge)
//   5. Braille spinner in pane title → RUNNING
//   6. Star (✳) in pane title → IDLE
//
// This is inherently heuristic — patterns may change with Claude Code updates.

// claudeSignals is everything we can infer from a single pass over a Claude
// pane's terminal capture. Lets callers (status detection, intent
// extraction, future token/elapsed parsing) share one bottom-line walk
// instead of each scanning the same buffer independently.
type claudeSignals struct {
	status string // "running" / "needs-input" / ""
	verb   string // current rotating word ("Crafting…"), or ""
	shells int    // count of background shells from "· N shells" footer (0 = none)
}

// claudeSignalsCache memoizes scan results within a single loadTree pass so
// claudeTerminalStatus and extractClaudeWorkingVerb don't each kick off
// their own bottom-line walk on the same capture. Cleared by
// resetClaudeSignalsCache() at the top of loadTree (capture data
// changes per refresh, so the cache must too).
//
// Concurrency: loadTree runs on bubbletea's worker pool. Two ticks can
// overlap (a slow loadTree + the next 1s tick) and both touch this map,
// triggering Go's runtime "concurrent map read and map write" detector
// — fatal, NOT recoverable. Caught two real crashes in production logs
// before this guard. The mutex is read/write because the hot path is
// the cache hit.
var (
	claudeSignalsCache   map[string]claudeSignals
	claudeSignalsCacheMu sync.RWMutex
)

// resetClaudeSignalsCache wipes the per-refresh memo. Called at the start
// of loadTree alongside the capture-pane prefetch.
func resetClaudeSignalsCache() {
	claudeSignalsCacheMu.Lock()
	claudeSignalsCache = nil
	claudeSignalsCacheMu.Unlock()
}

// scanClaudeTerminal does the single bottom-line walk. Detection layers, in
// priority order:
//  1. Verb line ("Word… (... tokens ...)")          → running, capture verb
//  2. ctrl+c / esc to interrupt (legacy form)       → running
//  3. Permission / [y/n] / box-dialog patterns       → needs-input
//  4. None of the above                              → ""
//
// Verb-first because newer Claude versions stopped printing the interrupt
// suffix; the verb line is the only signal left for "actively generating".
func scanClaudeTerminal(paneID string) claudeSignals {
	if paneID == "" {
		return claudeSignals{}
	}
	claudeSignalsCacheMu.RLock()
	cached, ok := claudeSignalsCache[paneID]
	claudeSignalsCacheMu.RUnlock()
	if ok {
		return cached
	}
	sig := scanClaudeTerminalUncached(paneID)
	claudeSignalsCacheMu.Lock()
	if claudeSignalsCache == nil {
		claudeSignalsCache = make(map[string]claudeSignals)
	}
	claudeSignalsCache[paneID] = sig
	claudeSignalsCacheMu.Unlock()
	return sig
}

// scanClaudeTerminalUncached holds the actual scan logic. Split out so
// scanClaudeTerminal can wrap it with the per-refresh memo without
// littering every return statement with a cache write.
func scanClaudeTerminalUncached(paneID string) claudeSignals {
	var out string
	if pc := getCapturedPane(paneID); pc != nil {
		// Use long capture — new Claude layout pushes verb and question
		// lines 8-12 lines above bottom (recap + dual separator + status
		// bar eat the bottom 6-7 lines). The bottom-15 trim below is
		// enough to keep the scan tight.
		out = pc.long
		if out == "" {
			out = pc.short
		}
	} else {
		var err error
		out, err = tmuxQuery("capture-pane", "-pt", paneID, "-S", "-20", "-J")
		if err != nil {
			return claudeSignals{}
		}
	}
	if out == "" {
		return claudeSignals{}
	}

	allLines := strings.Split(out, "\n")
	bottomLines := allLines
	if len(bottomLines) > 15 {
		bottomLines = bottomLines[len(bottomLines)-15:]
	}

	// Single pass over the bottom slice. Every line gets classified into
	// one of: verb-line / interrupt-line / permission-marker / none.
	var (
		verb               string
		shells             int
		hasInterrupt       bool
		hasEscCancel       bool
		hasPermission      bool
		hasYesNo           bool
		hasNumberedOptions bool
		hasBoxDialog bool
	)
	for i := len(bottomLines) - 1; i >= 0; i-- {
		line := bottomLines[i]
		lower := strings.ToLower(line)

		// Verb line — newest match wins (loop runs bottom-up).
		if verb == "" {
			if v := matchVerbLine(line); v != "" {
				verb = v
			}
		}
		// Legacy "actively generating" marker.
		if !hasInterrupt && strings.Contains(lower, "ctrl+c") && strings.Contains(lower, "interrupt") {
			hasInterrupt = true
		}
		if strings.Contains(lower, "esc to cancel") {
			hasEscCancel = true
		}
		// Permission prompt — must be the line's main content, not prose.
		if (strings.Contains(line, "Do you want to proceed?") ||
			strings.Contains(line, "Would you like to proceed?")) && len(strings.TrimSpace(line)) < 60 {
			hasPermission = true
		}
		// [y/n] prompt — short, near end of line.
		if (strings.Contains(line, "[y/n]") || strings.Contains(line, "[Y/n]")) && len(strings.TrimSpace(line)) < 80 {
			hasYesNo = true
		}
		// Permission dialog options ("   1. Yes" / "   2. No") — short,
		// indented. Must NOT match Claude's regular numbered lists.
		trimmed := strings.TrimSpace(line)
		if len(trimmed) > 2 && len(trimmed) < 40 &&
			(trimmed[0] == '1' || trimmed[0] == '2' || trimmed[0] == '3') &&
			trimmed[1] == '.' &&
			len(line) > 0 && (line[0] == ' ' || line[0] == '\t') {
			hasNumberedOptions = true
		}
		if strings.Contains(line, "╭") && strings.Contains(line, "─") {
			hasBoxDialog = true
		}
		// Background shells footer ("· N shells"). Bottommost match wins —
		// the footer is rewritten in place each render, so any earlier
		// match further up the buffer is stale.
		if shells == 0 {
			if m := shellsLineRe.FindStringSubmatch(line); m != nil {
				if n, err := strconv.Atoi(m[1]); err == nil {
					shells = n
				}
			}
		}
	}

	// Decide status. Verb-line and interrupt-line are running signals;
	// permission patterns are needs-input. Running takes precedence —
	// Claude can't simultaneously be generating AND awaiting input.
	// Only structured prompts (numbered options, [y/n], permission
	// dialogs) count as needs-input — not open-ended questions.
	switch {
	case verb != "" || hasInterrupt:
		return claudeSignals{status: "running", verb: verb, shells: shells}
	case hasPermission && (hasEscCancel || hasNumberedOptions):
		debugLog("needs-input %s: permission+esc/numbered", paneID)
		return claudeSignals{status: "needs-input", shells: shells}
	case hasYesNo:
		debugLog("needs-input %s: [y/n] prompt", paneID)
		return claudeSignals{status: "needs-input", shells: shells}
	case hasBoxDialog && hasEscCancel:
		debugLog("needs-input %s: box+esc", paneID)
		return claudeSignals{status: "needs-input", shells: shells}
	case hasNumberedOptions && hasEscCancel:
		debugLog("needs-input %s: numbered+esc", paneID)
		return claudeSignals{status: "needs-input", shells: shells}
	}
	return claudeSignals{shells: shells}
}

// matchVerbLine returns the verb token from a Claude status line of the
// shape "<glyph> Verb… (... tokens ...)", or "" if the line doesn't match.
// Anchor on the "… (" + "token"-in-parens shape — robust against prose.
func matchVerbLine(line string) string {
	paren := strings.Index(line, "… (")
	if paren < 0 {
		return ""
	}
	closeP := strings.IndexByte(line[paren:], ')')
	if closeP < 0 {
		return ""
	}
	if !strings.Contains(strings.ToLower(line[paren:paren+closeP]), "token") {
		return ""
	}
	head := strings.TrimSpace(line[:paren+len("…")])
	if j := strings.LastIndex(head, " "); j >= 0 {
		head = head[j+1:]
	}
	if !strings.HasSuffix(head, "…") || len(head) <= len("…") {
		return ""
	}
	return head
}

// claudeTerminalStatus is the legacy entry point — thin wrapper around
// scanClaudeTerminal for callers that only care about status.
func claudeTerminalStatus(paneID string) string {
	return scanClaudeTerminal(paneID).status
}

// codexTerminalStatus checks terminal capture for codex running state.
func codexTerminalStatus(paneID string) string {
	if paneID == "" {
		return ""
	}
	var out string
	if pc := getCapturedPane(paneID); pc != nil {
		out = pc.long
	} else {
		var err error
		out, err = tmuxQuery("capture-pane", "-pt", paneID)
		if err != nil {
			return ""
		}
	}
	if out == "" {
		return ""
	}
	// codexRunningRe is pre-compiled at package level
	if codexRunningRe.MatchString(out) {
		return "running"
	}
	return ""
}

// hookStatus returns the hook-based status for a pane from the context
// watcher, or "" if unavailable/stale. This is the high-confidence path:
// Claude Code hooks write structured lifecycle events, no regex guessing.
func hookStatus(paneID string) string {
	cw := globalCtxWatcher.Load()
	if cw == nil {
		return ""
	}
	hs := cw.HookStatusForPane(paneID)
	if hs == nil {
		return ""
	}
	return hs.Status
}

// effectivePaneStatus determines the final display status for a pane.
// Priority: hook-based status (structured events) > terminal scan (heuristic).
func effectivePaneStatus(paneID, command, title string, state *PaneState) string {
	app := liveAgentApp(command, title, state)
	if app == "" {
		return ""
	}

	// Hook-based status: highest priority when available. Written by
	// sidebar-status-hook.sh on every Claude lifecycle event.
	// "idle" maps to "" (no active status) so the done-transition and
	// verb-promotion logic in tree.go works unchanged.
	if hs := hookStatus(paneID); hs != "" {
		if hs == "idle" {
			return ""
		}
		return hs
	}

	status := ""
	if state != nil {
		status = strings.TrimSpace(strings.ToLower(state.Status))
	}

	if app == "codex" {
		switch status {
		case "running", "needs-input", "error", "done":
			return status
		}
		if ts := codexTerminalStatus(paneID); ts != "" {
			return ts
		}
		return ""
	}

	// Claude: terminal scan fallback (no hook installed or stale)
	if status == "idle" {
		if ts := claudeTerminalStatus(paneID); ts != "" {
			return ts
		}
		return ""
	}
	if ts := claudeTitleStatus(title); ts != "" {
		return ts
	}
	switch status {
	case "running", "needs-input", "error", "done":
		return status
	}
	if ts := claudeTerminalStatus(paneID); ts != "" {
		return ts
	}
	return ""
}

// extractClaudeIntent strips braille spinners and default title from pane title.
func extractClaudeIntent(title string) string {
	cleaned := strings.TrimSpace(intentPrefixRe.ReplaceAllString(title, ""))
	lower := strings.ToLower(cleaned)
	agentLower := strings.ToLower(Cfg().Agent.Name)
	if lower == agentLower {
		return ""
	}
	if parts := strings.Fields(agentLower); len(parts) > 1 && lower == parts[0] {
		return ""
	}
	return cleaned
}

// extractClaudeWorkingVerb returns Claude's live status verb (the rotating
// "Crafting…" / "Pondering…" / "Combobulating…" word) for a pane. Thin
// wrapper around scanClaudeTerminal; kept as a separate function so tests
// can target verb extraction in isolation. New code should prefer
// scanClaudeTerminal directly to amortize the bottom-line scan.
func extractClaudeWorkingVerb(paneID string) string {
	return scanClaudeTerminal(paneID).verb
}

// extractClaudeShells returns the count of background shells Claude is
// tracking on this pane (parsed from the "· N shells" footer). Returns 0
// when no footer match — either no shells active or pane couldn't be
// captured. Same memo as the verb scan.
func extractClaudeShells(paneID string) int {
	return scanClaudeTerminal(paneID).shells
}
