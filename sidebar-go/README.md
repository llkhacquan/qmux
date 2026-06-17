# sidebar-go

Single Go binary replacing tmux-sidebar Python+Bash plugin. Displays all Claude Code panes across tmux sessions as flat card layout with realtime updates, status detection, visual notifications.

## Setup

Three steps for a new machine:

### 1. Build and install

```bash
cd sidebar-go
make install   # builds, ad-hoc signs, installs to ~/.local/bin/sidebar-go
```

### 2. tmux config

Add the keybindings and hooks to `~/.tmux.conf` - see the [tmux config](#tmux-config) section below. Alternatively, source `sidebar.tmux` as a TPM plugin if you prefer.

### 3. Claude Code hooks (optional)

Hook scripts in `hooks/` feed richer data (instant status, live tool intent, context %, model name, AI titles, macOS notifications). Without them, sidebar-go still works via terminal capture.

See **[hooks/README.md](hooks/README.md)** for install instructions and full reference.

## macOS endpoint security (antivirus / EDR)

If endpoint security software (antivirus, EDR) is installed, it may intercept
every `exec` of this binary. Two failure modes to watch for:

- **Stripped binary → SIGKILL.** A stripped static Go binary can trip malware
  heuristics and get killed on every exec (`exit 137`); the whole sidebar fleet
  dies. **Never strip these binaries** (no `strip`, no `-ldflags="-s -w"` in
  the Makefile). The unstripped, symbol-rich binary is the safe one - security
  software scans it instead of killing it.
- **Unstripped binary → ~230-460ms per exec.** On-access scanning runs on every
  fork. Since tmux forks the binary per keypress (`focus-up/down`) and per
  focus hook (`on-focus`), nav feels sluggish.

Mitigations:

1. **Whitelist by path** (`~/.local/bin/sidebar-go`) in your security software.
   Use a *path / trusted-process exec exclusion*, not a malware-detection
   exception - the latter stops the kill but NOT the per-exec scan latency.
   Path-based survives rebuilds (the ad-hoc-signed binary's hash changes every
   `make install`).
2. **Avoid forking on the hot path.** `prefix+Up/Down` is bound to the
   `sidebar-focus-nav` shell script, and leader+leader (F12) / `prefix+Tab` to
   `sidebar-switch-last` - both pure tmux `send-keys` to the live TUI (no Go
   fork) instead of `sidebar-go focus-up/down` / `switch-last`. The
   `after-select-pane` focus hook (fires on every pane move) goes through
   `sidebar-focus-send`, which `socat`s the focused `pane|window` to the
   daemon's `focus.sock` (socat is a stable system binary security software
   caches, unlike the per-build-rehashed sidebar-go) instead of forking
   `on-focus`. See tmux config below.

Diagnose with the exit code, not wall time (a killed process also looks "fast"):
`~/.local/bin/sidebar-go window-name; echo $?` - `0` = allowed, `137` = SIGKILL.

## tmux config

Add to `~/.tmux.conf` (replaces tmux-sidebar plugin entirely):

