package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/llkhacquan/qmux/sidebar-go/internal/sidebarstate"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/fsnotify/fsnotify"
)

// binaryPath is the live executable, captured at startup. startBinaryWatcherCmd
// hooks fsnotify on its parent directory so an atomic rebuild rename
// (`go build -o`) wakes Update() immediately — no polling.
var binaryPath string

// startBinaryWatcherCmd installs the fsnotify watcher on the binary's
// directory and returns a Cmd that emits one binaryChangedMsg the moment a
// rebuild lands. The watcher self-stops after firing — the process is
// about to syscall.Exec into the new image anyway.
func startBinaryWatcherCmd() tea.Cmd {
	return func() tea.Msg {
		if awaitBinaryChange(binaryPath) {
			return binaryChangedMsg{}
		}
		return nil
	}
}

// awaitBinaryChange blocks until the atomic-rename swap that `make install`
// performs lands on path, returning true. Returns false on a watcher setup
// error or a closed channel. Shared by the interactive binary watcher (above)
// and the daemon's upgrade watcher (daemon.go) so both detect installs the same
// way. `go build -o X` writes a temp + renames over X, so we expect a Create or
// Rename event with the basename; macOS FSEvents/kqueue can coalesce that into a
// bare Write, so we catch Write too (the re-exec failure path is the safety net
// if a mid-write slips through).
func awaitBinaryChange(path string) bool {
	if path == "" {
		return false
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return false
	}
	defer w.Close()
	dir, base := filepath.Split(path)
	if err := w.Add(dir); err != nil {
		return false
	}
	for {
		select {
		case ev, ok := <-w.Events:
			if !ok {
				return false
			}
			if filepath.Base(ev.Name) != base {
				continue
			}
			if ev.Has(fsnotify.Create) || ev.Has(fsnotify.Rename) || ev.Has(fsnotify.Write) {
				return true
			}
		case _, ok := <-w.Errors:
			if !ok {
				return false
			}
		}
	}
}

// loadTreeCmd returns a Cmd that runs loadTree on bubbletea's worker pool and
// emits rowsLoadedMsg. Kept as a factory because we'll re-issue it from tick
// handlers in later phases without leaking across Update() invocations.
func loadTreeCmd() tea.Cmd {
	return func() tea.Msg {
		// loadTree shells out to tmux, so it must not run inline in Update().
		// bubbletea pumps Cmd functions on its own goroutines; the framework
		// guarantees the resulting Msg is delivered back on the main thread.
		rows := loadTree()
		// Single shared-state read covers Active + LastActive — saves a
		// tmux fork on every load tick. Cold-start fallback to the legacy
		// tmux option lives in activePaneID() (helpers.go).
		s := readSharedState()
		active := s.Active
		if active == "" {
			active = tmuxOption("@tmux_sidebar_main_pane")
		}
		return rowsLoadedMsg{rows: rows, activePaneID: active, activeWindowID: s.ActiveWindow, lastActivePaneID: s.LastActive}
	}
}

// loadSharedSnapshotCmd does a one-shot read of shared state at boot so the
// new sidebar mirrors the cursor / active / last_active / view-scroll state
// every other instance has already agreed on. The fsnotify+UDS watchers
// only fire on subsequent CHANGES — without this the new sidebar would
// show its own cursor-tracked viewport until the next peer event lands.
// Wrapped as a cursorChangedMsg (not a new Msg) so the single Update path
// applies it.
func loadSharedSnapshotCmd() tea.Cmd {
	return func() tea.Msg {
		st := readSharedState()
		return cursorChangedMsg{
			cursor:       st.Cursor,
			active:       st.Active,
			activeWindow: st.ActiveWindow,
			last:         st.LastActive,
			viewYOffset:  st.ViewYOffset,
			viewPinned:   st.ViewPinned,
		}
	}
}

// loadSharedRowsCmd reads rows the active sidebar last published to shared
// state. Hidden sidebars use this instead of loadTreeCmd to avoid the
// per-tick tmux capture-pane fork storm — a leader pattern where the active
// sidebar does the work once and writes the result for everyone.
//
// Falls back to loadTreeCmd when shared state has no rows yet (first launch,
// or every sidebar happens to be hidden — e.g. user is in a window without
// any sidebar pane). One-fork edge-case fallback, then back to leader mode
// once an active sidebar resumes publishing.
func loadSharedRowsCmd() tea.Cmd {
	return func() tea.Msg {
		s := readSharedState()
		if len(s.Rows) == 0 {
			return loadTreeCmd()()
		}
		return rowsLoadedMsg{
			rows:             s.Rows,
			activePaneID:     s.Active,
			activeWindowID:   s.ActiveWindow,
			lastActivePaneID: s.LastActive,
		}
	}
}

