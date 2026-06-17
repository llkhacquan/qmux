# Claude Code Hooks for sidebar-go

Optional hook scripts that feed richer data to the sidebar. Without them, sidebar-go still works (status via terminal capture, no live intent labels). With them, you get instant status transitions, live tool intent, context %, model name, AI-generated session titles, background agent counts, and macOS notifications.

## Quick install

```bash
# Copy hooks to Claude Code's hook directory
cp hooks/*.sh hooks/*.cjs hooks/*.py ~/.claude/hooks/

# Make executable
chmod +x ~/.claude/hooks/sidebar-status-hook.sh \
         ~/.claude/hooks/intent-tracker.sh \
         ~/.claude/hooks/notify-attention.sh \
         ~/.claude/hooks/intent_title.py
```

Then add to `~/.claude/settings.json`:

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
      { "command": "~/.claude/hooks/intent_title.py", "async": true, "timeout": 2 }
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
      { "command": "~/.claude/hooks/intent_title.py", "async": true, "timeout": 2 }
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

## Hook reference

### Core hooks (recommended)

| Script | Event(s) | What it provides |
|--------|----------|-----------------|
| `sidebar-status-hook.sh` | All lifecycle events | Instant status transitions (running/idle/needs-input) without terminal capture polling |
| `intent-tracker.sh` | PreToolUse | Live "editing foo.go" / "searching bar" labels on cards |
| `statusline-wrapper.cjs` | statusLine.command | Context %, model name, effort level, session name |

### Enhancement hooks (optional)

| Script | Event(s) | What it provides | Extra deps |
|--------|----------|-----------------|------------|
| `sidebar-bg-track.cjs` | SubagentStart/Stop | Background agent count per card | node |
| `intent_title.py` | UserPromptSubmit, SessionStart | AI-generated 3-6 word session titles | python3, GROQ_API_KEY |
| `notify-attention.sh` | Notification | macOS banner when Claude needs input | alerter (macOS only) |

### Dependencies

All hooks require `jq` for JSON parsing.

| Hook | Runtime | Optional deps |
|------|---------|--------------|
| `sidebar-status-hook.sh` | bash, jq | - |
| `intent-tracker.sh` | bash, jq | - |
| `statusline-wrapper.cjs` | node | - |
| `sidebar-bg-track.cjs` | node | - |
| `intent_title.py` | python3 | GROQ_API_KEY (env var or `.env` file next to script) |
| `notify-attention.sh` | bash, jq | alerter, macOS |

## How it works

All hooks write JSON files to `~/.local/state/tmux-sidebar/context/`. sidebar-go watches this directory via fsnotify and updates cards within milliseconds.

```
Claude Code hook event
       |
       v
hook script writes JSON to ~/.local/state/tmux-sidebar/context/
       |
       v
sidebar-go fsnotify picks up change -> card updates
```

### File types

| Pattern | Writer | Contents |
|---------|--------|----------|
| `hook-status-<paneID>.json` | sidebar-status-hook.sh | `{event, status, tool, ts, session_id, pane_id}` |
| `intent-<sessionID>.json` | intent-tracker.sh | `{intent, ts, tool, file_path, subagent_type}` |
| `ck-context-<sessionID>.json` | statusline-wrapper.cjs | `{paneId, modelName, percent, effort, cwd, ...}` |
| `bg-<paneID>.json` | sidebar-bg-track.cjs | `{agents: [{id, type, started}]}` |
| `title-<sessionID>.json` | intent_title.py | `{stage, title, ts, model, latency_ms, ...}` |

## Degradation without hooks

sidebar-go works with any subset of hooks installed (including zero):

| Missing hook | Impact |
|-------------|--------|
| sidebar-status-hook.sh | Status detection falls back to terminal capture regex (slower, less reliable) |
| intent-tracker.sh | No live tool intent labels on cards |
| statusline-wrapper.cjs | No context %, model name, or effort level |
| intent_title.py | Card titles fall back to Claude's pane title |
| sidebar-bg-track.cjs | No background agent count |
| notify-attention.sh | No macOS notifications |

## Environment variables

All hooks respect these for state directory resolution:

| Variable | Default | Purpose |
|----------|---------|---------|
| `TMUX_SIDEBAR_STATE_DIR` | - | Explicit override for state directory |
| `XDG_STATE_HOME` | `~/.local/state` | XDG base; state dir becomes `$XDG_STATE_HOME/tmux-sidebar` |

`intent_title.py` also reads:
- `GROQ_API_KEY` - API key for Groq LLM title generation
- `INTENT_TITLE_MODEL` - model override (default: `llama-3.1-8b-instant`)

`notify-attention.sh` also reads:
- `TMUX_SIDEBAR_TERMINAL` - terminal app name for activation (default: auto-detect from `$TERM_PROGRAM`)
