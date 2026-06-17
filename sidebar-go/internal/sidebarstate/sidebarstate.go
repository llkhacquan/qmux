// Package sidebarstate is the shared transport layer used by every sidebar-*
// binary in this module. It owns three things and nothing else:
//
//   - Where the shared-state JSON lives on disk.
//   - flock-serialized, atomic read/write of that file.
//   - The UDS datagram doorbell — bind on <pid>.sock, fan out to peers.
//
// Higher-level concerns (the typed Row schema, sharedState fields beyond
// cursor/active/last_active/ts) stay in the sidebar-go binary. Lean helpers
// like sidebar-notify reach in via WriteRaw + WithLock and treat the rest of
// the document as opaque JSON to preserve cross-version field forwarding.
//
// See tmux/sidebar-go/docs/ipc-uds-notify.md for the design rationale.
package sidebarstate

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Dir resolves the shared state directory. Order: TMUX_SIDEBAR_STATE_DIR >
// $XDG_STATE_HOME/tmux-sidebar > ~/.local/state/tmux-sidebar. Both binaries
// MUST agree on this — divergence silently splits the doorbell mesh.
func Dir() string {
	if d := os.Getenv("TMUX_SIDEBAR_STATE_DIR"); d != "" {
		return d
	}
	if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
		return filepath.Join(xdg, "tmux-sidebar")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "tmux-sidebar")
}

// FilePath returns the shared-state JSON path.
func FilePath() string { return filepath.Join(Dir(), "shared-state") }

func lockPath() string { return filepath.Join(Dir(), "shared-state.lock") }

// DaemonSocketPath is the UDS stream socket the `sidebar-go serve` daemon
// binds and `sidebar-go display` clients dial. Distinct from the per-pid
// datagram doorbell sockets under SocketDir().
func DaemonSocketPath() string { return filepath.Join(Dir(), "daemon.sock") }

// DaemonLockPath is the flock file that elects a single daemon. The first
// display client to acquire it lazy-starts the daemon; the rest just dial.
func DaemonLockPath() string { return filepath.Join(Dir(), "daemon.lock") }

// ReadRaw returns the shared-state document as a byte slice. Missing/empty
// returns (nil, err) — callers typically fall through to a zero state.
func ReadRaw() ([]byte, error) { return os.ReadFile(FilePath()) }

// WriteRaw atomically writes data to the shared-state file then fires a UDS
// doorbell so every peer listener wakes faster than fsnotify can settle.
// Callers doing read-modify-write must hold WithLock; this function makes no
// concurrency guarantee on its own. MkdirAll is cheap and idempotent — first
// run on a fresh machine (or after the user nukes the state dir) would
// otherwise silently lose every update with ENOENT.
func WriteRaw(data []byte) error {
	if err := os.MkdirAll(Dir(), 0o755); err != nil {
		return err
	}
	p := FilePath()
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		return err
	}
	NotifyPeers(Doorbell)
	return nil
}

// WithLock serializes RMW of the shared-state file across every sidebar
// process AND every goroutine within one process. Best-effort: a failure to
// open the lock file falls through to fn() unlocked rather than blocking
// indefinitely or silently dropping the update. MkdirAll guarantees the
// open won't ENOENT on a fresh machine — without it, two helpers racing
// on first boot could both proceed unlocked.
func WithLock(fn func()) {
	_ = os.MkdirAll(Dir(), 0o755)
	lf, err := os.OpenFile(lockPath(), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		fn()
		return
	}
	defer lf.Close()
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX); err == nil {
		defer syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)
	}
	fn()
}

// PatchCursorActive merges cursor/active/active_window/last_active/ts into a
// JSON object, preserving every other field as-is. Used by lean helpers that
// don't link the full Row type. Returns ok=false if the input isn't a JSON
// object — caller decides whether to overwrite or bail.
//
// active_window is the tmux window_id of the focused pane. The sidebar uses it
// to mark a Claude card whose window holds focus even when the focused pane
// itself is a non-Claude console — that card isn't `active` but shares the
// live window. An empty activeWindow leaves the stored value untouched so a
// caller without the window id (legacy hook) doesn't clobber it.
//
// The LastActive promotion mirrors writeSharedCursorActive in sidebar-go's
// helpers.go: when active genuinely transitions (old != new and old
// non-empty), the prior active becomes last_active.
func PatchCursorActive(doc []byte, cursor, active, activeWindow string) (out []byte, changed bool, ok bool) {
	m := map[string]json.RawMessage{}
	if len(doc) > 0 {
		if err := json.Unmarshal(doc, &m); err != nil {
			return nil, false, false
		}
	}

	curBytes, _ := json.Marshal(cursor)
	actBytes, _ := json.Marshal(active)

	prevCur, _ := unmarshalString(m["cursor"])
	prevAct, _ := unmarshalString(m["active"])
	prevWin, _ := unmarshalString(m["active_window"])
	if prevCur == cursor && prevAct == active && (activeWindow == "" || prevWin == activeWindow) {
		return doc, false, true
	}

	if active != prevAct && prevAct != "" {
		prevActRaw, _ := json.Marshal(prevAct)
		m["last_active"] = prevActRaw
	}
	m["cursor"] = curBytes
	m["active"] = actBytes
	if activeWindow != "" {
		winBytes, _ := json.Marshal(activeWindow)
		m["active_window"] = winBytes
	}

	tsBytes, _ := json.Marshal(time.Now().UnixMilli())
	m["ts"] = tsBytes

	merged, err := json.Marshal(m)
	if err != nil {
		return nil, false, false
	}
	return merged, true, true
}

