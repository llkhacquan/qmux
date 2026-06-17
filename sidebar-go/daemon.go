package main

import (
	"encoding/json"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/llkhacquan/qmux/sidebar-go/internal/sidebarstate"
)

// The `sidebar-go serve` daemon: ONE engine (loadTree + tmux control conn +
// git watcher + usage scan) feeding many thin `sidebar-go display` clients over
// the UDS stream at sidebarstate.DaemonSocketPath(). Replaces the
// leader-election multi-process model — the daemon is the permanent leader and
// pushes canonical state instead of every client reading the shared-state file.
// See plans/.../architecture-daemon-thin-clients.md.

// snapshotDebounce coalesces a burst of state changes (loadTree completion +
// a control notification + an intent landing within milliseconds) into a
// single client push.
const snapshotDebounce = 50 * time.Millisecond

type daemon struct {
	mu      sync.Mutex
	clients map[*daemonClient]struct{}
	dirty   chan struct{} // any state change → (debounced) broadcast
	reload  chan struct{} // request an immediate loadTree

}

// daemonClient is one connected display. The writer goroutine drains send;
// the hub enqueues pre-marshaled snapshot frames (drop-oldest when the client
// is slow so one stuck display never stalls the others or the hub).
type daemonClient struct {
	send     chan []byte
	done     chan struct{}
	doneOnce sync.Once
}

func (c *daemonClient) close() { c.doneOnce.Do(func() { close(c.done) }) }

func newDaemon() *daemon {
	return &daemon{
		clients: make(map[*daemonClient]struct{}),
		dirty:   make(chan struct{}, 1),
		reload:  make(chan struct{}, 1),
	}
}

// cmdServe is the `serve` subcommand entrypoint. flock elects a single daemon:
// a loser (another daemon already running) exits 0 silently so the lazy-start
// path is a safe no-op race.
func cmdServe() {
	lf, ok := acquireDaemonLock()
	if !ok {
		return
	}
	defer lf.Close()

	if p, err := os.Executable(); err == nil {
		binaryPath = p // upgrade watcher (§7) re-execs into this on `make install`
	}

	// The daemon is spawned in-session (NOT via setsid — Elastic EDR SIGKILLs
	// adhoc-signed binaries that setsid into a new session; verified). Ignoring
	// SIGHUP is what then lets it outlive the launching display pane closing: a
	// pane teardown SIGHUPs its session, and we must not die with it. SIG_IGN
	// also survives the upgrade syscall.Exec, so the re-execed image stays
	// hangup-proof too.
	signal.Ignore(syscall.SIGHUP)

	cleanupStateDir()
	startLogReopener(30 * time.Second)
	if os.Getenv("SIDEBAR_DEBUG") == "1" || tmuxOption("@tmux_sidebar_debug") == "1" {
		dir := stateDir()
		_ = os.MkdirAll(dir, 0o755)
		debugFile, _ = openLogFile(dir + "/debug.log")
	}
	installPprofSignal()

	sock := sidebarstate.DaemonSocketPath()
	_ = os.MkdirAll(sidebarstate.Dir(), 0o755)
	// Stale socket from a crash with no listener would refuse Accept.
	_ = os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		debugLog("serve: listen %s: %v", sock, err)
		os.Exit(1)
	}
	defer ln.Close()
	defer os.Remove(sock)

	d := newDaemon()
	d.start()
	d.acceptLoop(ln)
}

// acquireDaemonLock takes a non-blocking exclusive flock on DaemonLockPath.
// Returns (file, true) when held — the file MUST stay open for the daemon's
// lifetime. (nil, false) means another daemon owns it.
func acquireDaemonLock() (*os.File, bool) {
	_ = os.MkdirAll(sidebarstate.Dir(), 0o755)
	lf, err := os.OpenFile(sidebarstate.DaemonLockPath(), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, false
	}
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lf.Close()
		return nil, false
	}
	return lf, true
}

// start boots the engine + broadcaster goroutines. The engine owns all I/O
// (control conn, git/usage/context watchers); the broadcaster fans debounced
// snapshots out to clients.
func (d *daemon) start() {
	safeGo("daemon.broadcaster", d.broadcaster)
	safeGo("daemon.engine", d.engine)
	safeGo("daemon.binaryWatch", d.watchBinaryUpgrade)
}

