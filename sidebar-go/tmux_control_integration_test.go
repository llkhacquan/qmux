package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestTmuxControlIntegration drives a real control-mode connection against
// a throwaway tmux server on a private socket. Skipped when tmux is absent.
func TestTmuxControlIntegration(t *testing.T) {
	tmuxBin, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux not in PATH")
	}

	sock := filepath.Join(t.TempDir(), "sock")
	run := func(args ...string) error {
		return exec.Command(tmuxBin, append([]string{"-S", sock}, args...)...).Run()
	}
	if err := run("new-session", "-d", "-s", "it", "-x", "80", "-y", "24"); err != nil {
		t.Fatalf("new-session: %v", err)
	}
	t.Cleanup(func() { run("kill-server") })

	tc, err := startTmuxControlOn([]string{"-S", sock}, nil)
	if err != nil {
		t.Fatalf("startTmuxControlOn: %v", err)
	}
	t.Cleanup(tc.Close)

	// display-message: a one-line reply.
	out, err := tc.run("display-message", "-p", "#{session_name}")
	if err != nil {
		t.Fatalf("display-message: %v", err)
	}
	if out != "it" {
		t.Errorf("session_name = %q, want it", out)
	}

	// list-sessions: confirms multi-field formatting round-trips.
	out, err = tc.run("list-sessions", "-F", "#{session_name}")
	if err != nil {
		t.Fatalf("list-sessions: %v", err)
	}
	if !strings.Contains(out, "it") {
		t.Errorf("list-sessions = %q, want to contain 'it'", out)
	}

	// An invalid command must surface as a routed error, not a hang.
	if _, err := tc.run("this-is-not-a-command"); err == nil {
		t.Error("invalid command: want error, got nil")
	}

	// Sequential commands must stay FIFO-aligned (no reply cross-routing).
	for i := range 5 {
		out, err := tc.run("display-message", "-p", "#{window_width}")
		if err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
		if out != "80" {
			t.Errorf("iter %d window_width = %q, want 80", i, out)
		}
	}
}

// TestControlSupervisorReconnect verifies the supervisor redials after the
// tmux server restarts, restoring globalTmuxControl.
func TestControlSupervisorReconnect(t *testing.T) {
	tmuxBin, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux not in PATH")
	}
	sock := filepath.Join(t.TempDir(), "sock")
	newServer := func() {
		exec.Command(tmuxBin, "-S", sock, "new-session", "-d", "-s", "it", "-x", "80", "-y", "24").Run()
	}
	newServer()

	// Point the supervisor's $TMUX-derived socket at our throwaway server.
	t.Setenv("TMUX", sock+",1,$0")
	startControlConn()
	t.Cleanup(stopControlConn)

	waitConn := func(want bool) bool {
		for range 100 { // up to ~10s
			got := globalTmuxControl.Load() != nil
			if got == want {
				return true
			}
			time.Sleep(100 * time.Millisecond)
		}
		return false
	}

	if !waitConn(true) {
		t.Fatal("supervisor never established initial conn")
	}
	if out, err := tmuxQuery("display-message", "-p", "#{session_name}"); err != nil || out != "it" {
		t.Fatalf("pre-restart query = %q, %v", out, err)
	}

	// Kill the server: conn dies, supervisor should drop globalTmuxControl.
	exec.Command(tmuxBin, "-S", sock, "kill-server").Run()
	if !waitConn(false) {
		t.Fatal("globalTmuxControl not cleared after server death")
	}

	// Bring the server back: supervisor must redial.
	newServer()
	if !waitConn(true) {
		t.Fatal("supervisor did not reconnect after server restart")
	}
	if out, err := tmuxQuery("display-message", "-p", "#{session_name}"); err != nil || out != "it" {
		t.Fatalf("post-reconnect query = %q, %v", out, err)
	}
}

// TestControlFailReapsChild verifies fail() reaps the `tmux -C` child instead
// of orphaning it. Regression: fail() used to close stdin without killing, and
// a later Close() short-circuited on closed==true — so on a wedged server (or
// any timed-out conn) the control child survived re-exec and held a server
// pipe open. Now fail() schedules a watchdog reap and Close() always blocks
// until the child is gone.
func TestControlFailReapsChild(t *testing.T) {
	tmuxBin, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux not in PATH")
	}
	sock := filepath.Join(t.TempDir(), "sock")
	if err := exec.Command(tmuxBin, "-S", sock, "new-session", "-d", "-s", "it").Run(); err != nil {
		t.Fatalf("new-session: %v", err)
	}
	t.Cleanup(func() { exec.Command(tmuxBin, "-S", sock, "kill-server").Run() })

	tc, err := startTmuxControlOn([]string{"-S", sock}, nil)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	<-tc.ready

	tc.fail() // timeout/write-error path — must not orphan the child
	select {
	case <-tc.Done():
	case <-time.After(controlTimeout + 2*time.Second):
		t.Fatal("fail() did not reap the control child (Done never fired)")
	}

	// Close() after fail() must still return (idempotent, no short-circuit hang).
	done := make(chan struct{})
	go func() { tc.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Close() after fail() hung")
	}
}

// TestTmuxControlConnDeathOnKill verifies Done() fires when the server dies.
func TestTmuxControlConnDeathOnKill(t *testing.T) {
	tmuxBin, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux not in PATH")
	}
	sock := filepath.Join(t.TempDir(), "sock")
	mk := exec.Command(tmuxBin, "-S", sock, "new-session", "-d", "-s", "it")
	if err := mk.Run(); err != nil {
		t.Fatalf("new-session: %v", err)
	}
	tc, err := startTmuxControlOn([]string{"-S", sock}, nil)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(tc.Close)

	exec.Command(tmuxBin, "-S", sock, "kill-server").Run()
	select {
	case <-tc.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("Done() did not fire after kill-server")
	}
}