// tickCmd schedules a tickMsg at the default refresh interval.
func tickCmd() tea.Cmd {
	return tickCmdWithInterval(cfgRefreshSec())
}

// tickCmdWithInterval schedules a tickMsg after the given seconds.
func tickCmdWithInterval(seconds float64) tea.Cmd {
	d := time.Duration(float64(time.Second) * seconds)
	return tea.Tick(d, func(time.Time) tea.Msg { return tickMsg{} })
}

// startupBinaryMtime is captured once on first checkBinaryMtimeCmd call. The
// fsnotify watcher in startBinaryWatcherCmd is the primary install detector,
// but macOS FSEvents drops events under bursts (cp+codesign+mv inside
// ~100ms) — pre-bubbletea code worked because it polled mtime every tick.
// This Cmd restores that backstop: on any tickMsg, stat the binary; if
// mtime moved, emit binaryChangedMsg. Cheap (one stat per second).
//
// Mutex protects against torn writes from overlapping ticks: time.Time is
// 24 bytes (wall, ext, loc) and a non-atomic write from one worker while
// another reads can produce a half-updated value that compares unequal
// to itself, triggering spurious binaryChangedMsg.
var (
	startupBinaryMtime   time.Time
	startupBinaryMtimeMu sync.Mutex
)

// resetStartupBinaryMtime clears the captured mtime so the next
// checkBinaryMtimeCmd re-baselines. Called by runBubble at the top of
// every loop iteration: after a syscall.Exec failure fallback the
// previous mtime would otherwise re-trigger binaryChangedMsg in the new
// program and rebuild-loop indefinitely.
func resetStartupBinaryMtime() {
	startupBinaryMtimeMu.Lock()
	startupBinaryMtime = time.Time{}
	startupBinaryMtimeMu.Unlock()
}

func checkBinaryMtimeCmd() tea.Cmd {
	return func() tea.Msg {
		if binaryPath == "" {
			return nil
		}
		info, err := os.Stat(binaryPath)
		if err != nil {
			return nil
		}
		startupBinaryMtimeMu.Lock()
		defer startupBinaryMtimeMu.Unlock()
		if startupBinaryMtime.IsZero() {
			startupBinaryMtime = info.ModTime()
			return nil
		}
		if !info.ModTime().Equal(startupBinaryMtime) {
			return binaryChangedMsg{}
		}
		return nil
	}
}

// startContextWatcherCmd boots the fsnotify watcher once and returns its
// channel. We do this as a Cmd (not in Init() inline) so any error stays out
// of the runtime path and the watcher goroutine starts on a worker.
func startContextWatcherCmd() tea.Cmd {
	return func() tea.Msg {
		notify := make(chan struct{}, 1)
		w, err := NewContextWatcher(notify)
		if err != nil {
			fmt.Fprintf(os.Stderr, "context watcher: %v\n", err)
			return ctxWatcherReadyMsg{} // nil watcher → Update skips waitCtxCmd
		}
		// tree.go reads context via this global (existing contract). Set it
		// once here; Phase 9 may inline this if we shrink the contract.
		globalCtxWatcher.Store(w)
		return ctxWatcherReadyMsg{watcher: w, notify: notify}
	}
}

// spinTickCmd schedules the next spinTickMsg. Fast enough that the spinner
// glyph rotates roughly twice a second; cheap because the only work is a
// counter bump in Update().
func spinTickCmd() tea.Cmd {
	return tea.Tick(150*time.Millisecond, func(time.Time) tea.Msg { return spinTickMsg{} })
}

// blinkTickCmd schedules the next blinkTickMsg. 700ms matches the cadence
// of the classic tcell renderer.
func blinkTickCmd() tea.Cmd {
	return tea.Tick(700*time.Millisecond, func(time.Time) tea.Msg { return blinkTickMsg{} })
}

// standaloneFallbackCmd fires standaloneFallbackMsg once after the deadline so a
// thin client that never hears from a daemon falls back to the in-process
// engine. One-shot — not re-armed. See standaloneFallbackMsg.
func standaloneFallbackCmd() tea.Cmd {
	return tea.Tick(cfgStandaloneDeadline(), func(time.Time) tea.Msg { return standaloneFallbackMsg{} })
}

// waitCtxCmd blocks on the watcher's notify channel and emits a single
// ctxChangedMsg per signal. Caller re-issues it after handling so the watch
// loop continues for the program's lifetime.
func waitCtxCmd(notify <-chan struct{}) tea.Cmd {
	return func() tea.Msg {
		_, ok := <-notify
		if !ok {
			return nil
		}
		return ctxChangedMsg{}
	}
}