```tmux
# sidebar-go
set -g @tmux_sidebar_focus_on_open 0
run-shell -b "$HOME/.local/bin/sidebar-go init"
bind-key t run-shell -b "$HOME/.local/bin/sidebar-go toggle"
bind-key T run-shell -b "$HOME/.local/bin/sidebar-go focus"
# Cursor nav + last-pane toggle: shell scripts (pure tmux, no Go fork) — see endpoint security section above.
bind-key Up   run-shell -b "$HOME/.local/bin/sidebar-focus-nav up"
bind-key Down run-shell -b "$HOME/.local/bin/sidebar-focus-nav down"
bind-key Tab  run-shell -b "$HOME/.local/bin/sidebar-switch-last"
bind-key F12  run-shell -b "$HOME/.local/bin/sidebar-switch-last"
set-hook -g "client-active[198]" "run-shell -b '$HOME/.local/bin/sidebar-go ensure'"
set-hook -g "client-attached[199]" "run-shell -b '$HOME/.local/bin/sidebar-go ensure'"
# Minimal focus set (pane / window / session move). client-focus-in and
# session-window-changed dropped: each hook forks the heavy binary (costly under
# endpoint security scanning), and the daemon's 1s tick + control-conn
# notifications cover the residual drift those two added.
# after-select-pane (most frequent focus event) → sidebar-focus-send: socat the
# focused pane|window to the daemon's focus.sock, NO sidebar-go fork. Falls back
# to forked on-focus when no daemon. Window/session switches keep forked on-focus
# (they also need cmdEnsure, which the detached daemon can't safely run).
set-hook -g "client-session-changed[200]" "run-shell -b '$HOME/.local/bin/sidebar-go on-focus #{pane_id} #{window_id}'"
set-hook -g "after-select-window[203]" "run-shell -b '$HOME/.local/bin/sidebar-go on-focus #{pane_id} #{window_id}'"
set-hook -g "after-select-pane[204]" "run-shell -b '$HOME/.local/bin/sidebar-focus-send #{pane_id} #{window_id}'"
# No `notify` hooks: cmdNotify is a no-op. The daemon learns split/resize/rename
# over its tmux -C control conn (%layout-change / %window-renamed), so forking
# the binary for after-split-window / after-resize-pane / after-rename-window
# was pure overhead under endpoint security scanning.
set-hook -g "after-new-window[207]" "run-shell -b '$HOME/.local/bin/sidebar-go ensure #{hook_pane} #{hook_window}'"
set-hook -g "after-kill-pane[208]" "run-shell -b '$HOME/.local/bin/sidebar-go on-exit #{hook_pane} #{hook_window}'"
set-hook -g "pane-exited" "run-shell -b '$HOME/.local/bin/sidebar-go on-exit #{hook_pane} #{hook_window}'"
```

## Features

Big features have detail docs under `docs/`. Click through for the why, how, and pitfalls.

### Display

- **Card-based dashboard** — one card per Claude / Codex pane. Lines: intent (from title), git (repo + branch + diff counts), context % + model, location (session:window), live tool intent (`⚡ running …`).
- **4-state styling (Catppuccin Mocha)** — running (green stripe), unread (yellow), needs-input (peach, blink), idle (dim). Cards highlight active pane (blue border) separately from cursor pane (`▌` half-block).
- **Live Claude verb** — when a pane is mid-response, the top border label cycles its real status word (`crafting ✦`, `pondering ✻`, `combobulating ✶`, …) read straight from the pane's status line, in a rainbow shimmer that mirrors Claude's own ✦ glyph cycle. → [docs/status-detection.md](docs/status-detection.md)
- **Snowflake spinner** — rotating ✦ ✧ ✶ ✷ ✸ ✹ ✺ ✻ glyph at 150ms, color-cycled with the rainbow palette.
- **Software blink** for needs-input cards (peach pulse every 700ms; works on terminals that ignore ANSI blink).
- **Pane-by-number jump** — `1`-`9` to jump to the Nth Claude pane in sidebar order.
- **Circular navigation** — `j`/`k`/`↑`/`↓` wrap top↔bottom. `gg`/`G` for top/bottom, `Ctrl+U`/`Ctrl+D` for half-page.
- **Search** — `/` jumps cursor to first match; `n`/`N` walks matches.
- **Right-click workspace menu** — right-click toggles a show/hide menu per tmux session; click a row to hide/unhide its cards.

### Detection & data

- **Terminal-based status detection** — Claude has no API; status (running / needs-input / idle / done) is inferred from `tmux capture-pane`: bottom-line patterns + pane title heuristics + per-pane state file. → [docs/status-detection.md](docs/status-detection.md)
- **Single-scan signals** — one bottom-15 walk per pane per refresh produces both status and live verb (memoized inside `loadTree`).
- **Git info with parallel caching** — repo + branch detected per pane working dir, fetched in parallel (up to 8 workers), cached to disk with 10s TTL, shared across instances.
- **Unread tracking** — when a pane transitions running → idle/done while you weren't looking, card turns yellow until you visit it.

### Daemon + thin clients