// watchBinaryUpgrade blocks on an fsnotify watch of the daemon's own binary and
// re-execs into the new image the moment `make install` swaps it. This is the
// SINGLE re-exec on upgrade: clients merely see EOF and reconnect (§7), so the
// fleet no longer re-execs in a burst — the Elastic-kill mitigation. Restarted
// after a failed exec so a later install retry is still caught.
func (d *daemon) watchBinaryUpgrade() {
	if !awaitBinaryChange(binaryPath) {
		return
	}
	d.reexec()
}

// reexec replaces the daemon image with the freshly-installed binary. Clients
// get an advisory reexec frame (they reconnect on the EOF that follows anyway),
// then syscall.Exec swaps the image in place — same PID, so the flock fd
// (O_CLOEXEC) releases on exec and the new cmdServe re-acquires it. The stale
// socket file is unlinked + rebound by the new image's listen path. On exec
// failure the daemon stays up (clients keep their connections) and re-arms the
// watcher for the next install attempt.
func (d *daemon) reexec() {
	debugLog("serve: binary changed; re-exec into new image")
	d.broadcastReexec()
	time.Sleep(50 * time.Millisecond) // let writers flush the advisory frame
	// Tear down the control conn so the `tmux -C` child is reaped before we
	// swap our image — same-PID exec inherits the pipe fds but the new engine
	// spawns its own conn, so without this the old child orphans and keeps a
	// server pipe open (single-threaded server => hang risk). Mirrors
	// runBubble's pre-exec teardown. Re-execed image's engine() redials.
	stopControlConn()
	if err := syscall.Exec(binaryPath, os.Args, os.Environ()); err != nil {
		debugLog("serve: re-exec failed: %v; staying up", err)
		startControlConn() // exec failed — restore the conn we just tore down
		safeGo("daemon.binaryWatch", d.watchBinaryUpgrade)
	}
}

// broadcastReexec fans an advisory reexec frame to every client. Best-effort:
// clients reconnect on the EOF that follows the image swap regardless.
func (d *daemon) broadcastReexec() {
	b, err := json.Marshal(Envelope{T: msgReexec})
	if err != nil {
		return
	}
	frame := append(b, '\n')
	d.mu.Lock()
	for c := range d.clients {
		enqueue(c.send, frame)
	}
	d.mu.Unlock()
}

