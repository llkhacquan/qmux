# Tmux Hooks and Integration Dependencies

sidebar-go is not a standalone binary. It integrates deeply with tmux (hooks, control mode, options) and Claude Code (lifecycle hooks that write state files). This document catalogues every integration point.

## Overview

```
tmux hooks (tmux.conf)
       |
       v
sidebar-go {init, ensure, on-focus, on-exit, display, ...}
       |
       v
tmux -C control conn <-- async notifications (%window-pane-changed, ...)
       |
       v
~/.local/state/tmux-sidebar/
  |- shared-state        (daemon <-> clients)
  |- daemon.sock         (client <-> daemon RPC)
  |- focus.sock          (shell hooks -> daemon, via socat)
  '- context/
      |- ck-context-*.json     (statusline-wrapper.cjs)
      |- intent-*.json         (intent-tracker.sh)
      |- hook-status-*.json    (sidebar-status-hook.sh)
      |- title-*.json          (intent-title.py)
      '- bg-*.json             (sidebar-bg-track.cjs)
```

---

## 1. Tmux Hooks

These are set in `tmux.conf` via `set-hook -g`. Each fires a shell command when the corresponding tmux event occurs.

| Hook | Index | Command | Purpose |
|------|-------|---------|---------|
| `client-active` | [198] | `sidebar-go ensure` | Lazy-create sidebar when terminal gains focus |
| `client-attached` | [199] | `sidebar-go ensure` | Lazy-create sidebar when client attaches |
| `client-session-changed` | [200] | `sidebar-go on-focus <pane> <window>` | Track session switches |
| `after-select-window` | [203] | `sidebar-go on-focus <pane> <window>` | Track window switches |
| `after-select-pane` | [204] | `sidebar-focus-send <pane> <window>` | Focus tracking via socat (no Go fork) |
| `after-new-window` | [207] | `sidebar-go ensure <pane> <window>` | Create sidebar in new windows |
| `after-kill-pane` | [208] | `sidebar-go on-exit <pane> <window>` | Cleanup when panes die |
| `pane-exited` | (bare) | `sidebar-go on-exit <pane> <window>` | Fallback cleanup (pane process exits) |

**Index numbers** are arbitrary unique IDs that prevent hook collisions with other plugins.

**Why `after-select-pane` uses a shell script instead of sidebar-go directly:**
Every exec of the Go binary costs ~230ms for endpoint security on-access scanning (antivirus, EDR). `after-select-pane` fires on every pane move (the most frequent event), so the socat bridge (`sidebar-focus-send`) ships a datagram to `focus.sock` without forking the binary.

### Required tmux.conf setup

```tmux
# Initialize sidebar-go (registers subcommands, daemon watchdog)
run-shell -b "$HOME/.local/bin/sidebar-go init"

# Focus tracking
set-hook -g "client-session-changed[200]" "run-shell -b '$HOME/.local/bin/sidebar-go on-focus #{pane_id} #{window_id}'"
set-hook -g "after-select-window[203]"    "run-shell -b '$HOME/.local/bin/sidebar-go on-focus #{pane_id} #{window_id}'"
set-hook -g "after-select-pane[204]"      "run-shell -b '$HOME/.local/bin/sidebar-focus-send #{pane_id} #{window_id}'"

# Lifecycle
set-hook -g "after-new-window[207]"  "run-shell -b '$HOME/.local/bin/sidebar-go ensure #{hook_pane} #{hook_window}'"
set-hook -g "after-kill-pane[208]"   "run-shell -b '$HOME/.local/bin/sidebar-go on-exit #{hook_pane} #{hook_window}'"
set-hook -g pane-exited              "run-shell -b '$HOME/.local/bin/sidebar-go on-exit #{hook_pane} #{hook_window}'"

# Optional: auto-ensure on attach
set-hook -g "client-active[198]"   "run-shell -b '$HOME/.local/bin/sidebar-go ensure'"
set-hook -g "client-attached[199]" "run-shell -b '$HOME/.local/bin/sidebar-go ensure'"
```

---

## 2. Tmux Key Bindings

All optional. sidebar-go works without any keybindings (hooks drive it).

