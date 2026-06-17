# Shared State + Multi-Instance Sync

> **Status: historical / standalone-only.** The leader-election model below was
> superseded by the daemon + thin-client architecture — see
> [architecture.md](architecture.md). The daemon now owns the single `loadTree`
> and pushes snapshots; clients no longer elect a leader or sync through this
> file. The leader/visibility code still runs in the **standalone fallback**
> (bare `sidebar-go`), and the shared-state file survives as the bridge to lean
> hook helpers (`on-focus`, `notify`). Read this for the standalone path and the
> file schema; read architecture.md for the live interactive path.

Multiple sidebar instances (one per attached client × per window) stay in sync via a single JSON file + fsnotify. One instance does the expensive work, the rest read the result.

## Why

Every attached tmux client opens its own sidebar pane. With 4–6 windows × 2 attached clients, you can easily end up with 8 sidebar processes side-by-side. If each one ran its own `~540ms` tree refresh, the host would melt. They all show the same data anyway — there's no per-client information.

Goal: **N sidebars, O(1) tmux I/O.** Achieved via two coordinated patterns: (1) shared-state cursor sync (existing), and (2) leader-elected `loadTree` (added 2026-04-27 after pprof showed `prefetchPaneCaptures` dominating CPU).

## What

A single shared-state file under `~/.local/state/tmux-sidebar/shared-state` carries:

- `cursor` - current cursor pane ID (the highlighted card)
- `active` - currently active pane ID (the focused tmux pane)
- `active_window` - tmux window_id of the focused pane (quiet-blue for same-window cards)
- `last_active` - previous active pane (for switch-last toggle)
- `rows` - full row list (cards, borders, intent text)
- `pane_rows` - derived list of one row per pane (focusable subset)
- `scrolloff` - scroll margin
- `unread` - set of pane IDs with unseen status transitions
- `done_panes` - panes that transitioned running-to-idle (value: unix-millis timestamp)
- `asked_panes` - subset of done_panes where last message was a question
- `running_panes` - running-start timestamps for elapsed duration labels
- `view_y_offset` / `view_pinned` - shared scroll position across instances
- `hidden_sessions` - tmux sessions hidden from the sidebar view
- `ts` - `time.UnixMilli()` of write

Helpers in `helpers.go`:

- `readSharedState() sharedState`
- `writeSharedState(sharedState)` - atomic via `tmp + rename`
- `writeSharedCursorActive(cursor, active, activeWindow)` - partial update, no row clobber, **dedup-skips when unchanged**

## How — leader-elected `loadTree`

`loadTree` is the expensive workhorse: 1× `tmux list-panes -a`, ~10× `capture-pane` per N panes, plus git status forks. At 1Hz this is ~12 forks/sec per sidebar — multiplied by N sidebars it's the dominant CPU draw across the whole sidebar fleet.

**Leader rule:** only the sidebar whose tmux **window** is currently displayed runs `loadTree`. "Window-active" — not "pane-focused" — so the leader keeps refreshing while the user works in nvim or a terminal pane in the same window. The sidebar is still on screen; only sidebars in *other* (off-screen) windows are demoted to readers. Demoted sidebars read `Rows` + `PaneRows` from shared state — zero forks.

```
                tmux window switch
        ┌─────────────────┴─────────────────┐
        ▼                                   ▼
sidebar A (now active)              sidebar B (now hidden)
  tickMsg (1Hz):                      tickMsg (1Hz):
    sidebarWindowIsActive() = true      sidebarWindowIsActive() = false
    loadTreeCmd:                        loadSharedRowsCmd:
      list-panes + 2N capture-pane        readSharedState() → rowsLoadedMsg
      → rowsLoadedMsg                     (no forks)
    publishRowsCmd:
      writeSharedState(rows, paneRows)
      └──► fsnotify → cursor watcher in B
                       (currently consumes cursor/active only;
                        rows refresh on B's next tickMsg)
```

**Latency on focus regain.** When the user switches to a window with a hidden sidebar, that sidebar's next 1Hz tick polls `sidebarWindowIsActive()` → flips to active → runs `loadTreeCmd` in the same tick. Worst-case wait: one tick interval (~1s) + loadTree time (~50–200ms). Until then, the sidebar shows whatever the prior leader last published — so it's never blank, just slightly stale.

**Edge case: no active sidebar.** If the focused window has no sidebar pane, no leader exists. Hidden sidebars keep reading the last-published `Rows` (still fresh enough to look right), and the moment the user switches to a sidebar window, that sidebar becomes leader within ~1s.

