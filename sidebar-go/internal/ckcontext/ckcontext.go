// Package ckcontext reads Claude Code session context files written by
// statusline-wrapper.cjs. Each file maps one tmux pane to one Claude session.
package ckcontext

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// Worktree holds worktree metadata from Claude Code's statusline JSON.
type Worktree struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	Branch    string `json:"branch"`
	OriginalDir string `json:"original_dir"`
}

// Context is the enriched session data written by statusline-wrapper.cjs.
type Context struct {
	PaneID      string    `json:"paneId"`
	SessionID   string    `json:"sessionId"`
	ModelName   string    `json:"modelName"`
	Percent     int       `json:"percent"`
	Size        int       `json:"size"`
	Timestamp   int64     `json:"timestamp"`
	Effort      string    `json:"effort,omitempty"`
	SessionName string    `json:"sessionName,omitempty"`
	Worktree    *Worktree `json:"worktree,omitempty"`
	Cwd         string    `json:"cwd,omitempty"`
}

// Intent is the per-tool-call action written by intent-tracker.sh
// (PreToolUse hook). One file per session, rewritten on every tool call.
// Timestamp is epoch seconds (shell `date +%s`), NOT ms like Context.
type Intent struct {
	Intent       string `json:"intent"`
	TS           int64  `json:"ts"`
	Tool         string `json:"tool"`
	FilePath     string `json:"file_path,omitempty"`
	SubagentType string `json:"subagent_type,omitempty"`
}

// Title is the AI-summarized session intent written by intent-title.py
// (UserPromptSubmit hook). One file per Claude session, rewritten on every
// user prompt. Used as the card title fallback when Claude Code's own
// pane_title goes stale (claude only emits it once per session).
//
// The hook's full debug payload has more fields (request, response, latency,
// etc.); the renderer only cares about Title and TS.
type Title struct {
	Title string `json:"title"`
	TS    int64  `json:"ts"` // epoch seconds when the record was written
	Stage string `json:"stage"`
}

// HookStatus is the per-pane status written by sidebar-status-hook.sh on
// every Claude Code lifecycle event. Keyed by pane_id (from $TMUX_PANE).
// Replaces terminal-scan heuristics when fresh.
type HookStatus struct {
	Event     string `json:"event"`
	Status    string `json:"status"`
	Tool      string `json:"tool,omitempty"`
	TS        int64  `json:"ts"`
	SessionID string `json:"session_id"`
	PaneID    string `json:"pane_id"`
}

// Dir returns the default ck-context directory:
// $HOME/.local/state/tmux-sidebar/context
func Dir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "tmux-sidebar", "context")
}

// IsContextFile reports whether name matches ck-context-*.json.
func IsContextFile(name string) bool {
	return strings.HasPrefix(name, "ck-context-") && strings.HasSuffix(name, ".json")
}

// IsIntentFile reports whether name matches intent-*.json.
func IsIntentFile(name string) bool {
	return strings.HasPrefix(name, "intent-") && strings.HasSuffix(name, ".json")
}

// IntentSessionID extracts the sessionID from an intent file name.
// Returns empty string if name is not an intent file.
func IntentSessionID(name string) string {
	if !IsIntentFile(name) {
		return ""
	}
	return strings.TrimPrefix(strings.TrimSuffix(name, ".json"), "intent-")
}

// IsTitleFile reports whether name matches title-*.json.
func IsTitleFile(name string) bool {
	return strings.HasPrefix(name, "title-") && strings.HasSuffix(name, ".json")
}

// TitleSessionID extracts the sessionID from a title file name.
// Returns empty string if name is not a title file.
func TitleSessionID(name string) string {
	if !IsTitleFile(name) {
		return ""
	}
	return strings.TrimPrefix(strings.TrimSuffix(name, ".json"), "title-")
}

// ReadTitleFile reads and parses a single title file. Returns nil on any
// error or if Title is empty. The hook writes a much larger debug payload
// but only Title/TS/Stage are decoded — the rest is dead weight here.
func ReadTitleFile(path string) *Title {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var t Title
	if err := json.Unmarshal(data, &t); err != nil {
		return nil
	}
	if t.Title == "" {
		return nil
	}
	return &t
}

// ReadIntentFile reads and parses a single intent file. Returns nil on any
// error or if Intent is empty.
func ReadIntentFile(path string) *Intent {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var i Intent
	if err := json.Unmarshal(data, &i); err != nil {
		return nil
	}
	if i.Intent == "" {
		return nil
	}
	return &i
}

// IsHookStatusFile reports whether name matches hook-status-*.json.
func IsHookStatusFile(name string) bool {
	return strings.HasPrefix(name, "hook-status-") && strings.HasSuffix(name, ".json")
}

// HookStatusPaneID extracts the pane ID from a hook-status file name.
func HookStatusPaneID(name string) string {
	if !IsHookStatusFile(name) {
		return ""
	}
	return strings.TrimPrefix(strings.TrimSuffix(name, ".json"), "hook-status-")
}

// ReadHookStatusFile reads and parses a hook-status file. Returns nil on error
// or if PaneID is empty.
func ReadHookStatusFile(path string) *HookStatus {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var h HookStatus
	if err := json.Unmarshal(data, &h); err != nil {
		return nil
	}
	if h.PaneID == "" {
		return nil
	}
	return &h
}

// ReadFile reads and parses a single ck-context file. Returns nil on any error
// or if PaneID is missing.
func ReadFile(path string) *Context {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var c Context
	if err := json.Unmarshal(data, &c); err != nil {
		return nil
	}
	if c.PaneID == "" {
		return nil
	}
	return &c
}

// ReadAll scans dir and returns all valid contexts keyed by PaneID.
// Later files for the same PaneID overwrite earlier ones.
func ReadAll(dir string) map[string]*Context {
	out := make(map[string]*Context)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if !IsContextFile(e.Name()) {
			continue
		}
		if c := ReadFile(filepath.Join(dir, e.Name())); c != nil {
			out[c.PaneID] = c
		}
	}
	return out
}

// FindByPaneID returns the context for paneID from dir, or nil.
func FindByPaneID(dir, paneID string) *Context {
	return ReadAll(dir)[paneID]
}

// FindBySessionID returns the context for sessionID from dir, or nil.
func FindBySessionID(dir, sessionID string) *Context {
	for _, c := range ReadAll(dir) {
		if c.SessionID == sessionID {
			return c
		}
	}
	return nil
}
