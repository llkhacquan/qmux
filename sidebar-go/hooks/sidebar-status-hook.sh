#!/bin/bash
# Hook-based status detection for sidebar-go.
# Writes hook-status-{pane_id}.json on every Claude Code lifecycle event.
# sidebar-go fsnotify-watches this file for instant status transitions
# instead of polling terminal output via capture-pane regex.
#
# Usage: sidebar-status-hook.sh <event>
# Events: SessionStart, UserPromptSubmit, PreToolUse, PostToolUse,
#         Stop, Notification, SubagentStart, SubagentStop
#
# Install: copy to ~/.claude/hooks/ and register in settings.json
# Config: settings.json hooks, async: true, timeout: 3
set -euo pipefail

EVENT="${1:-}"
[ -z "$EVENT" ] && exit 0

PANE="${TMUX_PANE:-}"
[ -z "$PANE" ] && exit 0

INPUT=$(cat)
SID=$(echo "$INPUT" | jq -r '.session_id // empty' 2>/dev/null)
[ -z "$SID" ] && exit 0

case "$EVENT" in
  SubagentStop)
    # Late SubagentStop after parent's Stop would overwrite idle with running.
    exit 0 ;;
  SubagentStart|UserPromptSubmit|PreToolUse|PostToolUse)
    STATUS="running" ;;
  Stop)
    STATUS="idle" ;;
  Notification)
    STATUS="needs-input" ;;
  SessionStart)
    STATUS="idle" ;;
  *)
    STATUS="running" ;;
esac

TOOL=$(echo "$INPUT" | jq -r '.tool_name // empty' 2>/dev/null)

STATE_DIR="${TMUX_SIDEBAR_STATE_DIR:-${XDG_STATE_HOME:-$HOME/.local/state}/tmux-sidebar}"
DIR="$STATE_DIR/context"
mkdir -p "$DIR"

OUTFILE="$DIR/hook-status-${PANE}.json"
jq -nc \
  --arg event "$EVENT" \
  --arg status "$STATUS" \
  --arg tool "${TOOL:-}" \
  --argjson ts "$(date +%s)" \
  --arg session_id "$SID" \
  --arg pane_id "$PANE" \
  '{event:$event,status:$status,tool:$tool,ts:$ts,session_id:$session_id,pane_id:$pane_id}' \
  > "${OUTFILE}.tmp"
mv "${OUTFILE}.tmp" "$OUTFILE"
