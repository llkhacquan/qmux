# Visibility Gating

> **Applies to both paths.** The visibility gate runs in standalone mode AND in
> thin-client `display` mode. Thin clients gate render on `windowActive` via
> `applySnapshot` - the daemon pushes snapshots to all clients, but only visible
> clients repaint. The leader-elected `loadTree` pattern below applies to the
> standalone fallback; the daemon model replaces it with a single engine.

The "game-engine" pattern: state always ticks, render only when visible. Hidden sidebars keep their model in lock-step with the visible leader so the visibility flip paints correctly with no jank, but the render plane and any side-effect Cmds are skipped while nobody's looking.

## Why

A typical fleet is 9 sidebar processes (3 attached clients × 3 windows with the sidebar enabled). Pre-gate, every one of them was running `composeStyledLines` at 6.6 fps from `spinTickMsg`, plus a 1Hz `loadTreeCmd` (10+ tmux fork+exec). Most of that work was invisible: 8 of 9 sidebars at any given moment have no visible terminal pane.

`shared-state-sync.md` already covers the fork-side win — leader-elected `loadTree` so only the visible sidebar runs the expensive data load. This doc is the render-side counterpart: a single `m.windowActive` flag gates *every* repaint and side-effect, while the raw model state still mutates so the next visible flip is instant.

The discriminator is `sidebarWindowIsActive()` (in `main.go`), polled once per tick and cached in `m.windowActive` (in `tea_model.go`). One-second granularity - the worst-case staleness on hidden-to-visible transition is one tick.

## Data plane vs render plane

| Plane | Hidden sidebar behavior |
|-------|-------------------------|
| **Data plane** (model state mutations) | Always runs. `m.spinFrame++`, `m.blinkOn` toggle, `applyStatusTransitions`, peer-cursor mirror, `loadSharedRowsCmd` reading the leader's snapshot. |
| **Render plane** (`composeStyledLines` + `viewport.SetContent`) | Skipped. The bytes would land in a tmux pane nobody's looking at; tmux replays the LATEST frame on visibility flip anyway. |
| **Side-effect Cmds** (publishing, fork bursts) | Skipped. Hidden sidebars are pure consumers of shared state; the leader does the writing. |

The split keeps the data plane in lock-step across the fleet so the render-plane skip is invisible to the user — when they switch windows, the cursor highlight, animation phase, and last-active state are already correct, no 1-tick re-snap.

## Gated sites

Multiple sites in `tea_model.go::Update` gate `refreshContent` on `m.windowActive`. Re-grep when line numbers shift:

```bash
grep -n 'windowActive && \|m\.windowActive\|!m\.windowActive' tea_model.go
```

| Site | Without gate | With gate |
|------|--------------|-----------|
| `rowsLoadedMsg` | composeStyledLines + viewport.SetContent on every shared-state read, hidden or not. | Hidden: rows + paneRows + activePaneID still mutate; just no repaint. Visible: paint as before. |
| `spinTickMsg` | 6.6 fps x 9 sidebars x full pipeline, even when no panes are running. | Hidden: spinFrame still bumps so animation phase is correct on flip; no repaint. Visible: existing `hasStatus("running") || shouldRefreshForMarch` gate still applies. |
| `blinkTickMsg` | Same shape as spinTick at 700ms cadence. Smaller impact, same gate for symmetry. | Hidden: blinkOn still toggles; no repaint. |
| `cursorChangedMsg` | fsnotify burst from peer click -> repaint x N instances per click. | Hidden: cursor/active mirror still applies (so visibility flip paints correctly); no repaint, no follow-up `loadTreeCmd` fork burst. |
| `controlNotifyMsg` | Daemon control conn notification -> repaint on every pane move. | Hidden: state mutates; no repaint. |
| `applySnapshot` (client mode) | Daemon snapshot -> repaint even when off-screen. | Hidden: snapshot applied to model; repaint gated. |

Two complementary gates use the inverted form to skip side-effect Cmds entirely:

| Site | Cmd skipped on hidden |
|------|----------------------|
| `rowsLoadedMsg` | `publishRowsCmd` - only the leader writes shared state. Hidden sidebars republishing what they just read is busywork. |
| `tickMsg` | `loadTreeCmd` (the 10+ tmux capture-pane fork burst). Replaced by `loadSharedRowsCmd` - same `rowsLoadedMsg` shape, no forks. |
| `ctxChangedMsg` | `loadTreeCmd` - ctx burst on a hidden sidebar would just throw away the result on next tick. Watcher stays armed via `waitCtxCmd` so the next ctx event isn't dropped. |

