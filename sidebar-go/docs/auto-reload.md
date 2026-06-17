# Auto-Reload on Binary Change

> **Status: standalone path.** The self-watching re-exec below is how the
> **standalone** sidebar (bare `sidebar-go`) reloads. Under the daemon +
> thin-client model (now primary), **only the daemon** watches the binary and
> re-execs; `display` clients carry no binary watcher — they hit EOF on the
> daemon's swap and re-exec at the reconnect handshake (newer mtime or proto
> bump). This collapses the fleet-wide re-exec burst to one, the endpoint-security
> mitigation. See [architecture.md](architecture.md) "Binary upgrade". The fsnotify
> + atomic-rename + `tea.Quit → syscall.Exec` mechanics here are identical on both
> paths; only *who* watches differs (`daemon.go:watchBinaryUpgrade` vs the
> standalone `tea_commands.go:startBinaryWatcherCmd`).

After `make install`, the running sidebar swaps itself out for the new binary in-place. No manual `prefix T` reload needed.

## Why

Sidebar processes are long-lived: one per attached tmux client × one per window with the sidebar enabled. Without auto-reload, every code change required manually killing and respawning every sidebar pane to pick up the new binary. With many windows open, that meant rebuilding mental state on every iteration. Auto-reload turns the dev loop into:

```
edit → make install → done
```

Sidebar visibly blinks once and reappears with the new code in place. tmux pane keeps its file descriptors, so the new process attaches to the same TTY — no pane churn, no flicker beyond a single repaint.

## What

When a new binary lands at the install path:

1. The running process unwinds bubbletea (alt-screen off, cursor on, mouse off, terminal modes restored).
2. `runBubble` returns from `tea.Run`.
3. `syscall.Exec(binaryPath, os.Args, os.Environ())` replaces the process image.
4. New process starts, calls `runBubble` again, attaches to the same TTY, repaints.

From the user's perspective: one frame of blank, sidebar back. Cursor position, focus, marks — all reloaded from shared state.

## How

### 1. Detection — fsnotify on the parent directory

On startup an fsnotify watcher is added on the parent directory of `os.Executable()`, **not** on the file path itself.

Why the directory? Because the install pattern is an atomic rename. fsnotify on the file path would lose the watch the moment the inode changes (the watch is on the *inode*, not the *path*). Watching the directory and filtering by basename survives renames cleanly.

```go
// tea_commands.go:startBinaryWatcherCmd
dir, base := filepath.Split(binaryPath)
w.Add(dir)
for ev := range w.Events {
    if filepath.Base(ev.Name) != base { continue }
    ...
}
```

### 2. Event filter - Create, Rename, and Write

`go build -o X` writes a temp file and renames it over X, so the expected event is a Create or Rename. However, macOS FSEvents/kqueue can coalesce that into a bare Write, so the filter accepts all three. The Makefile's atomic-rename pattern ensures the binary is complete before the event fires; the re-exec failure loop is the safety net if a mid-write slips through.

`tmux/Makefile`'s `install-%` target uses atomic rename:

```make
install-%: build-%
	mkdir -p $(INSTALL_DIR)
	cp .bin/$* $(INSTALL_DIR)/$*.tmp
	codesign -s - $(INSTALL_DIR)/$*.tmp 2>/dev/null || true
	mv $(INSTALL_DIR)/$*.tmp $(INSTALL_DIR)/$*
```

The `mv` fires a single `Rename` event with the final binary fully written.

### 3. Handoff - `tea.Quit` then `syscall.Exec`

On `binaryChangedMsg`, the model sets `reExecRequested = true` and returns `tea.Quit`:

```go
// tea_model.go:Update
case binaryChangedMsg:
    reExecRequested = true
    return m, tea.Quit
```

bubbletea unwinds first, `runBubble` notices `reExecRequested == true` after `tea.Run` returns, then calls `syscall.Exec`. Exec'ing mid-render leaves the terminal in raw mode + alt-screen, which the new process can't fully recover from - you'd see a stuck cursor and dead keystrokes.

### 4. Self-stopping watcher

The watcher closes its fsnotify handle the moment it fires `binaryChangedMsg`. No point holding the slot while we're about to exec away.

## Implementation

| File | Symbol | Responsibility |
|------|--------|----------------|
| `tea_commands.go` | `startBinaryWatcherCmd` | fsnotify watcher Cmd that emits one `binaryChangedMsg` |
| `tea_messages.go` | `binaryChangedMsg` | Message dispatched on rebuild |
| `tea_model.go` | `Update`/`runBubble`/`reExecRequested` | Quit then exec handoff |
| `tmux/Makefile` | `install-%` target | Atomic-rename install pattern |

## Pitfalls observed

- **Don't `syscall.Exec` from inside `Update`** - bubbletea hasn't restored terminal state yet.
- **Write events are accepted but atomic rename is still preferred** - the Makefile uses temp+rename so the binary is complete before any event fires. The re-exec failure loop handles the edge case.
