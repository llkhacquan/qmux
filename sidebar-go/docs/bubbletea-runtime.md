# Bubbletea Runtime

The interactive TUI is built on [bubbletea](https://github.com/charmbracelet/bubbletea) + bubbles + lipgloss. Migrated off tcell in a 9-phase rewrite.

> **Note:** this runtime is shared by both the `display` thin client and the
> standalone fallback. In `clientMode` (display), state arrives as daemon
> snapshots and mutations leave as intents instead of reading/writing the
> shared-state file — see [architecture.md](architecture.md). The Model/Update/View
> structure described here is identical on both paths.

## Why

The tcell event loop hand-rolled message dispatch, frame composition, and async refresh in one big `runInteractive()` function. Pain points:

- **Concurrency model was implicit** — refresh goroutine + main event loop coordinated via mutexes + channels + signals (SIGUSR1). Easy to introduce races.
- **Cell-poking renders** — `render_beauty.go` post-processed cells after the main render to inject status overlays. Two-pass logic, ordering bugs.
- **Hand-coded scroll/search/keymap** — every primitive was bespoke.
- **No adaptive theming** — colors hardcoded to dark backgrounds.

bubbletea's Elm-style `Model + Update + View` makes the data flow explicit: every state change is a Msg, every side effect is a Cmd. Combined with `bubbles` (viewport, textinput, key) and `lipgloss` (styles), most of the bespoke code is gone.

## What changed

- **`runInteractive()` shrunk to ~25 lines** — just calls `runBubble()`.
- **All tmux/fsnotify I/O moved to tea.Cmd factories** — runs on bubbletea's worker pool, never blocks Update().
- **One source of truth for state** — `teaModel` struct. Every mutation goes through `Update(msg) → (newModel, Cmd)`.
- **lipgloss styles** centralized in `tea_styles.go` - config-driven via TOML `[theme]` section (defaults to Catppuccin Mocha).
- **Per-segment styling** — `tea_render.go::composeStyledLines` builds each row with separately rendered left-border / inner / right-border segments, so each carries its own color without post-hoc cell pokes.

## File layout

| File | Responsibility |
|------|----------------|
| `tea_model.go` | `teaModel`, `Init`, `Update`, key/mouse handlers, `runBubble` |
| `tea_view.go` | `View()`, viewport plumbing, search bar |
| `tea_render.go` | `composeStyledLines`, border row composition, status overlays |
| `tea_messages.go` | All `Msg` types (rowsLoaded, tick, ctxChanged, cursorChanged, spinTick, blinkTick, binaryChanged, …) |
| `tea_commands.go` | All `Cmd` factories (loadTreeCmd, tickCmd, startContextWatcherCmd, startCursorWatcherCmd, startBinaryWatcherCmd, …) |
| `tea_keymap.go` | bubbles/key bindings |
| `tea_styles.go` | lipgloss styles + rainbow palette helpers |
| `tea_legacy.go` | tcell-free helpers extracted during the migration |

Data layer (unchanged by the rewrite): `tree.go`, `git.go`, `git_watch.go`, `gh.go`, `status.go`, `context.go`, `helpers.go`, `config.go`, `usage.go`, `tmux.go`, `commands.go`, `hidden.go`, `question.go`, `activity.go`.

Added post-rewrite: `daemon.go`, `display.go`, `protocol.go` (daemon architecture), `render_cell.go`, `render_decorators.go`, `render_march.go`, `render_state.go` (cell-grid pipeline), `user_config.go` (TOML config), `logging.go` (log rotation), `tmux_control.go` (persistent control conn).

## Performance wins captured during the rewrite

| Pitfall | Fix | Result |
|---------|-----|--------|
| 4 sequential tmux fork+exec on click | Pipeline into one fork via `tmux a \; b \; c` | Click latency ~1s → ~80ms |
| Update blocking on tmux I/O | All tmux calls now in `tea.Cmd` workers | Input latency stays <1ms |
| Peer sidebars laggy on focus changes | fsnotify on shared-state emits cursor + active in one event | Single-hop peer repaint, no 1s tick wait |
| 5+ tmux focus hooks per pane switch (~50 subprocess execs) | (1) reorder chained tmux so `set-option @tmux_sidebar_main_pane` fires first, (2) early-return in `cmdOnFocus` when stored matches, (3) dedup `writeSharedCursorActive` | ~50 → ~10 execs per click |

## Spinner / blink ticks

Two periodic Cmds drive animations:

- **`spinTickCmd`** — 150ms cadence. Bumps `m.spinFrame` and re-renders only when at least one pane is running. Drives the rotating arrow + rainbow color cycle.
- **`blinkTickCmd`** — 700ms cadence. Toggles `m.blinkOn` and re-renders only when at least one pane is `needs-input`. Drives the peach pulse on needs-input cards.

Both re-arm unconditionally but skip `refreshContent` when there's nothing to animate — cheap to keep them running.

## Open follow-ups

- Adaptive light/dark via `lipgloss.HasDarkBackground()` - deferred.
- Bottom preview area (capture-pane output for cursor card) - old TODO.
- Click-on-press only; tcell had a release-fallback for tmux focus consumption - watch for missed clicks.

Resolved since initial rewrite:
- ~~Catppuccin palette swap config~~ - done via `[theme]` in TOML config.
- ~~De-dupe loadTree across instances~~ - done via daemon architecture (single engine, thin clients).

## Pitfalls observed

- **Don't `syscall.Exec` from inside `Update`** — bubbletea hasn't restored terminal state yet. Use `tea.Sequence(tea.Quit, …)` and exec after `runBubble` returns. See [auto-reload.md](auto-reload.md).
- **Don't run tmux I/O inline in `Update`** — Update is the only goroutine that mutates model state; blocking it stalls input. Always wrap in `tea.Cmd`.
- **Don't share state between Cmd workers and Update via globals without thinking** — Cmd factories return Msgs, Msgs are dispatched serially to Update. That's the synchronization. `globalCtxWatcher` is one of the few exceptions (read-mostly + RWMutex internal).
