# IPC: UDS Datagram Notification Layer

Fast-path notification between sidebar processes (helpers ↔ bubbletea, bubbletea ↔ bubbletea), running alongside the existing shared-state file. Doorbell semantics today; foundation for typed peer messaging later.

> **Status: implemented.** The datagram doorbell lives in `internal/sidebarstate`
> and is still the wake-up path for lean hook helpers. Note: the daemon now also
> exposes a *separate* UDS **stream** socket (`daemon.sock`) for snapshot/intent —
> distinct from this datagram doorbell. See [architecture.md](architecture.md)
> "Two UDS layers". (Typed-event routing was sketched but not built.)

## Why

`docs/shared-state-sync.md` describes the current sync model: helper writes shared-state file, every sidebar wakes via fsnotify, re-reads, repaints. Works fine for correctness; becomes the bottleneck once we care about latency.

Where current model strains:

- **fsnotify edge cases on macOS.** kqueue occasionally drops bursty Create/Rename events. A 1Hz polling tick is the existing backstop — adds ≤1s lag and a needless capture-pane fork burst.
- **No targeted messaging.** Every shared-state write wakes every sidebar. No way to say "sidebar B specifically, you own this window now" without abusing the file.
- **No event payload.** All semantics inferred from diffing the file. Adding a new event type requires extending the schema and a re-read by every listener.

UDS datagram fixes all three. Helper fork cost (~50–100ms boot of `sidebar-go`) still dominates wall-clock latency for tmux hooks until step 3 (lean helper binary) lands — but UDS is the prerequisite that makes step 3 worth doing.

## What

One unixgram socket per long-running bubbletea sidebar, keyed by PID:

```
~/.local/state/tmux-sidebar/sock/<pid>.sock
```

A datagram on any of these sockets means: **shared-state may have changed; re-read it.** Today the payload is a single fixed byte — receivers don't inspect it. The wire format is reserved so future versions can route typed messages (`focus`, `exit`, `claim-window`, …) without breaking peers.

The shared-state file remains the source of truth:

- Catppuccin status formatter (`cmdWindowName`) reads it synchronously every status redraw, with no listener running.
- Crash recovery: a sidebar restarting after a crash reads the last-known cursor/active immediately.
- fsnotify stays as a fallback for the cases where UDS misses (socket dir not yet created, listener not yet bound, dropped datagram under load).

## How — listener side (bubbletea sidebar)

At bubbletea boot, `startCursorWatcherCmd` already creates the snapshot channel that drives `cursorChangedMsg`. Extend it to also bind a UDS:

```
listenUDS(out chan<- []byte) *net.UnixConn
    1. mkdir -p ~/.local/state/tmux-sidebar/sock (mode 0700)
    2. unlink any stale <self-pid>.sock
    3. net.ListenUnixgram("unixgram", <pid>.sock)
    4. spawn goroutine:
         loop:
           ReadFromUnix → forward to out (drop on full)
       defer conn.Close + os.Remove(socket)
```

Both fsnotify and UDS pumps push `sharedStateSnapshot{cursor, active, last}` into the **same** `out` channel. Bubbletea's `Update` stays source-agnostic; the existing `cursorChangedMsg` handler keeps its dedupe-on-`changed` guard, so fsnotify and UDS arriving together for the same write produces one repaint, not two.

Failure mode: if `listenUDS` returns nil (mkdir failed, port in use, sandboxed), the sidebar runs on fsnotify alone — same behavior as today. No regression.

Socket lifecycle:

| Event | Cleanup |
|-------|---------|
| Sidebar exits cleanly | goroutine `defer os.Remove(path)` |
| Sidebar killed (SIGKILL / crash) | socket file remains; `notifyPeers` unlinks it on next dial-fail |
| PID reuse before cleanup | tiny risk; mitigated by `kill -0 <pid>` liveness probe before dial |

## How — sender side (any helper or peer)

