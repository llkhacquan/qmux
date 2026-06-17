package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/fsnotify/fsnotify"
	"github.com/llkhacquan/qmux/sidebar-go/internal/ckcontext"
)

// globalCtxWatcher is set by startContextWatcherCmd (a tea.Cmd worker) and
// read by tree.go from concurrent loadTree workers. atomic.Pointer makes
// the publication race-free per Go memory model — without it, a 64-bit
// pointer write on word-aligned x86/arm64 is atomic in practice but the
// memory ordering relative to the watcher's internal map state is
// undefined.
var globalCtxWatcher atomic.Pointer[ContextWatcher]

// ClaudeContext aliases the shared context type (also used by swarm-go).
type ClaudeContext = ckcontext.Context

func cfgIntentStaleDuration() time.Duration    { return Cfg().Timing.IntentStale.Duration }
func cfgHookStatusRunningStale() time.Duration  { return Cfg().Timing.HookRunningStale.Duration }
func cfgHookStatusNeedsInputStale() time.Duration { return Cfg().Timing.HookNeedsInputStale.Duration }

// ContextWatcher uses fsnotify to watch the sidebar context directory
// for enriched Claude session files written by statusline-wrapper.cjs
// (ck-context-*.json), intent-tracker.sh (intent-*.json), and
// sidebar-status-hook.sh (hook-status-*.json).
type ContextWatcher struct {
	mu               sync.RWMutex
	byPane           map[string]*ClaudeContext       // paneID → latest context
	bySession        map[string]*ckcontext.Intent    // sessionID → latest intent (PreToolUse)
	titleBySession   map[string]*ckcontext.Title     // sessionID → latest AI-summarized title (UserPromptSubmit)
	hookStatusByPane map[string]*ckcontext.HookStatus // paneID → latest hook-based status
	watcher          *fsnotify.Watcher
	dir              string
	notify           chan<- struct{} // signal main loop on updates
	stop             chan struct{}
}

// NewContextWatcher creates a watcher on the sidebar context directory.
// Sends on notify (non-blocking) when context data changes.
func NewContextWatcher(notify chan<- struct{}) (*ContextWatcher, error) {
	dir := filepath.Join(stateDir(), "context")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}

	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	if err := w.Add(dir); err != nil {
		w.Close()
		return nil, err
	}

	cw := &ContextWatcher{
		byPane:           make(map[string]*ClaudeContext),
		bySession:        make(map[string]*ckcontext.Intent),
		titleBySession:   make(map[string]*ckcontext.Title),
		hookStatusByPane: make(map[string]*ckcontext.HookStatus),
		watcher:          w,
		dir:              dir,
		notify:           notify,
		stop:             make(chan struct{}),
	}

	// Load existing files before starting the watch loop
	cw.loadAll()

	safeGo("context.watcher", cw.loop)
	return cw, nil
}

// Stop shuts down the watcher goroutine and releases resources.
func (cw *ContextWatcher) Stop() {
	close(cw.stop)
	cw.watcher.Close()
}

// signal sends a non-blocking wake-up to the main loop.
func (cw *ContextWatcher) signal() {
	select {
	case cw.notify <- struct{}{}:
	default:
	}
}

// ForPane returns the context for a pane, or nil if not found.
// Staleness isn't checked — file presence is the liveness signal. Dead panes
// are purged by the opportunistic cleanup in statusline-wrapper.cjs (which
// reconciles against `tmux list-panes`). Idle sessions keep their last-known
// % visible until the pane actually closes.
func (cw *ContextWatcher) ForPane(paneID string) *ClaudeContext {
	cw.mu.RLock()
	defer cw.mu.RUnlock()
	return cw.byPane[paneID]
}

