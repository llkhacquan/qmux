#!/bin/bash
# PreToolUse hook (async) - writes 1-line intent to sidebar-go context dir.
# sidebar-go fsnotify-watches this file to show live "what Claude is doing" on cards.
#
# Output: ~/.local/state/tmux-sidebar/context/intent-<session_id>.json
# Format: {"intent":"verb target","ts":epoch,"tool":"ToolName"}
#
# Install: copy to ~/.claude/hooks/ and register in settings.json
# Config: settings.json hooks.PreToolUse, async: true, timeout: 5
set -euo pipefail

INPUT=$(cat)
TOOL=$(echo "$INPUT" | jq -r '.tool_name')
SID=$(echo "$INPUT" | jq -r '.session_id')
[[ -z "$SID" || "$SID" == "null" ]] && exit 0

case "$TOOL" in
  Edit|MultiEdit) VERB="editing" ;;
  Write)          VERB="creating" ;;
  Read)           VERB="reading" ;;
  Bash)           VERB="running" ;;
  Grep)           VERB="searching" ;;
  Glob)           VERB="finding" ;;
  Agent)          VERB="delegating" ;;
  WebFetch)       VERB="fetching" ;;
  WebSearch)      VERB="searching web" ;;
  *)              VERB="$TOOL" ;;
esac

FILE_PATH=""
SUBAGENT_TYPE=""
case "$TOOL" in
  Edit|MultiEdit|Write|Read)
    FILE_PATH=$(echo "$INPUT" | jq -r '.tool_input.file_path // ""')
    TARGET=$(echo "$FILE_PATH" | xargs basename 2>/dev/null || echo "") ;;
  Bash)
    TARGET=$(echo "$INPUT" | jq -r '.tool_input.command // ""' | head -c 50) ;;
  Grep)
    TARGET=$(echo "$INPUT" | jq -r '"\"\(.tool_input.pattern // "")\"" ') ;;
  Glob)
    TARGET=$(echo "$INPUT" | jq -r '.tool_input.pattern // ""') ;;
  Agent)
    SUBAGENT_TYPE=$(echo "$INPUT" | jq -r '.tool_input.subagent_type // ""')
    TARGET=$(echo "$INPUT" | jq -r '.tool_input.description // ""' | head -c 40) ;;
  WebFetch|WebSearch)
    TARGET=$(echo "$INPUT" | jq -r '.tool_input.url // .tool_input.query // ""' | head -c 40) ;;
  *)
    TARGET="" ;;
esac

STATE_DIR="${TMUX_SIDEBAR_STATE_DIR:-${XDG_STATE_HOME:-$HOME/.local/state}/tmux-sidebar}"
DIR="$STATE_DIR/context"
mkdir -p "$DIR"

OUTFILE="$DIR/intent-${SID}.json"
jq -nc --arg intent "$VERB $TARGET" --arg tool "$TOOL" --argjson ts "$(date +%s)" \
  --arg file_path "$FILE_PATH" --arg subagent_type "$SUBAGENT_TYPE" \
  '{intent: $intent, ts: $ts, tool: $tool, file_path: $file_path, subagent_type: $subagent_type}' > "${OUTFILE}.tmp"
mv "${OUTFILE}.tmp" "$OUTFILE"
exit 0