```
notifyPeers(event []byte)
    for each <pid>.sock in socket dir:
        if pid == self:                continue
        if kill -0 pid fails (ESRCH):  unlink, continue
        DialUnix("unixgram", path)
        SetWriteDeadline(20ms)
        Write(event)
        Close()
```

Wired at exactly one place: the end of `writeSharedState` in `helpers.go`. Every shared-state write fans out a doorbell, mirroring fsnotify semantics. No call-site changes elsewhere — every existing writer (`writeSharedCursorActive`, `publishRowsCmd`, `persistDonePanes`, etc.) is covered automatically.

Cost per notify: ~1ms × N listeners on macOS. With 4–8 sidebars across windows, ~4–8ms total — acceptable, and shared-state writes are themselves rare on the fast path (one per pane focus, one per leader's 1Hz publish).

## How — interaction with existing flow

Today's flow with fsnotify only:

```
helper.cmdOnFocus
  ├─ writeSharedCursorActive
  │    └─ writeSharedState
  │         └─ os.Rename(tmp, shared-state)
  └─ (helper exits)

every sidebar:
  fsnotify(rename) → readSharedState → cursorChangedMsg
                                       ↓ if changed
                                       refreshContent
                                       ↓ if windowActive
                                       loadTreeCmd  (fix from b3c2d70)
```

After UDS lands:

```
helper.cmdOnFocus
  ├─ writeSharedCursorActive
  │    └─ writeSharedState
  │         ├─ os.Rename(tmp, shared-state)
  │         └─ notifyPeers(doorbell)        ← new, ~5ms total
  └─ (helper exits)

every sidebar:
  EITHER UDS arrives first  (fast path, μs)
  OR     fsnotify arrives   (fallback, ms)
  → readSharedState → cursorChangedMsg → … same as before
```

`cursorChangedMsg`'s `changed`-guard handles the duplicate delivery: whichever source fires first updates state; the second sees `changed == false` and bails before the loadTree call.

## Phasing

**Phase 1 — doorbell only (landed in `ec40a29`).**
- `ipc.go` with `socketDir`, `socketPathForPid`, `listenUDS`, `notifyPeers`, `udsDoorbell`.
- `helpers.go` `writeSharedState` calls `notifyPeers(udsDoorbell)` after rename.
- `tea_commands.go` `startCursorWatcherCmd` adds UDS pump feeding the existing snapshot channel.
- No payload semantics. Reserved first byte = `0`.
- No listener-side changes for handling typed events yet.

**Phase 2 — typed events (deferred indefinitely; YAGNI — no consumer).**
- Envelope sketch retained for future reference: `byte(eventType) | optional CBOR/JSON payload`.
- Candidate event types: `focus` (current implicit), `exit`, `claim`, `cursor-mirror`.
- Listener would route by first byte; current handlers stay the default for byte `0`.

**Phase 3 — lean helper (landed alongside this revision).**
- New binary `sidebar-notify` (`tmux/sidebar-notify/`): boots in ~15ms vs the fat sidebar-go's ~50–100ms, links only stdlib + `internal/sidebarstate`.
- Phase 1's `ipc.go` and the bottom half of `helpers.go` (`shared-state` IO + flock) moved into `tmux/internal/sidebarstate/` so both binaries share one implementation. Drift risk eliminated.
- `sidebar.tmux` rewires the five on-focus hooks to point at `sidebar-notify`, with auto-fallback to `sidebar-go on-focus` when the lean helper isn't installed (old setups keep working).
- Active-pane source-of-truth migrated to `sharedState.Active` via a new `activePaneID()` helper. Five readers (tree.go, tea_commands.go, cmdFocusSidebar, switchByOffset, main.go boot) used to fork tmux to read `@tmux_sidebar_main_pane`; now they read shared state with the option as a cold-start fallback. User-action writers (cmdToggle, cmdSwitchLast, switchByOffset, cmdJumpTo, tea_model) keep writing the option as a defensive dual-write for any external consumers — those paths aren't latency-sensitive. With readers migrated, sidebar-notify drops its last tmux fork and the focus-hook hot path is now zero-fork.

End-to-end measurement on an idle laptop with a fleet of 10 sidebars: pane switch → shared-state write → UDS doorbell → first peer repaint, **~10ms wall-clock**. Down from ~250ms pre-phase-3, ~25ms with the stopgap fork still in place.

## Open questions

1. **Socket file mode.** `0700` for the dir, default `0600` for the socket itself? Single-user box; matters only if multiple Unix users share `~/.local/state/tmux-sidebar/sock` (we don't).
2. **Should `notifyPeers` skip writes from `publishRowsCmd`?** That's a 1Hz heartbeat from the leader. Peers don't actually need to repaint on it — `cursorChangedMsg`'s `changed` guard already discards row-only updates. So fan-out is wasted (~5ms × 1Hz × N peers). Cheap, but tagging the call would let us skip it. Defer until profile shows it matters.
3. **Failure observability.** Today fsnotify errors print to stderr (which the dup2 logger redirects). Mirror for UDS bind failure? Yes — one stderr line at boot if `listenUDS` returns nil so we know we're on fsnotify-only mode.
4. **Liveness probe race.** `kill -0 <pid>` succeeds → dial succeeds → process dies before `Write`. Caught by the 20ms write deadline; datagram silently dropped. Acceptable.
5. **PID wraparound.** macOS bumps `pid_max` enough that wraparound within a session is extremely unlikely. Skip mitigation.
6. **Should `listenUDS` retry on bind failure?** No — first failure usually means stateDir is unwritable; retry won't help. Fall through to fsnotify-only.

## Files added / changed

Phase 1 (`ec40a29`):

| File | Change |
|------|--------|
| `sidebar-go/ipc.go` | new — local `socketDir`, `listenUDS`, `notifyPeers`, `udsDoorbell` |
| `sidebar-go/helpers.go` | `writeSharedState` calls `notifyPeers(udsDoorbell)` after rename |
| `sidebar-go/tea_commands.go` | `startCursorWatcherCmd` spawns a UDS pump sharing the snapshot channel |

Phase 3 (this revision):

| File | Change |
|------|--------|
| `internal/sidebarstate/sidebarstate.go` | new — owns `Dir`, `FilePath`, `WithLock`, `ReadRaw`, `WriteRaw`, `PatchCursorActive`, plus the UDS layer (`SocketDir`, `SocketPathForPid`, `Doorbell`, `ListenUDS`, `NotifyPeers`) |
| `internal/sidebarstate/sidebarstate_test.go` | moved from `sidebar-go/ipc_test.go`; adds `PatchCursorActive` coverage |
| `sidebar-go/ipc.go` | deleted (logic moved to `internal/sidebarstate`) |
| `sidebar-go/helpers.go` | `readSharedState`/`writeSharedState`/`withSharedStateLock` delegate to the internal pkg |
| `sidebar-go/tea_commands.go` | `sharedStateFilePath()` → `sidebarstate.FilePath()`; `listenUDS` → `sidebarstate.ListenUDS` |
| `sidebar-go/config.go` | `stateDir()` is now a thin alias over `sidebarstate.Dir()` |
| `sidebar-notify/main.go` | new lean binary — argv parse, `set-option @tmux_sidebar_main_pane`, `PatchCursorActive` via `WithLock` + `WriteRaw`, pane-state badge clear |
| `Makefile` | `sidebar-notify` added to `BINS` (gets the same build/codesign/atomic-mv treatment) |
| `sidebar-go/sidebar.tmux` | factored `resolve_binary`; on-focus hooks point at `$NOTIFY_BINARY` with `$BINARY on-focus` as auto-fallback |

## Non-goals

- Replacing the shared-state file. Stays as source of truth.
- Replacing fsnotify. Stays as fallback.
- Cross-host IPC. Local-only by design (UDS).
- Reliable delivery. Datagrams are best-effort; the file remains authoritative.