// engine is the single owner of loadTree and its trigger sources. Every path
// that changes the pane tree (control notification, context change, periodic
// safety tick, an explicit reload intent) funnels through reloadTree; an
// out-of-process shared-state write (focus hook, lean helper) only triggers a
// rebroadcast since the daemon didn't author it.
func (d *daemon) engine() {
	startControlConn()
	if gw, err := newGitDirtyWatcher(); err == nil {
		globalGitWatcher.Store(gw)
	} else {
		debugLog("serve: git watcher: %v", err)
	}
	startUsageRefresh(30 * time.Second)

	ctxNotify := make(chan struct{}, 1)
	if w, err := NewContextWatcher(ctxNotify); err == nil {
		globalCtxWatcher.Store(w)
	} else {
		debugLog("serve: context watcher: %v", err)
	}

	// Out-of-process writers (sidebar-go on-focus, sidebar-notify) still mutate
	// the shared-state file during migration. Watch it so a hook's cursor/active/
	// done change rebroadcasts to clients. Our own writes also fire this — the
	// debounce + no-reload path makes that a cheap extra broadcast, not a loop.
	fileCh := make(chan struct{}, 1)
	d.watchSharedState(fileCh)

	// Fork-free focus tracking: the after-select-pane hook ships "pane|window"
	// to focus.sock via socat instead of forking the heavy binary. We own the
	// tmux control conn, so we can do the tracking work here. Falls back to the
	// forked `sidebar-go on-focus` (the hook's own fallback) when this bind fails.
	focusCh := make(chan []byte, 8)
	if c := sidebarstate.ListenUnixgramAt(sidebarstate.FocusSocketPath(), focusCh); c == nil {
		debugLog("serve: focus socket bind failed; on-focus falls back to fork")
	}
	safeGo("daemon.focus", func() {
		for msg := range focusCh {
			d.handleFocusMsg(msg)
		}
	})

	// Watch config file for live reload
	cfgCh := make(chan struct{}, 1)
	WatchConfigFile(cfgCh)

	interval := time.Duration(float64(time.Second) * cfgRefreshSec())
	tick := time.NewTicker(interval)
	defer tick.Stop()

	healthTick := time.NewTicker(10 * time.Second)
	defer healthTick.Stop()
	reapTick := time.NewTicker(5 * time.Minute)
	defer reapTick.Stop()
	engineStart := time.Now()

	// Reap orphan context files on startup
	if n := reapOrphanContextFiles(); n > 0 {
		debugLog("engine: reaped %d orphan context files on startup", n)
	}

	seedHiddenSessions() // persisted hidden set → shared state before first snapshot
	d.reloadTree()       // initial

	for {
		select {
		case <-tick.C:
			d.reloadTree()
		case <-controlNotify:
			d.reloadTree()
		case <-ctxNotify:
			d.reloadTree()
		case <-d.reload:
			d.reloadTree()
		case <-fileCh:
			d.markDirty()
		case <-cfgCh:
			ReloadConfig()
			debugLog("engine: config reloaded")
			d.reloadTree()
		case <-reapTick.C:
			if n := reapOrphanContextFiles(); n > 0 {
				debugLog("engine: reaped %d orphan context files", n)
			}
		case <-healthTick.C:
			if time.Since(engineStart) > 5*time.Second && globalCtxWatcher.Load() == nil {
				debugLog("health: context watcher nil after %s uptime, retrying", time.Since(engineStart).Round(time.Second))
				if w, err := NewContextWatcher(ctxNotify); err == nil {
					globalCtxWatcher.Store(w)
					debugLog("health: context watcher recovered")
					d.reloadTree()
				} else {
					debugLog("health: context watcher retry failed: %v", err)
				}
			}
		}
	}
}

// watchSharedState forwards shared-state-file changes (UDS doorbell primary,
// fsnotify fallback) onto out. Mirrors startCursorWatcherCmd's dual-pump but
// collapsed to a bare signal — the daemon re-reads the whole file on broadcast.
func (d *daemon) watchSharedState(out chan<- struct{}) {
	ping := func() {
		select {
		case out <- struct{}{}:
		default:
		}
	}
	udsCh := make(chan []byte, 4)
	if conn := sidebarstate.ListenUDS(udsCh); conn == nil {
		debugLog("serve: UDS doorbell bind failed; fsnotify only")
	}
	safeGo("daemon.sharedStateUDS", func() {
		for range udsCh {
			ping()
		}
	})
	// fsnotify fallback handled by the existing cursor-watcher infra would pull
	// in bubbletea Cmds; the doorbell covers every in-tree writer (they all go
	// through writeSharedState → NotifyPeers), so the daemon relies on it plus
	// the 1s safety tick rather than a second fsnotify watcher here.
}