- **One engine, many screens** — a single `sidebar-go serve` daemon owns the only `loadTree` pipeline, tmux control conn, git watcher, and usage scanner. Each window runs a thin `sidebar-go display` client that just renders daemon-pushed snapshots and forwards user actions as intents over a UDS stream. Replaces the old leader-election model (every window ran a full engine, peers synced through a shared-state file). → [docs/architecture.md](docs/architecture.md)
- **Spinner-first boot** — the client TUI starts before the daemon connects, so cold start shows an animated "waiting for daemon…" spinner (tight 25ms reconnect poll) instead of a blank pane until the first snapshot lands.
- **Standalone fallback** — if no daemon delivers a snapshot within 3s, a deadline quits the client and it runs the full in-process engine so the pane is never blank.
- **Background refresh, non-blocking** — heavy tmux I/O runs in tea.Cmd workers; input latency stays <1ms.
- **Hook-driven updates** — tmux focus / split / kill hooks signal changes instantly; 1s tick is just a fallback for content changes hooks can't see.
- **Shared-state file** — survives as the bridge to lean hook helpers (`on-focus`, `notify`) that poke state without linking the engine. → [docs/shared-state-sync.md](docs/shared-state-sync.md)

### Claude Code hooks

Optional hook scripts in [`hooks/`](hooks/) feed richer data to the sidebar. Without them, sidebar-go still works (status via terminal capture). With them, you get instant status, live tool intent labels, context %, model name, AI session titles, and macOS notifications. See **[hooks/README.md](hooks/README.md)** for install instructions and the full reference.

### Runtime & dev loop

- **Bubbletea runtime** - Elm-style `Model + Update + View` on bubbletea + bubbles + lipgloss. Per-segment border styling, click latency ~80ms (4 tmux ops pipelined into one fork), cross-instance fsnotify sync. -> [docs/bubbletea-runtime.md](docs/bubbletea-runtime.md)
- **Auto-reload on binary change** - `make install` blinks the sidebar once and reappears running the new code. fsnotify on the binary's parent directory + event filter + `tea.Quit` then `syscall.Exec` handoff. Loops on exec failure so the pane survives a partial install. -> [docs/auto-reload.md](docs/auto-reload.md)
- **Lifecycle management** - toggle / ensure / close / focus / cleanup all handled by Go subcommands invoked from tmux hooks. Signal handling in-process.

## Usage

```bash
sidebar-go                  # Standalone interactive TUI (full in-process engine)
sidebar-go serve            # Daemon: single engine, pushes snapshots to clients
sidebar-go display          # Thin client: renders daemon state in its tmux pane
sidebar-go --dump-render    # Non-interactive text output (for fzf/preview)
sidebar-go --version        # Print version

# Subcommands (called by tmux hooks)
sidebar-go toggle           # Show/hide sidebar
sidebar-go ensure [pane] [window]   # Create sidebar if missing
sidebar-go close [pane] [window]    # Close sidebar, restore layout
sidebar-go on-focus <pane> <window> # Track active pane
sidebar-go on-exit <pane> <window>  # Cleanup killed sidebar; also auto-kills orphan sidebar panes whose last non-sidebar sibling just died
sidebar-go notify           # Signal refresh to all sidebars
sidebar-go focus            # Toggle focus between sidebar and main
sidebar-go init             # Set up pane border format
sidebar-go window-name <path> <window_id>  # Status bar window name with spinner

# Navigation (for tmux keybindings)
sidebar-go goto <1-9>       # Jump to Nth Claude pane
sidebar-go switch-last      # Switch to last Claude pane (fallback only)
sidebar-go switch-next      # Next Claude pane
sidebar-go switch-prev      # Previous Claude pane
sidebar-go focus-up         # Focus sidebar + navigate up   (fallback only)
sidebar-go focus-down       # Focus sidebar + navigate down (fallback only)
```

> `focus-up/down` and `switch-last` are now used only as the no-sidebar
> fallback inside the `sidebar-focus-nav` / `sidebar-switch-last` scripts.
> Normal nav + last-pane toggle go through tmux `send-keys` to the live TUI to
> avoid the per-exec security scan tax (see the endpoint security section). The Go `switch-last` also
> self-heals stale toggle state: a killed last/current pane never survives in
> `active`/`last_active` (dead target → stay put).