// IntentForSession returns the most recent tool intent for a sessionID, or
// nil if missing / stale. Intents are ephemeral (decays in ~30s) — callers
// should treat nil as "Claude isn't actively doing anything right now".
func (cw *ContextWatcher) IntentForSession(sessionID string) *ckcontext.Intent {
	if sessionID == "" {
		return nil
	}
	cw.mu.RLock()
	defer cw.mu.RUnlock()
	in := cw.bySession[sessionID]
	if in == nil {
		return nil
	}
	if time.Since(time.Unix(in.TS, 0)) > cfgIntentStaleDuration() {
		return nil
	}
	return in
}

// HookStatusForPane returns the hook-based status for a pane, or nil if
// missing or stale. "running" decays after hookStatusStaleDuration (the hook
// should keep refreshing while Claude is active). "idle" and "needs-input"
// persist until the next event overwrites them.
func (cw *ContextWatcher) HookStatusForPane(paneID string) *ckcontext.HookStatus {
	if paneID == "" {
		return nil
	}
	cw.mu.RLock()
	defer cw.mu.RUnlock()
	hs := cw.hookStatusByPane[paneID]
	if hs == nil {
		return nil
	}
	age := time.Since(time.Unix(hs.TS, 0))
	switch hs.Status {
	case "running":
		if age > cfgHookStatusRunningStale() {
			return nil
		}
	case "needs-input":
		if age > cfgHookStatusNeedsInputStale() {
			return nil
		}
	}
	return hs
}

// TitleForSession returns the most recent AI-summarized title for a sessionID,
// or empty string if missing/pending. Stage="pending" rows are skipped
// (they hold only the truncated-prompt fallback) so the renderer falls back
// to pane.Title until the Groq round-trip completes.
// No TTL — title is a historical fact. Orphan files cleaned by pane-liveness reaper.
func (cw *ContextWatcher) TitleForSession(sessionID string) string {
	if sessionID == "" {
		return ""
	}
	cw.mu.RLock()
	defer cw.mu.RUnlock()
	t := cw.titleBySession[sessionID]
	if t == nil || t.Title == "" {
		return ""
	}
	if t.Stage == "pending" {
		return ""
	}
	return t.Title
}

// loadAll scans the context directory for existing files on startup.
func (cw *ContextWatcher) loadAll() {
	entries, err := os.ReadDir(cw.dir)
	if err != nil {
		return
	}
	cw.mu.Lock()
	defer cw.mu.Unlock()
	for _, e := range entries {
		name := e.Name()
		switch {
		case ckcontext.IsContextFile(name):
			if ctx := ckcontext.ReadFile(filepath.Join(cw.dir, name)); ctx != nil {
				cw.byPane[ctx.PaneID] = ctx
			}
		case ckcontext.IsIntentFile(name):
			sid := ckcontext.IntentSessionID(name)
			if sid == "" {
				continue
			}
			if in := ckcontext.ReadIntentFile(filepath.Join(cw.dir, name)); in != nil {
				cw.bySession[sid] = in
			}
		case ckcontext.IsTitleFile(name):
			sid := ckcontext.TitleSessionID(name)
			if sid == "" {
				continue
			}
			if t := ckcontext.ReadTitleFile(filepath.Join(cw.dir, name)); t != nil {
				cw.titleBySession[sid] = t
			}
		case ckcontext.IsHookStatusFile(name):
			if hs := ckcontext.ReadHookStatusFile(filepath.Join(cw.dir, name)); hs != nil {
				cw.hookStatusByPane[hs.PaneID] = hs
			}
		}
	}
}

