package sidebarstate

import (
	"net"
	"os"
	"syscall"
	"testing"
	"time"
)

// TestNotifyPeersDelivers verifies a doorbell datagram round-trips from
// NotifyPeers to a peer socket bound at <pid>.sock — using the test process's
// parent pid so the kill-0 liveness probe finds it alive and sends instead
// of unlinking. Mirrors the real flow: helper writes shared state, peer
// sidebar reads its socket, doorbell arrives.
func TestNotifyPeersDelivers(t *testing.T) {
	t.Setenv("TMUX_SIDEBAR_STATE_DIR", t.TempDir())
	if err := os.MkdirAll(SocketDir(), 0o700); err != nil {
		t.Fatalf("mkdir SocketDir: %v", err)
	}

	// Parent pid of the test binary (go test runner). Alive while the test
	// runs, so NotifyPeers won't reap the socket as stale.
	peerPid := os.Getppid()
	peerPath := SocketPathForPid(peerPid)

	addr := &net.UnixAddr{Name: peerPath, Net: "unixgram"}
	conn, err := net.ListenUnixgram("unixgram", addr)
	if err != nil {
		t.Fatalf("bind peer socket: %v", err)
	}
	defer conn.Close()
	defer os.Remove(peerPath)

	received := make(chan []byte, 1)
	go func() {
		buf := make([]byte, 256)
		n, _, err := conn.ReadFromUnix(buf)
		if err != nil {
			return
		}
		received <- append([]byte(nil), buf[:n]...)
	}()

	// Tiny settle window so the goroutine is parked in ReadFromUnix
	// before the datagram lands.
	time.Sleep(10 * time.Millisecond)

	NotifyPeers([]byte("ping"))

	select {
	case msg := <-received:
		if string(msg) != "ping" {
			t.Errorf("got %q want %q", msg, "ping")
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for doorbell")
	}
}

// TestNotifyPeersSkipsSelf — sender and listener share a pid (real ListenUDS
// is bound at <self-pid>.sock), so NotifyPeers must NOT echo our own
// doorbell back. Without the self-skip, every shared-state write would
// recursively wake the writing sidebar and burn CPU re-reading its own data.
func TestNotifyPeersSkipsSelf(t *testing.T) {
	t.Setenv("TMUX_SIDEBAR_STATE_DIR", t.TempDir())

	out := make(chan []byte, 4)
	conn := ListenUDS(out)
	if conn == nil {
		t.Fatal("ListenUDS returned nil")
	}
	defer conn.Close()

	NotifyPeers([]byte("ping"))

	select {
	case msg := <-out:
		t.Errorf("self-doorbell delivered: %q (NotifyPeers must skip own pid)", msg)
	case <-time.After(100 * time.Millisecond):
		// Expected: nothing arrives.
	}
}

// TestNotifyPeersUnlinksDeadSocket — a leftover <pid>.sock from a crashed
// sidebar must get reaped on the next fan-out so the directory doesn't grow
// without bound. We touch a socket file with a pid we're confident is dead;
// NotifyPeers's kill-0 probe must catch ESRCH and unlink.
func TestNotifyPeersUnlinksDeadSocket(t *testing.T) {
	t.Setenv("TMUX_SIDEBAR_STATE_DIR", t.TempDir())
	if err := os.MkdirAll(SocketDir(), 0o700); err != nil {
		t.Fatalf("mkdir SocketDir: %v", err)
	}

	deadPid := findDeadPid(t)
	deadPath := SocketPathForPid(deadPid)
	if err := os.WriteFile(deadPath, nil, 0o600); err != nil {
		t.Fatalf("create stale socket: %v", err)
	}

	NotifyPeers([]byte("ping"))

	if _, err := os.Stat(deadPath); !os.IsNotExist(err) {
		t.Errorf("expected stale socket %s to be unlinked, stat err=%v", deadPath, err)
	}
}

// TestPatchCursorActive covers the merge contract used by the lean helper:
// promotes prior active to last_active on transition, no-ops on identical
// input, and preserves opaque sibling fields the helper doesn't know about.
func TestPatchCursorActive(t *testing.T) {
	doc := []byte(`{"cursor":"%1","active":"%1","rows":[{"k":1}],"unread":{"%2":true}}`)

	out, changed, ok := PatchCursorActive(doc, "%2", "%2", "@5")
	if !ok || !changed {
		t.Fatalf("expected patch to apply: ok=%v changed=%v", ok, changed)
	}
	// Verify last_active promoted, active_window written, opaque siblings preserved.
	wantSubs := []string{
		`"cursor":"%2"`,
		`"active":"%2"`,
		`"active_window":"@5"`,
		`"last_active":"%1"`,
		`"rows":[{"k":1}]`,
		`"unread":{"%2":true}`,
	}
	for _, s := range wantSubs {
		if !contains(out, s) {
			t.Errorf("missing %q in output: %s", s, out)
		}
	}

	// Idempotent: same input → no-op.
	out2, changed2, ok2 := PatchCursorActive(out, "%2", "%2", "@5")
	if !ok2 || changed2 {
		t.Errorf("expected idempotent no-op, got changed=%v ok=%v", changed2, ok2)
	}
	if string(out2) != string(out) {
		t.Errorf("idempotent call mutated doc")
	}

	// Empty activeWindow leaves the stored value untouched (legacy caller).
	out3, changed3, ok3 := PatchCursorActive(out, "%3", "%3", "")
	if !ok3 || !changed3 {
		t.Fatalf("expected patch to apply: ok=%v changed=%v", ok3, changed3)
	}
	if !contains(out3, `"active_window":"@5"`) {
		t.Errorf("empty activeWindow clobbered stored value: %s", out3)
	}
}

func contains(haystack []byte, needle string) bool {
	return len(needle) <= len(haystack) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack []byte, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if string(haystack[i:i+len(needle)]) == needle {
			return i
		}
	}
	return -1
}

// findDeadPid scans backwards from a high pid range to locate one with no
// running process. Test infra only — the pid is used as a "guaranteed-dead"
// liveness probe target.
func findDeadPid(t *testing.T) int {
	t.Helper()
	for pid := 99999; pid > 50000; pid-- {
		proc, err := os.FindProcess(pid)
		if err != nil {
			continue
		}
		if err := proc.Signal(syscall.Signal(0)); err != nil {
			return pid
		}
	}
	t.Fatal("could not find a dead pid in scan range")
	return 0
}
