#!/usr/bin/env bash
# sidebar.tmux — TPM plugin entry point for sidebar-go
#
# Install via TPM:
#   set -g @plugin 'llkhacquan/tmux-claude-sidebar'
#
# Or manually: copy this file's contents into your .tmux.conf
# after building the binary: go install github.com/llkhacquan/tmux-claude-sidebar@latest
#
# Requirements:
#   - Go 1.21+ (for building from source)
#   - tmux 3.2+ (for hook slot numbering)
#   - Claude Code or Codex running in tmux panes

set -euo pipefail

CURRENT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BINARY_NAME="sidebar-go"
NOTIFY_BINARY_NAME="sidebar-notify"

# ---------------------------------------------------------------------------
# Resolve a binary path: user override > $PATH > plugin dir > build from source.
# Same resolution policy for both the fat sidebar binary and the lean focus helper.
# ---------------------------------------------------------------------------
resolve_binary() {
  local opt="$1" name="$2"
  local user_bin
  user_bin=$(tmux show-option -gqv "$opt" 2>/dev/null || true)
  if [[ -n "$user_bin" && -x "$user_bin" ]]; then
    echo "$user_bin"
    return
  fi

  if command -v "$name" &>/dev/null; then
    command -v "$name"
    return
  fi

  local built="$CURRENT_DIR/$name"
  if [[ -x "$built" ]]; then
    echo "$built"
    return
  fi

  if command -v go &>/dev/null; then
    (cd "$CURRENT_DIR" && go build -o "$name" .) 2>/dev/null
    if [[ -x "$built" ]]; then
      echo "$built"
      return
    fi
  fi

  echo ""
}

BINARY=$(resolve_binary @tmux_sidebar_binary "$BINARY_NAME")
if [[ -z "$BINARY" ]]; then
  tmux display-message "sidebar-go: binary not found. Install Go and rebuild, or set @tmux_sidebar_binary."
  exit 1
fi

# sidebar-notify is the lean on-focus helper. Falls back to `$BINARY on-focus`
# when the dedicated binary isn't installed yet — old setups keep working.
NOTIFY_BINARY=$(resolve_binary @tmux_sidebar_notify_binary "$NOTIFY_BINARY_NAME")

# ---------------------------------------------------------------------------
# Read user options (with defaults)
# ---------------------------------------------------------------------------
get_opt() {
  local val
  val=$(tmux show-option -gqv "$1" 2>/dev/null || true)
  echo "${val:-$2}"
}

KEY_TOGGLE=$(get_opt @tmux_sidebar_key_toggle "T")
KEY_FOCUS=$(get_opt @tmux_sidebar_key_focus "t")
KEY_SWITCH_LAST=$(get_opt @tmux_sidebar_key_switch_last "F12")
KEY_GOTO_PREFIX=$(get_opt @tmux_sidebar_key_goto "'")
FOCUS_ON_OPEN=$(get_opt @tmux_sidebar_focus_on_open "0")

# ---------------------------------------------------------------------------
# Initialize
# ---------------------------------------------------------------------------
tmux set -g @tmux_sidebar_focus_on_open "$FOCUS_ON_OPEN"
tmux run-shell -b "$BINARY init"

# ---------------------------------------------------------------------------
# Key bindings
# ---------------------------------------------------------------------------
tmux bind-key "$KEY_TOGGLE"      run-shell -b "$BINARY toggle"
tmux bind-key "$KEY_FOCUS"       run-shell -b "$BINARY focus"
tmux bind-key "$KEY_SWITCH_LAST" run-shell -b "$BINARY switch-last"
tmux bind-key Up                 run-shell -b "$BINARY focus-up"
tmux bind-key Down               run-shell -b "$BINARY focus-down"

# Quick jump: prefix + goto-key → wait for 1-9
tmux bind-key "$KEY_GOTO_PREFIX" switch-client -T sidebar-goto
for i in $(seq 1 9); do
  tmux bind-key -T sidebar-goto "$i" run-shell -b "$BINARY goto $i"
done

# ---------------------------------------------------------------------------
# Lifecycle hooks (numbered slots to avoid collisions with other plugins)
# ---------------------------------------------------------------------------