| Key | Command | Purpose |
|-----|---------|---------|
| `prefix + t` | `sidebar-go toggle` | Toggle sidebar on/off |
| `prefix + T` | `sidebar-go focus` | Focus the sidebar pane |
| `prefix + Tab` / `prefix + F12` | `sidebar-switch-last` | Toggle to last-active Claude pane |
| `prefix + Up/Down` | `sidebar-focus-nav up/down` | Navigate sidebar cursor |
| `1`-`9` (in TUI) | positional jump | Jump to Nth Claude pane (when sidebar focused) |

### Helper scripts (fork-free wrappers)

These scripts avoid forking the Go binary on the hot path:

| Script | Purpose | How it works |
|--------|---------|-------------|
| `sidebar-toggle` | Open/close sidebar | Shell-native tmux split + option writes on open; delegates to `sidebar-go close` on close |
| `sidebar-focus-nav` | Move cursor up/down | Finds sidebar pane by title, `send-keys` arrow to the running TUI |
| `sidebar-focus-send` | Ship focus event to daemon | `socat` datagram to `focus.sock`; falls back to `sidebar-go on-focus` |
| `sidebar-switch-last` | Toggle last-active pane | `send-keys` backtick to the running TUI |

---

## 3. Tmux Options

sidebar-go reads and writes global tmux options (`@tmux_sidebar_*`) for state persistence.

### Core options (required)

| Option | R/W | Purpose |
|--------|-----|---------|
| `@tmux_sidebar_enabled` | R/W | Master enable flag (0/1) |
| `@tmux_sidebar_main_pane` | R/W | User's last focused non-sidebar pane |
| `@tmux_sidebar_pane_w<windowID>` | R/W | Sidebar pane ID per window |
| `@tmux_sidebar_creating_w<windowID>` | R/W | Lock during sidebar creation |
| `@tmux_sidebar_ensure_w<windowID>` | R/W | `wait-for` mutex for atomic creation |

### Optional options

| Option | Purpose | Default |
|--------|---------|---------|
| `@tmux_sidebar_debug` | Enable debug logging | 0 |
| `@tmux_sidebar_scrolloff` | Scroll margin (rows) | 8 |
| `@tmux_sidebar_session_order` | Session sort order (CSV) | (none) |
| `@tmux_sidebar_hidden_sessions` | Hidden session names (CSV) | (none) |
| `@tmux_sidebar_focus_on_open` | Auto-focus sidebar on open | 0 |
| `@tmux_sidebar_layout_w<wID>` | Saved window layout for restore-on-close | (none) |
| `@tmux_sidebar_panes_w<wID>` | Saved pane IDs for restore | (none) |
| `@tmux_sidebar_badge_running` | Custom badge override | config.toml value |
| `@tmux_sidebar_badge_needs_input` | Custom badge override | config.toml value |
| `@tmux_sidebar_badge_done` | Custom badge override | config.toml value |
| `@tmux_sidebar_badge_error` | Custom badge override | config.toml value |

---

## 4. Tmux Control Mode

The daemon maintains a persistent `tmux -C attach-session` child for efficient query multiplexing and async event streaming.

**Connection:** `tmux -C attach-session -f no-output -S <socket>` where socket is derived from `$TMUX` env var.

**Subscribed notifications:**

| Event | Purpose |
|-------|---------|
| `%window-pane-changed` | Active pane moved within window |
| `%session-window-changed` | Session's current window changed |
| `%client-session-changed` | Client switched session |
| `%session-changed` | Session state changed |
| `%window-add` / `%window-close` | Window lifecycle |
| `%unlinked-window-add` / `%unlinked-window-close` | Unlinked window lifecycle |
| `%window-renamed` | Window renamed |

**Fallback:** If control mode is unavailable, queries fall back to `tmux` fork/exec.

---

## 5. Claude Code Hook Scripts

These scripts run as Claude Code hooks (configured in `~/.claude/settings.json`). They write JSON files to `~/.local/state/tmux-sidebar/context/` which sidebar-go watches via fsnotify.

### sidebar-status-hook.sh

**Hook events:** SessionStart, UserPromptSubmit, PreToolUse, PostToolUse, Stop, Notification, SubagentStart, SubagentStop

**Output:** `hook-status-<paneID>.json`

```json
{"event":"PreToolUse","status":"running","tool":"Edit","ts":1718649600,"session_id":"abc","pane_id":"%5"}
```