## Edge: hidden→visible transition

The 1Hz `tickMsg` is the visibility-flip beat. Sequence:

1. User switches to a window whose sidebar was hidden.
2. Within ≤1s, that sidebar's next `tickMsg` runs `m.windowActive = sidebarWindowIsActive()` → flips `false → true`.
3. Same `tickMsg` branch sees `windowActive == true`, dispatches `loadTreeCmd` (no longer the cheap `loadSharedRowsCmd`).
4. `loadTreeCmd` returns `rowsLoadedMsg` ~50–200ms later.
5. `rowsLoadedMsg` handler now sees `windowActive == true`, calls `refreshContent`, paints fresh frame.

Worst-case staleness on visit: one tick interval (~1s) + loadTree time. Until the new frame paints, the user sees whatever the prior leader last published — never blank, just slightly stale.

Critical: the cursor/active mirror runs *unconditionally* on hidden sidebars (`cursorChangedMsg` mutates state regardless of `windowActive`, only the repaint is gated). Without that, the moment of visibility flip would show stale cursor highlight for one tick before the next `tickMsg` reload corrected it. With the mirror, the mutation already landed via fsnotify; the flip just paints what's already in the model.

Same logic for `m.spinFrame` — bumped on every `spinTickMsg` even when hidden so the rainbow comet phase is continuous across the flip.

## Why not gate by `m.focused` instead

`m.focused` tracks whether the sidebar pane has *keyboard focus*. The user can be working in nvim or a terminal pane in the same window — the sidebar is on screen, but unfocused. Gating render on `m.focused` would freeze the sidebar's animation while the user types in nvim, defeating the point.

`windowActive` is the right granularity: the sidebar's window is the current client window. The user is either looking at it or its peer panes; either way, the sidebar's frame is on the wire.

## Why not gate every `refreshContent` call site

Many call sites in `Update` are user-driven: keypress, mouse, focus event. Those are visible-only by construction (the sidebar is the input target — hidden sidebars don't receive their own user events). Adding a redundant `windowActive` check buys nothing.

The four gated sites are the periodic + peer-driven repaints — the ones that fire across the whole fleet regardless of which sidebar the user is interacting with. Gating those covers the vast majority of wasted work; gating user-driven paints would just be noise.

## Implementation

| File | Symbol | Responsibility |
|------|--------|----------------|
| `main.go` | `sidebarWindowIsActive` | tmux query: is sidebar's window the current client window? |
| `tea_model.go` | `windowActive` (field) | Cached result, refreshed once per `tickMsg` |
| `tea_model.go` | `tickMsg` handler | Polls + flips windowActive; branches loadTreeCmd vs loadSharedRowsCmd |
| `tea_model.go` | `rowsLoadedMsg` handler | State always mutates; refreshContent + publishRowsCmd gated |
| `tea_model.go` | `spinTickMsg` / `blinkTickMsg` handlers | spinFrame/blinkOn always advance; refreshContent gated |
| `tea_model.go` | `cursorChangedMsg` handler | Cursor/active mirror always applies; refreshContent + loadTreeCmd gated |
| `tea_model.go` | `ctxChangedMsg` handler | loadTreeCmd skipped on hidden; watcher re-armed |
| `tea_commands.go` | `loadSharedRowsCmd` | Forkless replacement for loadTreeCmd on hidden sidebars |
| `tea_commands.go` | `publishRowsCmd` | Parameter-based `isActive bool` guard (from caller's `m.windowActive`) |

## Pitfalls observed

- **Don't gate state mutations on `windowActive`.** Only gate the render call (`refreshContent`) and side-effect Cmds (`publishRowsCmd`, `loadTreeCmd`). Stop mutating `m.spinFrame` / `m.blinkOn` / `m.activePaneID` on hidden and the visibility flip will paint a stale frame before the next tick corrects it.
- **`windowActive` is one-tick stale.** It's only refreshed in `tickMsg`. Any handler that needs visibility info more freshly than 1Hz must call `sidebarWindowIsActive()` itself — but each call is a tmux fork, so don't.
- **Hidden sidebar still owns its own keyboard focus state.** `m.focused` is tracked independently from `m.windowActive`. A hidden sidebar that previously had focus keeps `m.focused = true` until a `BlurMsg` arrives; the cursor-mirror logic in `cursorChangedMsg` correctly checks `m.focused`, not `m.windowActive`, when deciding whether to adopt a peer's cursor.
- **Cross-reference `shared-state-sync.md`** for the publish side — that doc explains the leader-write pattern; this one explains the consumer-read + render-skip side.