**Empty fallback.** First boot — shared state has no rows yet. `loadSharedRowsCmd` falls back to `loadTreeCmd` for that one tick so the sidebar isn't blank. Once any sidebar becomes active and publishes, every hidden sidebar runs forkless.

**Implementation files:**
- `tea_model.go:tickMsg` — branches on `m.windowActive` (cached from `sidebarWindowIsActive()` once per tick)
- `tea_commands.go:loadSharedRowsCmd` — reads `Rows`/`PaneRows`/`Active`/`LastActive` from shared state
- `tea_commands.go:publishRowsCmd` — already gated to active sidebar; writes the rows other sidebars read
- `tea_model.go:ctxChangedMsg` — also gated, hidden sidebars stay armed but don't reload

## How — fsnotify-driven repaint

Each sidebar runs an fsnotify watcher on the state directory in `tea_commands.go:startCursorWatcherCmd`. Events are filtered to the shared-state file path; on hit it reads the file once, emits a snapshot with cursor, active, activeWindow, and other fields to the model.

```
sidebar A clicks card → focusCursorPaneCmd:
    1. tmux set-option @tmux_sidebar_main_pane <pid>  (FIRST in chain)
    2. tmux switch-client / select-window / select-pane
    3. writeSharedCursorActive(<pid>, <pid>, <wid>)   (one flock + file write)

→ fsnotify fires on shared-state in every sidebar (A and peers)
→ each sidebar's Update() gets cursorChangedMsg
→ peer A: sets cursor + active, refreshContent
→ peer B/C/…: same. Single repaint, no tmux roundtrip.
```

Critical detail: the focusing instance writes both `cursor` and `active` in **one atomic file replace**. Peers repaint with both fields in a single fsnotify hop — no 1s tick wait, no inconsistent intermediate state where active changes before cursor.

## How — write dedup

`writeSharedCursorActive` short-circuits when both fields already match:

```go
func writeSharedCursorActive(cursor, active, activeWindow string) {
    withSharedStateLock(func() {
        s := readSharedState()
        if s.Cursor == cursor && s.Active == active && s.ActiveWindow == activeWindow {
            return
        }
        s.Cursor = cursor
        s.Active = active
        s.ActiveWindow = activeWindow
        writeSharedState(s)
    })
}
```

Reason: tmux fires 5+ focus hooks per pane switch (`after-select-pane`, `after-select-window`, `client-session-changed`, `client-focus-in`, `session-window-changed`). Each forks `sidebar-go on-focus`, which writes shared state. Without dedup, that's 5+ fsnotify events per switch in every peer sidebar — wakes Update(), reads file, no-op repaint, repeat. With dedup, only the first write actually mutates; the rest are silent.

Pairs with the early-return in `commands.go:trackFocus` that bails when stored `@tmux_sidebar_main_pane` already matches the incoming paneID.

## How — race safety

All shared state mutations end up in the bubbletea Update goroutine via Msg dispatch. fsnotify deliveries are channelized through `tea.Cmd` factories, so:

- No mutex on model state.
- File reads from the watcher goroutine, but all interpretation of the read is in Update().
- Atomic file replace (`tmp + rename`) protects against torn reads.

## Implementation

| File | Symbol | Responsibility |
|------|--------|----------------|
| `helpers.go` | `sharedState`, `readSharedState`, `writeSharedState` | File schema + atomic IO |
| `helpers.go` | `writeSharedCursorActive` | Dedupped partial update for cursor+active |
| `tea_commands.go` | `startCursorWatcherCmd` | fsnotify watcher Cmd, emits snapshots |
| `tea_commands.go` | `waitCursorCmd` | Re-arm wrapper around the watcher channel |
| `tea_model.go` | `cursorChangedMsg` handler | Adopts cursor (if not focused) and active |
| `commands.go` | `cmdOnFocus` | Early-return when storedMainPane == paneID |
| `tea_model.go` | `focusCursorPaneCmd` | set-option FIRST, then select-pane |

## Pitfalls observed

- **Always set the tmux option FIRST in the chained focus command** — otherwise the hooks `select-pane` fires read the *old* main_pane value and miss the early-return optimization. Net effect: instead of dropping ~50 tmux execs per click, you keep all of them.
- **fsnotify often emits Write+Chmod pairs** — coalesce by reading the file once per sweep and pushing a single snapshot, drop overflow.
- **Peer sidebars must not reflect their own cursor writes** — focused instance owns its own cursor. The handler checks `sidebarHasFocus()` before adopting `msg.cursor` (active is always adopted).