// loop processes fsnotify events until stopped.
func (cw *ContextWatcher) loop() {
	for {
		select {
		case <-cw.stop:
			return
		case event, ok := <-cw.watcher.Events:
			if !ok {
				return
			}
			name := filepath.Base(event.Name)
			switch {
			case ckcontext.IsContextFile(name):
				if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) {
					if ctx := ckcontext.ReadFile(event.Name); ctx != nil {
						cw.mu.Lock()
						cw.byPane[ctx.PaneID] = ctx
						cw.mu.Unlock()
						cw.signal()
					}
				}
				if event.Has(fsnotify.Remove) {
					cw.removeByFile(name)
				}
			case ckcontext.IsIntentFile(name):
				if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) {
					sid := ckcontext.IntentSessionID(name)
					if sid == "" {
						continue
					}
					if in := ckcontext.ReadIntentFile(event.Name); in != nil {
						cw.mu.Lock()
						cw.bySession[sid] = in
						cw.mu.Unlock()
						cw.signal()
					}
				}
				if event.Has(fsnotify.Remove) {
					sid := ckcontext.IntentSessionID(name)
					if sid != "" {
						cw.mu.Lock()
						delete(cw.bySession, sid)
						cw.mu.Unlock()
						cw.signal()
					}
				}
			case ckcontext.IsTitleFile(name):
				if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) {
					sid := ckcontext.TitleSessionID(name)
					if sid == "" {
						continue
					}
					if t := ckcontext.ReadTitleFile(event.Name); t != nil {
						cw.mu.Lock()
						cw.titleBySession[sid] = t
						cw.mu.Unlock()
						cw.signal()
					}
				}
				if event.Has(fsnotify.Remove) {
					sid := ckcontext.TitleSessionID(name)
					if sid != "" {
						cw.mu.Lock()
						delete(cw.titleBySession, sid)
						cw.mu.Unlock()
						cw.signal()
					}
				}
			case ckcontext.IsHookStatusFile(name):
				if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) {
					if hs := ckcontext.ReadHookStatusFile(event.Name); hs != nil {
						cw.mu.Lock()
						cw.hookStatusByPane[hs.PaneID] = hs
						cw.mu.Unlock()
						cw.signal()
					}
				}
				if event.Has(fsnotify.Remove) {
					pid := ckcontext.HookStatusPaneID(name)
					if pid != "" {
						cw.mu.Lock()
						delete(cw.hookStatusByPane, pid)
						cw.mu.Unlock()
						cw.signal()
					}
				}
			}
		case _, ok := <-cw.watcher.Errors:
			if !ok {
				return
			}
		}
	}
}

// removeByFile removes context entries associated with a deleted file.
func (cw *ContextWatcher) removeByFile(filename string) {
	// Extract sessionID from filename to find matching entry
	sessionID := strings.TrimPrefix(strings.TrimSuffix(filename, ".json"), "ck-context-")
	cw.mu.Lock()
	defer cw.mu.Unlock()
	for pane, ctx := range cw.byPane {
		if ctx.SessionID == sessionID {
			delete(cw.byPane, pane)
			select {
			case cw.notify <- struct{}{}:
			default:
			}
			return
		}
	}
}

// formatIntentLine builds the display string for the ephemeral tool intent.
// Example: "⚡ editing handler.ts (2s)"
// maxWidth is total display cells the result may consume; body is truncated
// in display cells (not bytes) to fit after subtracting the prefix and suffix.
// Embedded newlines/tabs/CRs in the intent body would otherwise break out of
// the card walls (terminal would soft-wrap), so they're collapsed to spaces.
func formatIntentLine(in *ckcontext.Intent, maxWidth int) string {
	body := sanitizeIntentBody(toolActivityLabel(in))
	age := time.Since(time.Unix(in.TS, 0))
	suffix := fmt.Sprintf(" (%ds)", int(age.Seconds()))
	prefix := "⚡ "
	overhead := lipgloss.Width(prefix) + lipgloss.Width(suffix)
	if maxWidth > overhead {
		bodyMax := maxWidth - overhead
		if lipgloss.Width(body) > bodyMax {
			body = truncateCells(body, bodyMax-1) + "…"
		}
	}
	return prefix + body + suffix
}

// sanitizeIntentBody collapses control runes (\n, \r, \t) to spaces so a
// multiline tool intent doesn't wrap past the card walls.
func sanitizeIntentBody(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		return r
	}, s)
}

// truncateCells returns s truncated to at most maxCells display cells,
// counted via lipgloss.Width so wide runes (emoji, CJK) consume the right
// number of cells. Stops at a rune boundary — never splits a multi-byte rune.
func truncateCells(s string, maxCells int) string {
	if maxCells <= 0 {
		return ""
	}
	var b strings.Builder
	b.Grow(len(s))
	width := 0
	for _, r := range s {
		w := lipgloss.Width(string(r))
		if width+w > maxCells {
			break
		}
		b.WriteRune(r)
		width += w
	}
	return b.String()
}