// waitControlNotifyCmd blocks on the control-mode doorbell and emits one
// controlNotifyMsg per signal. Re-issued by Update after handling so the
// loop runs for the program's lifetime. The channel is a package global
// (survives control-conn reconnects), so this never needs re-creating.
func waitControlNotifyCmd() tea.Cmd {
	return func() tea.Msg {
		<-controlNotify
		return controlNotifyMsg{}
	}
}

// startCursorWatcherCmd boots a watcher on shared-state changes and forwards
// snapshots on a channel. Two notification sources feed the same channel:
//
//  1. UDS datagram (primary, sub-millisecond) — helper processes call
//     notifyPeers after every shared-state write. See docs/ipc-uds-notify.md.
//  2. fsnotify on the state dir (fallback) — covers boot races where the
//     socket dir or peer socket isn't ready yet, and macOS kqueue's
//     occasional dropped-event behavior.
//
// Both pumps push sharedStateSnapshot{cursor,active,last} into the same out
// channel; the bubbletea Update side stays source-agnostic. Duplicate
// deliveries are absorbed by cursorChangedMsg's `changed` guard. The
// returned channel feeds waitCursorCmd in a long-lived re-arm loop.
func startCursorWatcherCmd() tea.Cmd {
	return func() tea.Msg {
		w, err := fsnotify.NewWatcher()
		if err != nil {
			fmt.Fprintf(os.Stderr, "cursor watcher: %v\n", err)
			return cursorWatcherReadyMsg{}
		}
		dir := stateDir()
		if err := w.Add(dir); err != nil {
			w.Close()
			fmt.Fprintf(os.Stderr, "cursor watcher add: %v\n", err)
			return cursorWatcherReadyMsg{}
		}

		out := make(chan sharedStateSnapshot, 1)
		target := sidebarstate.FilePath()

		// fsnotify pump (fallback).
		go func() {
			for {
				select {
				case ev, ok := <-w.Events:
					if !ok {
						return
					}
					if ev.Name != target {
						continue
					}
					// Coalesce bursts — fsnotify often fires Write+Chmod
					// pairs; one re-read per sweep is enough.
					st := readSharedState()
					select {
					case out <- sharedStateSnapshot{cursor: st.Cursor, active: st.Active, activeWindow: st.ActiveWindow, last: st.LastActive, viewYOffset: st.ViewYOffset, viewPinned: st.ViewPinned}:
					default:
					}
				case _, ok := <-w.Errors:
					if !ok {
						return
					}
				}
			}
		}()

		// UDS doorbell pump (primary). Best-effort — listenUDS returns nil
		// on bind failure (sandboxed env, unwritable stateDir, etc.) and
		// we run on fsnotify alone with no regression.
		udsCh := make(chan []byte, 4)
		if conn := sidebarstate.ListenUDS(udsCh); conn == nil {
			fmt.Fprintf(os.Stderr, "cursor watcher: UDS bind failed; running on fsnotify only\n")
		}
		go func() {
			for range udsCh {
				st := readSharedState()
				select {
				case out <- sharedStateSnapshot{cursor: st.Cursor, active: st.Active, activeWindow: st.ActiveWindow, last: st.LastActive}:
				default:
				}
			}
		}()

		return cursorWatcherReadyMsg{notify: out}
	}
}

// waitCursorCmd blocks on the cursor watcher channel and emits a single
// cursorChangedMsg per signal. Re-armed by Update() after each delivery.
func waitCursorCmd(notify <-chan sharedStateSnapshot) tea.Cmd {
	return func() tea.Msg {
		v, ok := <-notify
		if !ok {
			return nil
		}
		return cursorChangedMsg{cursor: v.cursor, active: v.active, activeWindow: v.activeWindow, last: v.last, viewYOffset: v.viewYOffset, viewPinned: v.viewPinned}
	}
}

// initialFocusCmd queries tmux once at startup to seed m.focused, since
// tmux only emits CSI focus sequences on transitions and a sidebar booting
// already-focused would otherwise stay focused=false until a focus toggle.
func initialFocusCmd() tea.Cmd {
	return func() tea.Msg {
		return initialFocusMsg{focused: sidebarHasFocus()}
	}
}

// publishRowsCmd refreshes the Rows + PaneRows + Timestamp slots of the
// shared-state file so cmdWindowName (catppuccin window-text formatter)
// can spot which panes are running / needs-input per window. Only the
// sidebar in the currently-active tmux window writes, mirroring the old
// tcell guard; otherwise every multi-window sidebar would write on every
// 1s tick and storm fsnotify peers.
func publishRowsCmd(rows []Row, paneRows []Row, isActive bool) tea.Cmd {
	return func() tea.Msg {
		if !isActive {
			return nil
		}
		withSharedStateLock(func() {
			s := readSharedState()
			s.Rows = rows
			s.PaneRows = paneRows
			writeSharedState(s)
		})
		return nil
	}
}
