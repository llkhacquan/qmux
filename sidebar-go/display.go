package main

import (
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/llkhacquan/qmux/sidebar-go/internal/sidebarstate"

	tea "github.com/charmbracelet/bubbletea"
)

// The `sidebar-go display` thin client. It owns a bubbletea program and its
// tmux pane's TTY but no engine: state arrives as daemon snapshots over the
// UDS stream at sidebarstate.DaemonSocketPath() and user actions go back as
// intents. A background connection manager keeps the socket alive (reconnect
// on EOF) so the bubbletea program is never torn down mid-session — it shows
// the last snapshot during a brief daemon gap rather than blanking.
// See plans/.../architecture-daemon-thin-clients.md §6.

func cfgDaemonBindPoll() time.Duration      { return Cfg().Timing.DaemonBindPoll.Duration }
func cfgStandaloneDeadline() time.Duration  { return Cfg().Timing.StandaloneDeadline.Duration }

// binaryStartMTime is this client's own binary mtime, sampled at boot. A thin
// client carries no fsnotify binary watcher (the burst it would cause on a
// fleet-wide `make install` is the Elastic-kill risk), so it can't notice an
// upgrade on its own. Instead it compares this baseline against the on-disk
// binary at each reconnect handshake: `make install` re-execs the daemon, which
// drops every client to EOF; on reconnect a newer on-disk mtime means this
// client is stale and re-execs onto the new image. This converges pure-logic
// changes too, so proto need only bump on real wire changes.
var binaryStartMTime time.Time

// binaryMTime returns path's mtime, or the zero time if it can't be stat'd.
func binaryMTime(path string) time.Time {
	fi, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}

// binaryUpgraded reports whether the on-disk binary is newer than the image this
// client booted from. False when either mtime is unknown — never re-exec on a
// missing stat.
func binaryUpgraded() bool {
	if binaryStartMTime.IsZero() {
		return false
	}
	cur := binaryMTime(binaryPath)
	return !cur.IsZero() && cur.After(binaryStartMTime)
}

// cmdDisplay is the `display` subcommand entrypoint. Mirrors runInteractive's
// process setup (log reopener, debug file, pprof signal) MINUS the engine —
// no control conn, no usage/git watcher, no binary watcher. main() already
// called initLogging for this subcommand, so we don't repeat it.
func cmdDisplay() {
	cleanupStateDir()
	startLogReopener(30 * time.Second)
	if os.Getenv("SIDEBAR_DEBUG") == "1" || tmuxOption("@tmux_sidebar_debug") == "1" {
		dir := stateDir()
		_ = os.MkdirAll(dir, 0o755)
		debugFile, _ = openLogFile(filepath.Join(dir, "debug.log"))
		if debugFile != nil {
			defer debugFile.Close()
		}
	}
	installPprofSignal()
	if err := runDisplay(); err != nil {
		debugLog("display: %v", err)
	}
}

// runDisplay wires the bubbletea client to a persistent connection manager
// and runs until the tmux pane closes. On a daemon proto mismatch it re-execs
// itself onto the (newer) binary on disk — the same convergence path as the
// leader's binary-watcher re-exec, but triggered by the handshake instead of
// fsnotify (clients carry no binary watcher).
func runDisplay() error {
	if p, err := os.Executable(); err == nil {
		binaryPath = p
		binaryStartMTime = binaryMTime(p) // baseline for the stale-binary re-exec check
	}
	paneID := tmuxPaneID()
	windowID := sidebarWindowID(paneID)

	// No blocking daemon pre-flight: the TUI starts immediately so the pane
	// shows the connecting spinner during cold boot instead of staying blank
	// until a daemon answers. The connManager lazy-starts + connects the daemon
	// in the background; the model swaps the spinner for cards on the first
	// snapshot. If none arrives within cfgStandaloneDeadline(), the model's
	// standalone-fallback timer quits the program and we run the in-process
	// engine here — so a pane is never stuck spinning when the daemon can't boot.

	// snapshotCh is buffered(1) and fed drop-oldest by the reader, so a slow
	// repaint never blocks the socket and the model always gets the freshest
	// state. intentCh is small + lossy for the same reason on the way out.
	snapshotCh := make(chan StateSnapshot, 1)
	intentCh := make(chan intentMsg, 8)
	stop := make(chan struct{})

	m := newTeaModel()
	m.clientMode = true
	m.snapshotCh = snapshotCh
	m.intentTx = intentCh
	m.windowID = windowID

	prog := tea.NewProgram(
		m,
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
		tea.WithReportFocus(),
	)

	mgr := &connManager{
		paneID:   paneID,
		windowID: windowID,
		snapshot: snapshotCh,
		intent:   intentCh,
		stop:     stop,
		prog:     prog,
	}
	safeGo("display.connManager", mgr.run)

	_, err := prog.Run()
	close(stop) // tear the connection manager down with the program

	if standaloneRequested {
		standaloneRequested = false
		debugLog("display: no daemon snapshot within %s; standalone fallback", cfgStandaloneDeadline())
		return runStandalone()
	}
	if reExecRequested {
		reExecRequested = false
		execErr := syscall.Exec(binaryPath, os.Args, os.Environ())
		debugLog("display: re-exec failed (%v); exiting", execErr)
	}
	return err
}