## Keybindings

| Key | Action |
|-----|--------|
| `j`/`k`, `↑`/`↓` | Navigate panes (circular wrap) |
| `gg` | Jump to first pane |
| `G` | Jump to last pane |
| `Ctrl+U` / `Ctrl+D` | Half-page up/down |
| `1`-`9` | Jump to Nth pane |
| Enter | Switch to cursor pane |
| `/` | Search mode |
| `n`/`N` | Next/prev search match |
| `q`, `Ctrl+C` | Close sidebar |
| `Esc`, `Ctrl+L` | Unfocus sidebar (return to main pane) |
| Mouse click | Select and switch pane |
| Right-click | Open workspace show/hide menu (click a row to toggle; esc / right-click to close) |
| Scroll wheel | Scroll sidebar |

Hidden workspaces are a view-only filter — the tmux session, its Claude panes, the
daemon refresh, and all hooks keep running. State persists in the
`@tmux_sidebar_hidden_sessions` tmux option and rides the daemon snapshot so every
sidebar instance converges.

## Configuration

### Config file

`~/.config/qmux/config.toml` (or `$QMUX_CONFIG`, or `$XDG_CONFIG_HOME/qmux/config.toml`).

Changes are picked up live - the daemon watches the file via fsnotify and rebuilds the tree on save. No restart needed. Everything has sensible defaults; only add sections you want to customize.

See **[example.toml](example.toml)** for the full annotated reference with every option explained. Quick start:

```bash
cp example.toml ~/.config/qmux/config.toml
# edit to taste - sidebar updates live on save
```

Sections: `[[workspace]]` (folder shortcuts in card titles), `[icons.sessions]` (per-session icons), `[agent]` (AI agent detection), `[[pricing]]` (token cost), `[theme]` (colors), `[badges]` (status emoji), `[timing]` (refresh rates), `[sidebar]` (display defaults), `[card]` (toggle card lines).

### Tmux options

Tmux options (set via `tmux set -g`):

| Option | Default | Description |
|--------|---------|-------------|
| `@tmux_sidebar_scrolloff` | config | Scroll margin (overrides TOML config) |
| `@tmux_sidebar_session_order` | - | Comma-separated session sort order |
| `@tmux_sidebar_focus_on_open` | 0 | Auto-focus sidebar when opened |
| `@tmux_sidebar_badge_running` | config | Custom running badge (overrides TOML config) |
| `@tmux_sidebar_badge_needs_input` | config | Custom needs-input badge |
| `@tmux_sidebar_badge_done` | config | Custom done badge |
| `@tmux_sidebar_badge_error` | config | Custom error badge |
| `@tmux_sidebar_debug` | 0 | Enable debug logging |

## Debugging

```bash
# Subcommand logs
tail -f ~/.local/state/tmux-sidebar/sidebar-go.log

# Performance tracing (start sidebar with env var)
SIDEBAR_PERF=1 sidebar-go
cat ~/.local/state/tmux-sidebar/perf.log | grep key-nav
```

## Architecture

One binary, three runtime roles: a `serve` **daemon** owns the only engine; thin
`display` **clients** render daemon-pushed snapshots over a UDS stream; a bare
`sidebar-go` is the **standalone** full-engine fallback. Bubbletea runtime files
(`tea_*.go`) own interactive logic on both the client and standalone paths. See
**[docs/architecture.md](docs/architecture.md)** for the full picture.

