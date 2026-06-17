# sidebar-go architecture

One binary, three runtime roles. Topic docs go deep; this is the map.

## Roles

```
                        sidebar-go binary
                              │
   ┌──────────────┬──────────┴───────────┬─────────────────────┐
   │ serve        │ display              │ bare / fallback     │ toggle ensure
   ▼              ▼                       ▼                     ▼ on-focus notify …
┌────────┐  ┌──────────────┐     ┌──────────────┐      ┌──────────────┐
│ Daemon │  │ Display      │     │ Standalone   │      │ Hook helper  │
│headless│  │ owns pane TTY│     │ full engine  │      │ short-lived  │
│the only│  │ pure UI      │     │ + TTY        │      │ tmux op      │
│ engine │  └──────────────┘     └──────────────┘      └──────────────┘
└────────┘
```

The tmux sidebar pane spawns `sidebar-go display` (`display.go`). Bare
`sidebar-go` (`runInteractive`) is the legacy full engine, kept as the standalone
fallback and for dev runs.

## Topology — one engine, many screens

```
   display clients                 daemon.sock              sidebar-go serve
   (1 per window)                  (UDS stream)             (the daemon)
 ┌────────────────┐   hello/intent     │     snapshot   ┌─────────────────────┐
 │ display win1   │────────────────────┼───────────────▶│ engine (loadTree)   │
 └────────────────┘                    │                │ broadcast hub       │
 ┌────────────────┐                    │                │ tmux -C  (1 conn) ──┐│
 │ display win2   │◀───────────────────┤                │ git watcher         ││
 └────────────────┘                    │                │ usage scanner       ││
 ┌────────────────┐                    │                │ binary watcher      ││
 │ display winN   │◀───────────────────┘                └─────────────────────┘│
 └────────────────┘                                              ▲              │
   bubbletea + render LOCAL                                      └──────────────┘
```

The daemon is the permanent leader: **one** loadTree, **one** control conn,
**one** git watcher, **one** usage scan. Clients are pure UI. This replaced a
leader-election model where every window ran a full engine and synced through a
shared-state file.

## Display connect + intent flow

```
User      display client                    daemon (serve)
 │              │  dial daemon.sock                │
 │              │─────────────────────────────────▶
 │              │   (no daemon? flock-gated        │
 │              │    lazy-start: one winner forks  │
 │              │    `serve`, losers keep dialing) │
 │              │  hello {proto,pid,pane,window}   │
 │              │─────────────────────────────────▶
 │              │  welcome {proto, ok}             │
 │              │◀─────────────────────────────────
 │              │  (proto mismatch OR newer binary │
 │              │   on disk → re-exec onto image)  │
 │              │  snapshot (initial)              │
 │              │◀─────────────────────────────────
 │  keypress    │                                  │
 │─────────────▶│ local nav → render now           │
 │              │  intent {cursor|scroll|           │
 │              │   clear_done|toggle_hidden|reload}│
 │              │─────────────────────────────────▶
 │              │  snapshot (all clients converge) │
 │              │◀─────────────────────────────────
 │     EOF → connManager reconnects; last snapshot stays on screen
```

The TUI starts **before** the daemon connects — there is no blocking pre-flight.
Until the first snapshot lands the client renders an animated "waiting for
daemon…" spinner (not a blank pane), and the connManager lazy-starts + dials the
daemon on a tight 25ms poll in the background (a freshly forked daemon binds its
socket ~0.3s after fork). If no snapshot arrives within `cfgStandaloneDeadline()`
(default 3s, configurable via TOML), the model quits and `runDisplay` hands off to the in-process engine — so a
pane is never stuck spinning when the daemon truly can't boot.

Focus (`select-pane`) is **client-side**, not an intent - so `switch-client`
lands on the right display among many control conns. The only intents are
`cursor`, `scroll`, `clear_done`, `reload`, `toggle_hidden`.

## Daemon engine triggers

```
   control notify ─┐
   context change ─┤
   1s safety tick ─┼──▶ reloadTree ──┐
   reload intent  ─┘    (loadTree)   │
                                     ▼
   shared-state write ──────────▶ markDirty ──▶ [50ms debounce] ──▶ broadcastNow
   (hook helper)                                                    marshal once,
                                                                    fan to all
```

Marshal-once-send-many → cost is flat in client count. The per-client writer is
**drop-oldest**, so one stuck display never stalls the hub.

## Binary upgrade — single re-exec

```
make install        daemon                              clients
   │  atomic-rename    │                                   │
   │──────────────────▶│ fsnotify fires (watches own img)  │
   │                   │  reexec (advisory) ──────────────▶│
   │                   │  tear down control conn           │
   │                   │  (reap tmux -C child)             │
   │                   │  syscall.Exec → new image         │
   │                   │  (same PID, re-acquires flock)    │
   │                   │            EOF ──────────────────▶│ reconnect + handshake
   │                   │                                   │ newer mtime / proto bump?
   │                   │                                   │   → re-exec onto image
```

**Only the daemon re-execs** — clients just reconnect. Collapsing the old
fleet-wide re-exec burst to one is the endpoint-security mitigation.

## Two UDS layers — do not conflate

```
  daemon.sock  (SOCK_STREAM)  ──▶  snapshot / intent   ──▶  daemon ↔ display
  sock/<pid>.sock (SOCK_DGRAM) ─▶  1-byte wake-up       ──▶  shared-state peers
  focus.sock   (SOCK_DGRAM)    ─▶  "pane|window"        ──▶  after-select-pane hook
                                                             (socat, fork-free)
```

`focus.sock` is the fixed-path datagram the `after-select-pane` hook writes via
`sidebar-focus-send` (socat) so the most frequent focus event never forks the
heavy binary. The daemon runs the shared `trackFocus` on it. Window/session
switches stay on the forked `on-focus` because they also need `cmdEnsure`, and a
detached daemon must not `split-window`.

The `shared-state` JSON file is no longer the interactive sync path; it survives
as the bridge to hook helpers (`on-focus`, `notify`, `cmdWindowName`) that mutate
state without linking the engine. The daemon watches it as one more reload
trigger and re-publishes `rows` into it for out-of-process readers.

## Failure modes

| Failure | Behavior |
|---------|----------|
| No daemon at start | spinner shows; flock lazy-start spawns one; 25ms poll connects on bind |
| Daemon crashes | clients EOF → reconnect → flock respawns one (last snapshot stays) |
| Daemon never boots (≤3s) | standalone-fallback deadline quits client → in-process full engine |
| Proto mismatch mid-upgrade | `welcome.ok:false` → client re-execs onto new binary |
| Slow/stuck client | per-conn drop-oldest writer; never stalls the hub |
| Control conn dies in daemon | supervised reconnect with backoff |

Invariant: a display always shows *something* — daemon state when connected,
standalone-rendered when not.

## Topic docs

[bubbletea-runtime](bubbletea-runtime.md) ·
[render-pipeline](render-pipeline.md) ·
[status-detection](status-detection.md) ·
[shared-state-sync](shared-state-sync.md) ·
[ipc-uds-notify](ipc-uds-notify.md) ·
[auto-reload](auto-reload.md) ·
[visibility-gating](visibility-gating.md) ·
[tmux-hooks-and-integration](tmux-hooks-and-integration.md)
