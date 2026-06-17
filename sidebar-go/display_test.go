package main

import (
	"os"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// shortStateDir points TMUX_SIDEBAR_STATE_DIR at a short temp dir. t.TempDir()
// embeds the (here long, subtest-derived) test name, which overflows the macOS
// 104-byte unix-socket sun_path limit when daemon.sock is appended.
func shortStateDir(t *testing.T) {
	t.Helper()
	dir, err := os.MkdirTemp("", "sb")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	t.Setenv("TMUX_SIDEBAR_STATE_DIR", dir)
}

// TestTryStartDaemonHerdGate verifies the flock gate: while a daemon holds the
// lock, a client's tryStartDaemon must NOT spawn a serve (herd suppressed); once
// the lock frees, the next call spawns exactly one. spawnServe is stubbed so the
// test never forks a real process.
func TestTryStartDaemonHerdGate(t *testing.T) {
	shortStateDir(t)

	spawns := 0
	orig := spawnDaemonHook
	spawnDaemonHook = func() { spawns++ }
	defer func() { spawnDaemonHook = orig }()

	lf, ok := acquireDaemonLock()
	if !ok {
		t.Fatal("could not acquire daemon lock")
	}
	tryStartDaemon() // lock held → must not spawn
	if spawns != 0 {
		t.Fatalf("spawned %d while lock held, want 0", spawns)
	}

	lf.Close()
	tryStartDaemon() // lock free → spawn exactly one
	if spawns != 1 {
		t.Fatalf("spawned %d after release, want 1", spawns)
	}
}

// TestConnectingView: a thin client with no snapshot yet renders the spinner;
// once a snapshot has arrived it no longer does (empty rows fall through to the
// plain placeholder).
func TestConnectingView(t *testing.T) {
	m := newTeaModel()
	m.clientMode = true
	m.width = 30
	m.height = 10
	if !strings.Contains(m.View(), "waiting for daemon") {
		t.Fatalf("pre-snapshot client View missing spinner:\n%s", m.View())
	}
	m.gotSnapshot = true
	if strings.Contains(m.View(), "waiting for daemon") {
		t.Fatal("spinner still shown after first snapshot")
	}
}

// TestStandaloneFallback: the deadline message routes to standalone only when no
// snapshot has been seen, and is a no-op once connected.
func TestStandaloneFallback(t *testing.T) {
	standaloneRequested = false
	m := newTeaModel()
	m.clientMode = true
	_, cmd := m.Update(standaloneFallbackMsg{})
	if !standaloneRequested {
		t.Fatal("standaloneRequested not set on pre-snapshot deadline")
	}
	if cmd == nil {
		t.Fatal("expected tea.Quit cmd")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("fallback cmd is not tea.Quit")
	}

	standaloneRequested = false
	m2 := newTeaModel()
	m2.clientMode = true
	m2.gotSnapshot = true
	if _, _ = m2.Update(standaloneFallbackMsg{}); standaloneRequested {
		t.Fatal("fallback fired after a snapshot was received")
	}
}