func unmarshalString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

// ---------- UDS doorbell ----------

// SocketDir holds one <pid>.sock per running bubbletea sidebar.
func SocketDir() string { return filepath.Join(Dir(), "sock") }

// SocketPathForPid returns the UDS path a sidebar with the given pid binds.
func SocketPathForPid(pid int) string {
	return filepath.Join(SocketDir(), strconv.Itoa(pid)+".sock")
}

// FocusSocketPath is the daemon's fixed unixgram socket for focus events
// (after-select-pane → "pane|window"). Lives in Dir(), NOT SocketDir(): keeping
// it out of the per-pid sock dir means NotifyPeers never fans a wake-byte at it
// (it scans SocketDir only), so the focus listener never sees a malformed
// payload. A fixed path is what lets a shell hook address the daemon without
// knowing its pid.
func FocusSocketPath() string { return filepath.Join(Dir(), "focus.sock") }

// Doorbell is the payload NotifyPeers sends. Single byte today; first byte
// reserved for future event-type routing (focus/exit/claim/...).
var Doorbell = []byte{0}

// ListenUDS binds <self-pid>.sock and forwards received datagrams onto out.
// Best-effort: returns nil on failure, in which case the caller runs on
// fsnotify alone — same behavior as before this layer existed.
func ListenUDS(out chan<- []byte) *net.UnixConn {
	return ListenUnixgramAt(SocketPathForPid(os.Getpid()), out)
}

// ListenUnixgramAt binds a unixgram socket at path and forwards received
// datagrams onto out (drop-oldest). Best-effort: returns nil on bind failure.
// Shared by ListenUDS (per-pid doorbell) and the daemon's fixed focus.sock.
func ListenUnixgramAt(path string, out chan<- []byte) *net.UnixConn {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil
	}
	// Stale socket from a prior crash would block bind.
	_ = os.Remove(path)
	addr := &net.UnixAddr{Name: path, Net: "unixgram"}
	conn, err := net.ListenUnixgram("unixgram", addr)
	if err != nil {
		return nil
	}
	go func() {
		defer conn.Close()
		defer os.Remove(path)
		buf := make([]byte, 4096)
		for {
			n, _, err := conn.ReadFromUnix(buf)
			if err != nil {
				if errors.Is(err, net.ErrClosed) {
					return
				}
				continue
			}
			msg := make([]byte, n)
			copy(msg, buf[:n])
			// Drop on full: the listener already has a pending wake-up
			// queued and re-reads shared state once anyway. Coalescing
			// avoids storming Update() with duplicate doorbells.
			select {
			case out <- msg:
			default:
			}
		}
	}()
	return conn
}

// NotifyPeers fans out a single datagram to every <pid>.sock in SocketDir,
// skipping our own and unlinking sockets whose owner is gone. Never blocks
// long: dial errors and a 20ms write deadline are the only synchronous cost.
func NotifyPeers(event []byte) {
	entries, err := os.ReadDir(SocketDir())
	if err != nil {
		return
	}
	myPid := os.Getpid()
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".sock") {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSuffix(name, ".sock"))
		if err != nil {
			continue
		}
		if pid == myPid {
			continue
		}
		path := filepath.Join(SocketDir(), name)
		// Liveness probe: signal 0 returns ESRCH if pid is gone. Lets us
		// reap stale sockets eagerly instead of relying on the kernel
		// returning ECONNREFUSED on dial (which only fires for some
		// failure modes).
		if proc, perr := os.FindProcess(pid); perr == nil {
			if serr := proc.Signal(syscall.Signal(0)); serr != nil {
				_ = os.Remove(path)
				continue
			}
		}
		addr := &net.UnixAddr{Name: path, Net: "unixgram"}
		conn, derr := net.DialUnix("unixgram", nil, addr)
		if derr != nil {
			_ = os.Remove(path)
			continue
		}
		_ = conn.SetWriteDeadline(time.Now().Add(20 * time.Millisecond))
		_, _ = conn.Write(event)
		_ = conn.Close()
	}
}