# Sidebar creation
tmux set-hook -g "client-active[198]"          "run-shell -b '$BINARY ensure'"
tmux set-hook -g "client-attached[199]"        "run-shell -b '$BINARY ensure'"

# Focus tracking — updates active pane, clears needs-input badges.
# The lean sidebar-notify binary boots ~5–10× faster than the fat sidebar-go
# (no bubbletea/lipgloss/fsnotify in its link graph), which matters because
# tmux fires *every* focus hook below per pane switch — the helper boot cost
# is the dominant component of perceived focus latency. Falls back to
# `$BINARY on-focus` when sidebar-notify isn't installed yet.
if [[ -n "$NOTIFY_BINARY" ]]; then
  # #{q:pane_title} shell-escapes the title so apostrophes/quotes/backslashes
  # don't break the outer single-quoted run-shell arg. tmux 2.6+. pane_id
  # and window_id are tmux-controlled (% / @ + digits), no escaping needed.
  FOCUS_CMD="$NOTIFY_BINARY #{pane_id} #{window_id} #{q:pane_title}"
else
  FOCUS_CMD="$BINARY on-focus #{pane_id} #{window_id}"
fi
# Minimal focus-hook set: pane move, in-session window move, session switch.
# client-focus-in (terminal refocus, not a pane change) and session-window-changed
# (overlaps after-select-window) dropped — every fork of the heavy binary pays an
# EDR on-access scan, and the daemon's 1s tick + control-conn notifications cover
# residual drift. after-select-window keeps the `; ensure` tail so a never-visited
# window still gets its lazy sidebar on first visit.
# after-select-pane (the most frequent focus event) goes through the fork-free
# sidebar-focus-send helper: socat the "pane|window" to the daemon's focus.sock
# instead of forking the heavy binary. window/session switches keep the forked
# focus path because they also need the fat `ensure` (lazy sidebar create), which
# the detached daemon can't safely do.
FOCUS_SEND="$(dirname "$BINARY")/sidebar-focus-send"
tmux set-hook -g "client-session-changed[200]" "run-shell -b '$FOCUS_CMD ; $BINARY ensure #{pane_id} #{window_id}'"
tmux set-hook -g "after-select-window[203]"    "run-shell -b '$FOCUS_CMD ; $BINARY ensure #{pane_id} #{window_id}'"
tmux set-hook -g "after-select-pane[204]"      "run-shell -b '$FOCUS_SEND #{pane_id} #{window_id}'"
# No client-resized hook: tmux reflows the sidebar off its fixed width on a
# terminal resize, but the hook fires before the reflow settles (reads a stale
# width, skips the resize). The daemon re-pins every sidebar on its reload loop
# (syncAllSidebarWidths), which runs after the layout settles and forks nothing.

# Structural signals. No `notify` hooks: cmdNotify is a no-op — the daemon learns
# split/resize/rename over its tmux -C control conn (%layout-change / %window-renamed).
tmux set-hook -g "after-new-window[207]"       "run-shell -b '$BINARY ensure #{hook_pane} #{hook_window}'"
tmux set-hook -g "after-kill-pane[208]"        "run-shell -b '$BINARY on-exit #{hook_pane} #{hook_window}'"
# pane-exited covers natural process exits (claude exit, shell exit) —
# after-kill-pane only fires on explicit kill-pane commands. tmux 3.6 silently
# rejects the numbered-slot form for pane-exited, so use the bare form.
tmux set-hook -g pane-exited                   "run-shell -b '$BINARY on-exit #{hook_pane} #{hook_window}'"

# ---------------------------------------------------------------------------
# Optional: catppuccin status bar integration
# Uncomment if using catppuccin/tmux theme — shows repo name + Claude spinner
# ---------------------------------------------------------------------------
# tmux set -g @catppuccin_window_default_text "#($BINARY window-name '#{pane_current_path}' '#{window_id}')"
# tmux set -g @catppuccin_window_current_text "#($BINARY window-name '#{pane_current_path}' '#{window_id}')"
# tmux set -g @catppuccin_window_text "#($BINARY window-name '#{pane_current_path}' '#{window_id}')"