**Status mapping:**
| Event | Status |
|-------|--------|
| UserPromptSubmit, PreToolUse, PostToolUse, SubagentStart | running |
| Stop | idle |
| Notification | needs-input |
| SessionStart | idle |
| SubagentStop | (ignored - prevents overwriting parent's idle) |

### intent-tracker.sh

**Hook event:** PreToolUse (async, timeout: 5s)

**Output:** `intent-<sessionID>.json`

```json
{"intent":"editing user_config.go","ts":1718649600,"tool":"Edit","file_path":"/path/to/file","subagent_type":""}
```

**Verb mapping:**
| Tool | Verb |
|------|------|
| Edit/MultiEdit | editing |
| Write | creating |
| Read | reading |
| Bash | running |
| Grep | searching |
| Glob | finding |
| Agent | delegating |
| WebFetch | fetching |
| WebSearch | searching web |

Sidebar displays this as the live "what Claude is doing" label on each card. Decays after `intent_stale` duration (default 30s).

### statusline-wrapper.cjs

**Trigger:** `statusLine.command` in settings.json (runs on every statusline render, ~1s)

**Output:** `ck-context-<sessionID>.json`

```json
{
  "paneId": "%5",
  "sessionId": "abc-123",
  "modelName": "Claude Opus 4",
  "percent": 42,
  "size": 200000,
  "timestamp": 1718649600000,
  "effort": "high",
  "sessionName": "fix-parser-bug",
  "worktree": null,
  "cwd": "/path/to/project"
}
```

Sidebar uses `percent` for context % display, `modelName` for the model badge, and `paneId` for pane-to-session mapping.

**Cleanup:** On ~5% of renders, sweeps dead-pane context files via `tmux list-panes`.

### intent-title.py

**Hook event:** UserPromptSubmit, SessionStart (async, timeout: 2s)

**Output:** `title-<sessionID>.json`

Two-stage pipeline:
1. **Foreground (<50ms):** Writes placeholder with user's prompt text as title
2. **Detached worker:** Calls Groq LLM to generate a 3-6 word session title, atomic-rewrites the file

```json
{
  "stage": "complete",
  "title": "Fix sidebar footer bug",
  "ts": 1718649600,
  "model": "llama-3.1-8b-instant",
  "latency_ms": 180
}
```

Sidebar reads `title` field as the card's session name (overrides pane title).

**Requires:** `GROQ_API_KEY` in `.env` file. Falls back to prompt text if Groq unavailable.

### sidebar-bg-track.cjs

**Hook events:** SubagentStart (mode=start), SubagentStop (mode=stop)

**Output:** `bg-<paneID>.json`

```json
{"agents":[{"id":"agent-123","type":"Explore","started":1718649600000}]}
```

Sidebar shows count of running background agents per card.

### notify-attention.sh

**Hook event:** Notification (permission_prompt only)

**Output:** macOS notification via `alerter` binary. Click navigates to the Claude pane.

Not strictly a sidebar dependency - it's a standalone notification feature that reads intent files from the same context directory.

---

## 6. Claude Code settings.json Configuration

The hooks above must be registered in `~/.claude/settings.json`:

```json
{
  "hooks": {
    "PreToolUse": [
      { "command": "~/.claude/hooks/intent-tracker.sh", "async": true, "timeout": 5 },
      { "command": "~/.claude/hooks/sidebar-status-hook.sh PreToolUse", "async": true, "timeout": 3 }
    ],
    "PostToolUse": [
      { "command": "~/.claude/hooks/sidebar-status-hook.sh PostToolUse", "async": true, "timeout": 3 }
    ],
    "UserPromptSubmit": [
      { "command": "~/.claude/hooks/sidebar-status-hook.sh UserPromptSubmit", "async": true, "timeout": 3 },
      { "command": "~/.claude/hooks/intent-title.py", "async": true, "timeout": 2 }
    ],
    "Stop": [
      { "command": "~/.claude/hooks/sidebar-status-hook.sh Stop", "async": true, "timeout": 3 }
    ],
    "Notification": [
      { "command": "~/.claude/hooks/sidebar-status-hook.sh Notification", "async": true, "timeout": 3 },
      { "command": "~/.claude/hooks/notify-attention.sh", "async": true, "timeout": 3 }
    ],
    "SessionStart": [
      { "command": "~/.claude/hooks/sidebar-status-hook.sh SessionStart", "async": true, "timeout": 3 },
      { "command": "~/.claude/hooks/intent-title.py", "async": true, "timeout": 2 }
    ],
    "SubagentStart": [
      { "command": "~/.claude/hooks/sidebar-bg-track.cjs start", "async": true, "timeout": 3 },
      { "command": "~/.claude/hooks/sidebar-status-hook.sh SubagentStart", "async": true, "timeout": 3 }
    ],
    "SubagentStop": [
      { "command": "~/.claude/hooks/sidebar-bg-track.cjs stop", "async": true, "timeout": 3 },
      { "command": "~/.claude/hooks/sidebar-status-hook.sh SubagentStop", "async": true, "timeout": 3 }
    ]
  },
  "statusLine": {
    "command": "node ~/.claude/hooks/statusline-wrapper.cjs"
  }
}
```

---

## 7. UDS Sockets

| Socket | Path | Type | Purpose |
|--------|------|------|---------|
| `daemon.sock` | `~/.local/state/tmux-sidebar/daemon.sock` | STREAM | Client-daemon RPC and snapshot broadcast |
| `focus.sock` | `~/.local/state/tmux-sidebar/focus.sock` | DGRAM | Shell hooks send focus events (socat) |
| `<pid>.sock` | `~/.local/state/tmux-sidebar/sock/<pid>.sock` | DGRAM | Peer doorbell for cross-process wake |

---

## 8. Shared State Files

| File | Location | Format | Writer | Reader |
|------|----------|--------|--------|--------|
| `shared-state` | state dir | JSON | daemon, display clients | all clients |
| `shared-state.lock` | state dir | flock | sidebarstate.WithLock | sidebarstate.WithLock |
| `daemon.lock` | state dir | flock | daemon | daemon (election) |
| `width` | state dir | plain int | saveSidebarWidth | configuredSidebarWidth |
| `debug.log` | state dir | text | debugLog | operator |

---

## 9. Environment Variables

| Variable | Required | Purpose |
|----------|----------|---------|
| `$TMUX` | Yes | Tmux server socket path (format: `socket,pid,session`) |
| `$TMUX_PANE` | Yes | Current pane ID |
| `$HOME` | Yes | Home directory for state and config paths |
| `$XDG_STATE_HOME` | No | Override state directory (default: `~/.local/state`) |
| `$XDG_CONFIG_HOME` | No | Override config directory (default: `~/.config`) |
| `$TMUX_SIDEBAR_STATE_DIR` | No | Explicit state directory override |
| `$QMUX_CONFIG` | No | Explicit config file path override |
| `$SIDEBAR_DEBUG` | No | Enable debug logging (set to 1) |

---

## 10. External Dependencies

| Binary | Required | Purpose |
|--------|----------|---------|
| `tmux` (>= 3.3) | Yes | Everything. Control mode, hooks, pane management |
| `git` | No | Git metadata on cards (branch, ahead/behind) |
| `socat` | No | Fork-free focus tracking via `focus.sock`. Falls back to Go binary |
| `jq` | Yes (for hooks) | JSON parsing in hook shell scripts |
| `node` | Yes (for hooks) | Runs statusline-wrapper.cjs and sidebar-bg-track.cjs |
| `python3` | No | intent-title.py (Groq LLM titles). Falls back to prompt text |
| `alerter` | No | macOS notifications. Only for notify-attention.sh |

---

## Degradation Without Hooks

sidebar-go works without Claude Code hooks, but with reduced functionality:

| Missing hook | Impact |
|-------------|--------|
| sidebar-status-hook.sh | Status detection falls back to terminal capture regex (slower, less reliable) |
| intent-tracker.sh | No live "editing foo.go" labels on cards |
| statusline-wrapper.cjs | No context %, model name, or effort level display |
| intent-title.py | Card titles fall back to Claude's pane title (generic) |
| sidebar-bg-track.cjs | No background agent count |
| notify-attention.sh | No macOS notifications |

The sidebar's core tree display (sessions, windows, panes, git info, basic status via terminal scan) works with zero hooks configured.