// reloadTree runs the one true loadTree (which also persists done/running
// timestamps), publishes the fresh rows to shared state so out-of-process
// consumers like cmdWindowName keep working, then marks the snapshot dirty.
func (d *daemon) reloadTree() {
	// Authoritative width enforcement: re-pin every sidebar to the configured
	// width. tmux reflows panes on client resize / session switch without
	// emitting %layout-change to our control conn, so this reload loop (1s tick
	// + session/window-change notifications) is the only timing-robust place to
	// correct drift. Fork-free over the control conn; no-op when nothing drifted.
	syncAllSidebarWidths()

	rows := loadTree()
	paneRows := paneRowsFor(rows)
	scrolloff := configuredScrolloff()
	withSharedStateLock(func() {
		s := readSharedState()
		s.Rows = rows
		s.PaneRows = paneRows
		s.Scrolloff = scrolloff
		// Self-heal stale ActiveWindow. Authoritative source is the real
		// attached terminal's current window — focus-hook races (after-new-
		// window's ensure re-selects panes; a hook firing in a control-mode
		// client's context) can leave ActiveWindow on a window the user isn't
		// viewing. The per-session window_active set (lastActiveWindows) holds
		// one window per session and so can't distinguish a stale-but-still-
		// session-active ActiveWindow from the right one; only the terminal's
		// own current window can. Fall back to the membership check when no
		// real terminal is attached (all-detached / control-only).
		if tw := attachedTerminalWindow(); tw != "" {
			if s.ActiveWindow != tw {
				debugLog("reloadTree: correcting ActiveWindow %s -> %s (attached terminal)", s.ActiveWindow, tw)
				s.ActiveWindow = tw
			}
		} else if aw := lastActiveWindows; len(aw) > 0 && !aw[s.ActiveWindow] {
			for w := range aw {
				debugLog("reloadTree: correcting ActiveWindow %s -> %s (membership fallback)", s.ActiveWindow, w)
				s.ActiveWindow = w
				break
			}
		}
		// Self-heal stale Active pane. When ActiveWindow was corrected (or
		// a focus-hook race left Active pointing at a pane in a different
		// window), the trackFocus same-window early-return prevents the
		// focus hook from fixing Active. Detect the mismatch and resolve it.
		if s.Active != "" && s.ActiveWindow != "" {
			activeInWindow := false
			for _, r := range rows {
				if r.PaneID == s.Active && r.Window == s.ActiveWindow {
					activeInWindow = true
					break
				}
			}
			if !activeInWindow {
				if mp := claudePaneInWindow(rows, s.ActiveWindow); mp != "" {
					debugLog("reloadTree: correcting Active %s -> %s (not in window %s)", s.Active, mp, s.ActiveWindow)
					if s.Active != mp {
						s.LastActive = s.Active
					}
					s.Active = mp
					s.Cursor = mp
				}
			}
		}
		writeSharedState(s)
	})
	d.markDirty()
}

// handleFocusMsg applies a focus update delivered over focus.sock (socat from
// the after-select-pane hook, payload "pane|window"). Runs the SAME tracking
// path as the forked `on-focus` subcommand — trackFocus is shared code — then
// rebroadcasts. We ignore trackFocus's runEnsure return: the daemon never
// splits a sidebar pane (that stays on the forked after-select-window/
// client-session-changed path), it only records active-pane state.
//
// syncSidebarWidth restores the focused window's sidebar to the configured
// width — the forked cmdOnFocus used to do this, and dropping it on the
// fork-free migration is what let widths drift across windows. It rides the
// daemon's control conn (no fork) and only resizes when the width differs.
func (d *daemon) handleFocusMsg(msg []byte) {
	pane, window, ok := strings.Cut(string(msg), "|")
	if !ok || pane == "" {
		return
	}
	trackFocus(pane, window)
	syncSidebarWidth(window)
	d.markDirty()
}

func (d *daemon) markDirty() {
	select {
	case d.dirty <- struct{}{}:
	default:
	}
}

func (d *daemon) requestReload() {
	select {
	case d.reload <- struct{}{}:
	default:
	}
}

// broadcaster debounces dirty signals and pushes one snapshot per quiet window.
func (d *daemon) broadcaster() {
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	pending := false
	for {
		select {
		case <-d.dirty:
			if !pending {
				pending = true
				timer.Reset(snapshotDebounce)
			}
		case <-timer.C:
			pending = false
			d.broadcastNow()
		}
	}
}

// broadcastNow marshals the current canonical state once and fans the bytes
// out to every client. Drop-oldest on a full per-client queue: a slow display
// gets the newest snapshot, never blocks the hub.
//
// No identical-frame dedup: the 1s tick pushes a frame every second even when
// nothing moved. That heartbeat is cheap (one small marshal + one tiny socket
// write per client) because clients gate the expensive repaint on visibility —
// a hidden display stores the state and skips refreshContent. The unconditional
// emit also self-heals any client-local render drift within ~1s.
func (d *daemon) broadcastNow() {
	frame, err := encodeSnapshot()
	if err != nil {
		debugLog("serve: marshal snapshot: %v", err)
		return
	}
	d.mu.Lock()
	for c := range d.clients {
		enqueue(c.send, frame)
	}
	d.mu.Unlock()
}