// reapOrphanContextFiles removes context/intent/title/hook-status JSON files
// whose owning tmux pane no longer exists. This keeps kqueue's per-file FD
// count bounded (macOS kqueue opens every file in a watched directory).
//
// Liveness check: query tmux list-panes once, build a set, delete files
// whose paneID (or sessionID) has no live pane. Much more accurate than
// the old ModTime heuristic which killed files from idle-but-alive sessions.
func reapOrphanContextFiles() int {
	dir := filepath.Join(stateDir(), "context")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}

	// Build live pane set from tmux
	livePanes := make(map[string]bool)
	if out, err := runTmux("list-panes", "-a", "-F", "#{pane_id}"); err == nil {
		for line := range strings.SplitSeq(out, "\n") {
			if line != "" {
				livePanes[line] = true
			}
		}
	}
	if len(livePanes) == 0 {
		return 0 // tmux not running or no panes - don't reap anything
	}

	// First pass: collect live sessionIDs from ck-context files that have live panes.
	// Intent/title files are keyed by sessionID, so we need this mapping.
	liveSessions := make(map[string]bool)
	for _, e := range entries {
		name := e.Name()
		if !ckcontext.IsContextFile(name) {
			continue
		}
		if ctx := ckcontext.ReadFile(filepath.Join(dir, name)); ctx != nil {
			if livePanes[ctx.PaneID] {
				liveSessions[ctx.SessionID] = true
			}
		}
	}

	// Second pass: reap orphan files
	reaped := 0
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		orphan := false
		switch {
		case ckcontext.IsContextFile(name):
			if ctx := ckcontext.ReadFile(filepath.Join(dir, name)); ctx != nil {
				orphan = !livePanes[ctx.PaneID]
			}
		case ckcontext.IsHookStatusFile(name):
			paneID := ckcontext.HookStatusPaneID(name)
			orphan = paneID != "" && !livePanes[paneID]
		case ckcontext.IsIntentFile(name):
			sid := ckcontext.IntentSessionID(name)
			orphan = sid != "" && !liveSessions[sid]
		case ckcontext.IsTitleFile(name):
			sid := ckcontext.TitleSessionID(name)
			orphan = sid != "" && !liveSessions[sid]
		default:
			continue
		}
		if orphan {
			os.Remove(filepath.Join(dir, name))
			reaped++
		}
	}
	return reaped
}

// effortGlyph maps effort level strings to compact glyphs.
var effortGlyph = map[string]string{
	"low":    "○",
	"medium": "◐",
	"high":   "●",
	"xhigh":  "◉",
	"max":    "◉",
}

// contextDot returns a colored dot based on token usage.
func contextDot(percent, size int) string {
	used := size * percent / 100
	switch {
	case used > 200_000:
		return "🔴"
	case used > 150_000:
		return "🟡"
	default:
		return "🟢"
	}
}

// formatContextLine builds the display string for a card's context row.
// Example: "🟢 150k/1M · Opus ●"
func formatContextLine(cc *ClaudeContext) string {
	fmtK := func(tokens int) string {
		if tokens >= 1000000 {
			return fmt.Sprintf("%.0fM", float64(tokens)/1e6)
		}
		return fmt.Sprintf("%dk", tokens/1000)
	}
	used := cc.Size * cc.Percent / 100
	line := fmt.Sprintf("%s %s/%s", contextDot(cc.Percent, cc.Size), fmtK(used), fmtK(cc.Size))
	if cc.ModelName != "" {
		name := cc.ModelName
		if idx := strings.Index(name, " ("); idx > 0 {
			name = name[:idx]
		}
		line += " · " + name
	}
	if g, ok := effortGlyph[cc.Effort]; ok {
		line += " " + g
	}
	return line
}

