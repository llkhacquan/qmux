#!/bin/bash
# notify-attention.sh - macOS notification when Claude needs user input.
# Fires an alerter banner on permission_prompt events. Click navigates
# to the originating tmux pane.
#
# Claude Code payload (stdin JSON):
#   { session_id, notification_type, message, title, cwd, ... }
#
# notification_type values:
#   permission_prompt     - tool approval needed  (TRIGGERS notification)
#   idle_prompt           - long idle             (skipped, too noisy)
#   auth_success          - login ok              (skipped)
#   elicitation_dialog    - MCP input             (skipped)
#
# Requirements:
#   - alerter binary (brew install alerter, or https://github.com/vjeantet/alerter)
#   - macOS (uses osascript for frontmost-app check)
#   - Optional: TMUX_SIDEBAR_TERMINAL env var for the terminal app name
#     (default: auto-detects from $TERM_PROGRAM)
#
# Install: copy to ~/.claude/hooks/ and register in settings.json
# Config: settings.json hooks.Notification, async: true, timeout: 3
set -euo pipefail

INPUT=$(cat)
SID=$(echo "$INPUT" | jq -r '.session_id // empty')
NTYPE=$(echo "$INPUT" | jq -r '.notification_type // empty')
MSG=$(echo "$INPUT" | jq -r '.message // empty')
CWD=$(echo "$INPUT" | jq -r '.cwd // empty')

[[ "$NTYPE" != "permission_prompt" ]] && exit 0
[[ -z "$SID" ]] && exit 0

# Detect terminal app
TERMINAL="${TMUX_SIDEBAR_TERMINAL:-${TERM_PROGRAM:-Terminal}}"

# Skip if terminal is already frontmost
FRONT=$(osascript -e 'tell application "System Events" to get name of first process whose frontmost is true' 2>/dev/null | tr '[:upper:]' '[:lower:]' || true)
[[ "$FRONT" == "$(echo "$TERMINAL" | tr '[:upper:]' '[:lower:]')" ]] && exit 0

# Find the tmux pane for this session. Check context files first.
STATE_DIR="${TMUX_SIDEBAR_STATE_DIR:-${XDG_STATE_HOME:-$HOME/.local/state}/tmux-sidebar}"
PANE=""
CTX_FILE="$STATE_DIR/context/ck-context-${SID}.json"
if [[ -f "$CTX_FILE" ]]; then
  PANE=$(jq -r '.paneId // empty' "$CTX_FILE" 2>/dev/null || true)
fi
[[ -z "$PANE" ]] && exit 0

# Subtitle: repo + branch
REPO=$(basename "${CWD:-unknown}")
BRANCH=""
if [[ -n "$CWD" && -d "$CWD" ]]; then
  BRANCH=$(git -C "$CWD" rev-parse --abbrev-ref HEAD 2>/dev/null || true)
fi
SUBTITLE="$REPO"
[[ -n "$BRANCH" ]] && SUBTITLE="$REPO / $BRANCH"

# Body: recent intent (< 60s old) + Claude's message
INTENT_FILE="$STATE_DIR/context/intent-${SID}.json"
BODY="$MSG"
if [[ -f "$INTENT_FILE" ]]; then
  INTENT=$(jq -r '.intent // empty' "$INTENT_FILE" 2>/dev/null || true)
  INTENT_TS=$(jq -r '.ts // 0' "$INTENT_FILE" 2>/dev/null || echo 0)
  NOW=$(date +%s)
  if [[ -n "$INTENT" ]] && (( NOW - INTENT_TS < 60 )); then
    BODY="${INTENT}
${MSG}"
  fi
fi
[[ -z "$BODY" || "$BODY" == $'\n' ]] && BODY="Waiting for your input"

# Check for alerter
if ! command -v alerter >/dev/null 2>&1; then
  exit 0
fi

# Dispatch alerter in background - hook must return immediately.
(
  RESULT=$(alerter \
    --title "Claude needs input" \
    --subtitle "$SUBTITLE" \
    --message "$BODY" \
    --sound default \
    --actions "Go to pane" \
    --close-label "Dismiss" \
    --group "claude-noti-$SID" 2>/dev/null || true)
  if [[ "$RESULT" == "@CONTENTCLICKED" || "$RESULT" == "Go to pane" ]]; then
    tmux switch-client -t "$PANE" 2>/dev/null || true
    osascript -e "tell application \"$TERMINAL\" to activate" 2>/dev/null || true
  fi
) </dev/null >/dev/null 2>&1 &
disown

exit 0