| File | Purpose |
|------|---------|
| `main.go` | Entry + subcommand dispatch, pprof signal, logging setup |
| `daemon.go` | `serve`: flock election, engine loop, broadcast hub, accept loop, upgrade re-exec |
| `display.go` | `display`: dial/handshake/reconnect, lazy-start, standalone fallback |
| `protocol.go` | Wire envelope, message types, `StateSnapshot`, `protoVersion` |
| `tmux_control.go` | Persistent `tmux -C` control connection (the daemon's single conn) |
| `tea_model.go` | bubbletea Model, Update, key/mouse handlers, `runBubble` exec gate, `clientMode` |
| `tea_view.go` | View(), viewport plumbing, search bar |
| `tea_render.go` | composeStyledLines, border row composition, status overlays |
| `tea_messages.go` | All Msg types |
| `tea_commands.go` | All Cmd factories (loadTreeCmd, tickCmd, watchers, …) |
| `tea_keymap.go` | bubbles/key bindings |
| `tea_styles.go` | lipgloss styles + color definitions |
| `tea_legacy.go` | tcell-free helpers extracted during the migration |
| `render_cell.go` | Cell + Grid primitives, rasterize, serialize (ANSI coalescing) |
| `render_decorators.go` | State-to-style mapping, border labels, status overlays |
| `render_march.go` | Marching-comet animation (rainbow + lavender) |
| `render_state.go` | Precomputed render state maps shared across decorators |
| `commands.go` | Hook subcommands (toggle, ensure, close, on-focus, on-exit, init, goto, window-name) |
| `helpers.go` | Sidebar pane management + shared-state file IO |
| `hidden.go` | Per-workspace hide/show (view filter, tmux option persistence) |
| `question.go` | Question detection heuristics for "asked" border labels |
| `activity.go` | Tool intent label enrichment (file-type verbs, subagent labels) |
| `tree.go` | Data model, card layout, session grouping, word wrap, search |
| `status.go` | Claude/Codex detection (single-scan signals + verb extraction) |
| `context.go` | fsnotify watcher for ck-context + intent files |
| `git.go` / `git_watch.go` | Git detection (disk cache + parallel) + fsnotify index watcher |
| `gh.go` | GitHub PR info fetcher |
| `usage.go` | Claude usage / token cost footer (scans JSONL transcripts) |
| `config.go` | Constants, tmux option helpers, sidebar width persistence |
| `user_config.go` | TOML config file loader, workspace matching, live reload watcher |
| `tmux.go` | tmux command helpers |
| `logging.go` | Size-capped log rotation + fd-2 reopener |

Detail docs:

- [docs/architecture.md](docs/architecture.md) — daemon + thin-client topology, lifecycle, IPC, fallback, upgrade
- [docs/bubbletea-runtime.md](docs/bubbletea-runtime.md) — runtime + file-by-file responsibilities, perf wins from the rewrite
- [docs/status-detection.md](docs/status-detection.md) — terminal-scan heuristics + live verb extraction
- [docs/shared-state-sync.md](docs/shared-state-sync.md) — shared-state file shape + hook bridge
- [docs/ipc-uds-notify.md](docs/ipc-uds-notify.md) — datagram doorbell design
- [docs/auto-reload.md](docs/auto-reload.md) — fsnotify-driven `make install` hot-swap
- [docs/render-pipeline.md](docs/render-pipeline.md) — five-stage render pipeline, cost shape, cadence drivers
- [docs/visibility-gating.md](docs/visibility-gating.md) — render suppression for off-screen sidebars
- [docs/tmux-hooks-and-integration.md](docs/tmux-hooks-and-integration.md) — every integration point (hooks, scripts, env vars)

## Performance

| Metric | Value |
|--------|-------|
| Click latency (focus pane) | ~80ms (4 tmux ops in one fork) |
| `prefix+Up/Down` nav | ~30ms (sidebar-focus-nav, pure tmux, no Go fork) |
| leader+leader / `prefix+Tab` toggle | ~30ms (sidebar-switch-last, pure tmux, no Go fork; was ~0.5s) |
| Key navigation render | ~0.5ms |
| sidebar-go exec under endpoint security | ~230-600ms/exec (why nav + toggle avoid forking it) |
| Background refresh (tmux I/O) | ~540ms (non-blocking, on tea.Cmd workers) |
| Input latency | <1ms (Update goroutine never blocks on I/O) |
| Refresh model | Hook-driven + fsnotify watchers + 1s fallback tick |
| Verb refresh cadence | ~1s (driven by loadTree tick) |
| Spinner / blink ticks | 150ms / 700ms (skip render when no animating panes) |
| Binary size | ~12MB (unstripped, required for endpoint security compatibility) |
| Dependencies | bubbletea, bubbles, lipgloss, fsnotify, toml, termenv |