// sidebarWindowID resolves the tmux window_id for the client's own pane. One
// fork at boot; the value is stable for the pane's lifetime so we never poll
// it again (windowActive then derives from snapshots).
func sidebarWindowID(paneID string) string {
	if paneID == "" {
		return ""
	}
	out, err := tmuxQuery("display-message", "-p", "-t", paneID, "#{window_id}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// connManager keeps a live daemon connection behind the persistent snapshot/
// intent channels. The bubbletea program holds those channels for its whole
// life; this goroutine swaps the underlying socket on reconnect transparently.
type connManager struct {
	paneID   string
	windowID string
	snapshot chan StateSnapshot
	intent   chan intentMsg
	stop     chan struct{}
	prog     *tea.Program
}

func (mgr *connManager) run() {
	// spawned gates the lazy daemon start to once per disconnect episode. Re-
	// spawning on every failed poll would double-fork serve during the ~0.3s
	// gap between the fork and its child grabbing the flock (each fork is a
	// wasted EDR exec scan). Reset to false after a successful connect so the
	// next disconnect (mid-session daemon crash) re-arms the respawn.
	spawned := false
	for {
		select {
		case <-mgr.stop:
			return
		default:
		}

		conn, err := net.Dial("unix", sidebarstate.DaemonSocketPath())
		if err != nil {
			// No daemon listening: lazy-start one (flock-gated, herd-safe) the
			// first time, then poll tightly for its socket to bind. The daemon
			// binds ~0.3s after fork; a 25ms poll connects right as it comes up
			// instead of overshooting a doubling backoff. Dials are pure UDS
			// connects (no fork/EDR), so the tight interval is free. If the
			// daemon never binds, runDisplay's standalone-fallback deadline
			// takes over — this loop just keeps the spinner's data path warm.
			if !spawned {
				tryStartDaemon()
				spawned = true
			}
			debugLog("display: dial: %v", err)
			if !mgr.sleep(cfgDaemonBindPoll()) {
				return
			}
			continue
		}
		spawned = false // connected — re-arm respawn for the next episode

		reexec := mgr.serve(conn)
		conn.Close()
		if reexec {
			// Proto mismatch or a newer on-disk binary: re-exec onto the new
			// image. prog.Quit unwinds bubbletea so runDisplay's syscall.Exec
			// runs with the terminal restored.
			reExecRequested = true
			mgr.prog.Quit()
			return
		}

		select {
		case <-mgr.stop:
			return
		default:
		}
		// Daemon dropped us (it re-execs onto a new image on upgrade and
		// rebinds in well under a second). Poll back tightly rather than
		// stalling on a coarse backoff.
		if !mgr.sleep(cfgDaemonBindPoll()) {
			return
		}
	}
}

// serve runs one connection: handshake, then pump snapshots in and intents
// out until EOF or stop. Returns true when the caller should re-exec onto the
// on-disk binary — either a proto mismatch (daemon speaks a newer wire) or a
// stale-binary upgrade detected at the handshake.
func (mgr *connManager) serve(conn net.Conn) (reexec bool) {
	enc := json.NewEncoder(conn)
	dec := json.NewDecoder(conn)

	if err := writeMsg(enc, msgHello, helloMsg{
		Proto:    protoVersion,
		PID:      os.Getpid(),
		PaneID:   mgr.paneID,
		WindowID: mgr.windowID,
	}); err != nil {
		debugLog("display: hello: %v", err)
		return false
	}

	env, err := readEnvelope(dec)
	if err != nil || env.T != msgWelcome {
		debugLog("display: welcome read: %v", err)
		return false
	}
	var w welcomeMsg
	_ = decodePayload(env, &w)
	if !w.Ok {
		debugLog("display: proto mismatch client=%d daemon=%d", protoVersion, w.Proto)
		return true
	}

	// Stale-binary check: the daemon just re-execed onto a newer image (that's
	// why we reconnected), so a newer on-disk binary means this client is
	// behind. Re-exec onto it via the same path as a proto mismatch.
	if binaryUpgraded() {
		debugLog("display: binary upgraded on disk; re-exec onto new image")
		return true
	}

	// Writer: drain intents to the daemon until the conn dies or stop fires.
	// A write error closes connDone, which also unblocks the reader below.
	connDone := make(chan struct{})
	var closeOnce sync.Once
	closeDone := func() { closeOnce.Do(func() { close(connDone) }) }
	safeGo("display.writer", func() {
		for {
			select {
			case in := <-mgr.intent:
				if err := writeMsg(enc, msgIntent, in); err != nil {
					closeDone()
					return
				}
			case <-connDone:
				return
			case <-mgr.stop:
				return
			}
		}
	})

	// Reader: decode frames, fan snapshots to the model (drop-oldest). Runs on
	// this goroutine so its return ends the connection.
	for {
		env, err := readEnvelope(dec)
		if err != nil {
			closeDone()
			return false
		}
		switch env.T {
		case msgSnapshot:
			var snap StateSnapshot
			if decodePayload(env, &snap) == nil {
				pushSnapshot(mgr.snapshot, snap)
			}
		case msgReexec:
			// Advisory — EOF will follow and the reconnect handles it.
		}
	}
}

// sleep waits d or until stop. Returns false if stop fired (caller should
// exit its loop).
func (mgr *connManager) sleep(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-mgr.stop:
		return false
	}
}

// pushSnapshot does a drop-oldest enqueue so a slow repaint never stalls the
// reader: the model only ever cares about the newest state.
func pushSnapshot(ch chan StateSnapshot, s StateSnapshot) {
	select {
	case ch <- s:
	default:
		select {
		case <-ch:
		default:
		}
		select {
		case ch <- s:
		default:
		}
	}
}

// tryStartDaemon becomes the one client that boots the daemon, herd-safely.
// Taking the daemon flock non-blocking: losing means a daemon already owns it
// (running or just starting), so we do nothing and let the caller keep dialing.
// Winning means none exists — we fork a detached `serve` and release the lock so
// that child can re-acquire it for its lifetime. With N clients all failing to
// dial, the flock lets only ~one fork a serve; serve's own flock is the backstop
// if two race the tiny release window (the loser exits 0).
func tryStartDaemon() {
	lf, ok := acquireDaemonLock()
	if !ok {
		return
	}
	lf.Close()
	spawnDaemonHook()
}

// spawnDaemonHook is the daemon spawn, overridable in tests so they exercise the
// flock gating without forking a real process.
var spawnDaemonHook = spawnServe

// spawnServe forks a `sidebar-go serve` that outlives this client. Setpgid (own
// process group, NOT a new session) keeps terminal job-control signals (Ctrl-C
// in the launching pane) off it. We deliberately do NOT setsid: Elastic EDR
// SIGKILLs adhoc-signed binaries that setsid into a new session (it reads as
// daemon/malware behavior), and a plain in-session spawn is tolerated — verified
// empirically. The daemon ignores SIGHUP (cmdServe) so it still survives the
// launching pane closing despite staying in-session. stdio → /dev/null keeps it
// off the pane's alt-screen.
func spawnServe() {
	cmd := exec.Command(binaryPath, "serve")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0); err == nil {
		cmd.Stdin, cmd.Stdout, cmd.Stderr = devNull, devNull, devNull
		defer devNull.Close()
	}
	cmd.Env = os.Environ()
	if err := cmd.Start(); err != nil {
		debugLog("display: spawn serve: %v", err)
		return
	}
	_ = cmd.Process.Release()
}

// runStandalone runs the full in-process engine (control conn + git/usage
// watchers + loadTree) on bubbletea — today's pre-daemon behavior. The fallback
// when no daemon can be reached or started, so a display pane is never
// permanently blank. cmdDisplay already did the process setup (logging, debug,
// pprof); this only starts the engine I/O that runInteractive's tail does.
func runStandalone() error {
	startControlConn()
	startUsageRefresh(30 * time.Second)
	return runBubble()
}

// waitSnapshotCmd blocks on the daemon snapshot channel and emits one
// snapshotMsg per delivery. Re-armed by Update after each apply. Returns nil
// (no re-arm) only if the channel is closed, which the client never does
// while running — the connection manager keeps it open across reconnects.
func waitSnapshotCmd(ch <-chan StateSnapshot) tea.Cmd {
	return func() tea.Msg {
		s, ok := <-ch
		if !ok {
			return nil
		}
		return snapshotMsg{snap: s}
	}
}
