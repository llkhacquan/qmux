package main

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestProtocolRoundTrip exercises the wire encode/decode helpers without a
// socket: an envelope written by writeMsg must decode back to the same payload.
func TestProtocolRoundTrip(t *testing.T) {
	cr, cw := net.Pipe()
	defer cr.Close()
	defer cw.Close()

	enc := json.NewEncoder(cw)
	dec := json.NewDecoder(cr)

	want := intentMsg{Action: actionScroll, PaneID: "%7", YOffset: 12, Pinned: true}
	go func() { _ = writeMsg(enc, msgIntent, want) }()

	env, err := readEnvelope(dec)
	if err != nil {
		t.Fatalf("readEnvelope: %v", err)
	}
	if env.T != msgIntent {
		t.Fatalf("type = %q, want %q", env.T, msgIntent)
	}
	var got intentMsg
	if err := decodePayload(env, &got); err != nil {
		t.Fatalf("decodePayload: %v", err)
	}
	if got != want {
		t.Fatalf("payload = %+v, want %+v", got, want)
	}
}

// TestDaemonHandshake spins up the accept loop on a temp socket (no engine) and
// verifies: a matching-proto client gets welcome{Ok:true} + an initial
// snapshot; a mismatched-proto client gets welcome{Ok:false} and the conn
// closes. State is isolated to a temp dir via TMUX_SIDEBAR_STATE_DIR.
func TestDaemonHandshake(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMUX_SIDEBAR_STATE_DIR", dir)

	sock := filepath.Join(dir, "daemon.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	d := newDaemon()
	go d.acceptLoop(ln)

	t.Run("accepted", func(t *testing.T) {
		conn, err := net.Dial("unix", sock)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		enc := json.NewEncoder(conn)
		dec := json.NewDecoder(conn)

		if err := writeMsg(enc, msgHello, helloMsg{Proto: protoVersion, PID: 1}); err != nil {
			t.Fatalf("send hello: %v", err)
		}

		welEnv, err := readEnvelope(dec)
		if err != nil || welEnv.T != msgWelcome {
			t.Fatalf("welcome envelope: %v T=%q", err, welEnv.T)
		}
		var wel welcomeMsg
		if err := decodePayload(welEnv, &wel); err != nil {
			t.Fatalf("decode welcome: %v", err)
		}
		if !wel.Ok || wel.Proto != protoVersion {
			t.Fatalf("welcome = %+v, want Ok=true Proto=%d", wel, protoVersion)
		}

		snapEnv, err := readEnvelope(dec)
		if err != nil || snapEnv.T != msgSnapshot {
			t.Fatalf("initial snapshot: %v T=%q", err, snapEnv.T)
		}
		var snap StateSnapshot
		if err := decodePayload(snapEnv, &snap); err != nil {
			t.Fatalf("decode snapshot: %v", err)
		}
	})

	t.Run("proto_mismatch", func(t *testing.T) {
		conn, err := net.Dial("unix", sock)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		enc := json.NewEncoder(conn)
		dec := json.NewDecoder(conn)

		if err := writeMsg(enc, msgHello, helloMsg{Proto: protoVersion + 99, PID: 1}); err != nil {
			t.Fatalf("send hello: %v", err)
		}
		welEnv, err := readEnvelope(dec)
		if err != nil || welEnv.T != msgWelcome {
			t.Fatalf("welcome envelope: %v T=%q", err, welEnv.T)
		}
		var wel welcomeMsg
		if err := decodePayload(welEnv, &wel); err != nil {
			t.Fatalf("decode welcome: %v", err)
		}
		if wel.Ok {
			t.Fatalf("welcome Ok=true on proto mismatch, want false")
		}
	})
}

// TestDisplayClientRoundTrip drives the real connManager.serve against a real
// daemon accept loop over a temp socket: the client must receive the initial
// snapshot on its channel, and an intent it sends must reach the daemon and
// mutate canonical state. Uses actionScroll because it touches only the
// shared-state file (no tmux), so the test stays hermetic.
func TestDisplayClientRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMUX_SIDEBAR_STATE_DIR", dir)

	sock := filepath.Join(dir, "daemon.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	d := newDaemon()
	go d.acceptLoop(ln)

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	mgr := &connManager{
		snapshot: make(chan StateSnapshot, 1),
		intent:   make(chan intentMsg, 8),
		stop:     make(chan struct{}),
	}
	defer close(mgr.stop)
	go func() {
		if mismatch := mgr.serve(conn); mismatch {
			t.Errorf("unexpected proto mismatch")
		}
	}()

	select {
	case <-mgr.snapshot:
	case <-time.After(2 * time.Second):
		t.Fatal("no initial snapshot pushed to client")
	}

	mgr.intent <- intentMsg{Action: actionScroll, YOffset: 42, Pinned: true}

	deadline := time.Now().Add(2 * time.Second)
	for {
		if s := readSharedState(); s.ViewYOffset == 42 && s.ViewPinned {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("daemon never applied scroll intent")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestAwaitBinaryChange verifies the upgrade watcher fires on the atomic-rename
// swap that `make install` performs: writing the watched basename in its dir
// must unblock awaitBinaryChange with true.
func TestAwaitBinaryChange(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "sidebar-go")
	if err := os.WriteFile(bin, []byte("v1"), 0o755); err != nil {
		t.Fatalf("seed binary: %v", err)
	}

	done := make(chan bool, 1)
	go func() { done <- awaitBinaryChange(bin) }()

	// Let the watcher arm before the install-simulating rename.
	time.Sleep(100 * time.Millisecond)
	tmp := bin + ".new"
	if err := os.WriteFile(tmp, []byte("v2"), 0o755); err != nil {
		t.Fatalf("write new: %v", err)
	}
	if err := os.Rename(tmp, bin); err != nil {
		t.Fatalf("rename: %v", err)
	}

	select {
	case ok := <-done:
		if !ok {
			t.Fatal("awaitBinaryChange returned false on install")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("awaitBinaryChange never fired on install")
	}
}

// TestBroadcastReexec verifies the advisory reexec frame reaches a registered
// client. (The syscall.Exec itself is not exercised — it would replace the test
// process.)
func TestBroadcastReexec(t *testing.T) {
	d := newDaemon()
	c := &daemonClient{send: make(chan []byte, 4), done: make(chan struct{})}
	d.addClient(c)

	d.broadcastReexec()

	select {
	case b := <-c.send:
		var env Envelope
		if err := json.Unmarshal(b, &env); err != nil {
			t.Fatalf("decode frame: %v", err)
		}
		if env.T != msgReexec {
			t.Fatalf("frame type = %q, want %q", env.T, msgReexec)
		}
	case <-time.After(time.Second):
		t.Fatal("no reexec frame delivered to client")
	}
}

// TestDaemonLockExclusion verifies the flock is exclusive: a second acquire
// while the first is held must fail, and succeed again after release.
func TestDaemonLockExclusion(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMUX_SIDEBAR_STATE_DIR", dir)

	lf, ok := acquireDaemonLock()
	if !ok {
		t.Fatal("first acquire failed")
	}
	if _, ok2 := acquireDaemonLock(); ok2 {
		t.Fatal("second acquire succeeded while lock held")
	}
	lf.Close()

	lf2, ok3 := acquireDaemonLock()
	if !ok3 {
		t.Fatal("acquire after release failed")
	}
	lf2.Close()
}