// encodeSnapshot builds the wire frame (envelope + newline) for the current
// shared state + usage.
func encodeSnapshot() ([]byte, error) {
	snap := StateSnapshot{sharedState: readSharedState(), Usage: getUsage()}
	d, err := json.Marshal(snap)
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(Envelope{T: msgSnapshot, D: d})
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// enqueue does a drop-oldest push so a stuck client can't stall the hub.
func enqueue(ch chan []byte, b []byte) {
	select {
	case ch <- b:
	default:
		select {
		case <-ch:
		default:
		}
		select {
		case ch <- b:
		default:
		}
	}
}

func (d *daemon) addClient(c *daemonClient) {
	d.mu.Lock()
	d.clients[c] = struct{}{}
	d.mu.Unlock()
}

func (d *daemon) removeClient(c *daemonClient) {
	d.mu.Lock()
	delete(d.clients, c)
	d.mu.Unlock()
}

// acceptLoop serves connections until the listener closes.
func (d *daemon) acceptLoop(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			debugLog("serve: accept: %v", err)
			return
		}
		safeGo("daemon.conn", func() { d.handleConn(conn) })
	}
}

// handleConn runs the per-client protocol: handshake, register, initial
// snapshot, then read intents until EOF. A proto mismatch is acked with
// Ok:false and the conn dropped — the client re-execs or falls back.
func (d *daemon) handleConn(conn net.Conn) {
	defer conn.Close()
	dec := json.NewDecoder(conn)
	enc := json.NewEncoder(conn)

	env, err := readEnvelope(dec)
	if err != nil || env.T != msgHello {
		return
	}
	var h helloMsg
	_ = decodePayload(env, &h)
	ok := h.Proto == protoVersion
	_ = writeMsg(enc, msgWelcome, welcomeMsg{Proto: protoVersion, Ok: ok})
	if !ok {
		debugLog("serve: proto mismatch client=%d daemon=%d", h.Proto, protoVersion)
		return
	}

	c := &daemonClient{send: make(chan []byte, 4), done: make(chan struct{})}
	d.addClient(c)
	defer d.removeClient(c)

	// Initial snapshot so the client renders immediately, not on next change.
	if frame, err := encodeSnapshot(); err == nil {
		enqueue(c.send, frame)
	}

	safeGo("daemon.connWriter", func() {
		for {
			select {
			case b := <-c.send:
				if _, err := conn.Write(b); err != nil {
					c.close()
					return
				}
			case <-c.done:
				return
			}
		}
	})

	for {
		env, err := readEnvelope(dec)
		if err != nil {
			c.close()
			return
		}
		switch env.T {
		case msgIntent:
			var in intentMsg
			if decodePayload(env, &in) == nil {
				d.applyIntent(in)
			}
		case msgBye:
			c.close()
			return
		}
	}
}

// applyIntent mutates canonical state in response to a user action, then marks
// the snapshot dirty so every client (including the originator) converges on
// the daemon's view. Tmux side-effects route here because the daemon owns the
// only control connection.
func (d *daemon) applyIntent(in intentMsg) {
	switch in.Action {
	case actionCursor:
		s := readSharedState()
		writeSharedCursorActive(in.PaneID, s.Active, "")
	case actionScroll:
		withSharedStateLock(func() {
			s := readSharedState()
			if s.ViewYOffset == in.YOffset && s.ViewPinned == in.Pinned {
				return
			}
			s.ViewYOffset = in.YOffset
			s.ViewPinned = in.Pinned
			writeSharedState(s)
		})
	case actionClearDone:
		clearSharedDone(in.PaneID)
	case actionToggleHidden:
		if in.Session == "" {
			return
		}
		withSharedStateLock(func() {
			s := readSharedState()
			set := hiddenSliceToSet(s.HiddenSessions)
			if set[in.Session] {
				delete(set, in.Session)
			} else {
				set[in.Session] = true
			}
			s.HiddenSessions = sortedHiddenSlice(set)
			writeSharedState(s)
			writeHiddenOption(s.HiddenSessions) // persist for next boot
		})
	case actionReload:
		d.requestReload()
		return // reloadTree marks dirty itself
	}
	d.markDirty()
}
