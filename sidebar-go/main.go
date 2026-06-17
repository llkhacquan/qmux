package main

import (
	"fmt"
	"net"
	"net/http"
	_ "net/http/pprof" // registers /debug/pprof/* on http.DefaultServeMux
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// windowActiveCache: tick handler writes true/false every 1s; usage goroutine
// reads it instead of forking 2 tmux commands per 30s cycle.
var windowActiveCache atomic.Bool

// sidebarWindowIsActive checks if the sidebar's window is the current client window.
// Uses #{window_active} (1 fork) instead of comparing two window_ids (2 forks).
func sidebarWindowIsActive() bool {
	pane := tmuxPaneID()
	if pane == "" {
		return false
	}
	out, err := tmuxQuery("display-message", "-p", "-t", pane, "#{window_active}")
	if err != nil {
		return false
	}
	return strings.TrimSpace(out) == "1"
}

// sidebarHasFocus checks if the sidebar pane is the active pane.
func sidebarHasFocus() bool {
	pane := tmuxPaneID()
	if pane == "" {
		return false
	}
	out, err := tmuxQuery("display-message", "-p", "-t", pane, "#{pane_active}")
	if err != nil {
		return false
	}
	return strings.TrimSpace(out) == "1"
}

// focusMainPane returns tmux focus to the main (non-sidebar) pane in the
// current window. Previously this shelled out to the upstream tmux-sidebar
// plugin's focus-main-pane.sh, which doesn't ship with sidebar-go-only
// installs — exec.Run silently failed and ESC moved the cursor in-model
// without any actual tmux focus change. Native tmux call instead: find
// the first non-sidebar pane in the current window and select it.
func focusMainPane() {
	currentWindow, _ := runTmux("display-message", "-p", "#{window_id}")
	currentWindow = strings.TrimSpace(currentWindow)
	if currentWindow == "" {
		return
	}
	raw, err := runTmux("list-panes", "-t", currentWindow, "-F", "#{pane_id}|#{pane_title}")
	if err != nil {
		return
	}
	for line := range strings.SplitSeq(raw, "\n") {
		parts := strings.SplitN(line, "|", 2)
		if len(parts) == 2 && !sidebarTitles[parts[1]] {
			runTmux("select-pane", "-t", parts[0])
			return
		}
	}
}

// loadViewState loads rows and reconciles the selected pane.
// hasFocusHint: if true, skip resolving the active pane (trust in-memory selection)
// Returns: rows, paneRows, cursorPaneID, activePaneID.
func loadViewState(selectedPaneID string, hasFocusHint bool) ([]Row, []Row, string, string) {
	rows := loadTree()
	paneRows := paneRowsFor(rows)
	activeID := activePaneID()
	if activeID == "" && len(paneRows) > 0 {
		activeID = paneRows[0].PaneID
	}
	if !hasFocusHint && !sidebarHasFocus() {
		// Sync cursor from shared file (set by other sidebar instances)
		if fileCursor := readCursorFile(); fileCursor != "" {
			selectedPaneID = fileCursor
		} else {
			selectedPaneID = activeID
		}
	}
	return rows, paneRows, reconcileSelectedPane(selectedPaneID, paneRows), activeID
}

// perfLog writes timing info (always enabled).
var (
	perfFile *os.File
	perfMu   sync.Mutex
)

// debugLog writes structured debug info to ~/.local/state/tmux-sidebar/debug.log.
// Enabled via SIDEBAR_DEBUG=1 env var. File is truncated at 100KB to prevent unbounded growth.
var debugFile *os.File

func debugLog(format string, args ...any) {
	if debugFile == nil {
		return
	}
	perfMu.Lock()
	// Rotate: truncate if over 100KB
	if info, err := debugFile.Stat(); err == nil && info.Size() > 100*1024 {
		debugFile.Truncate(0)
		debugFile.Seek(0, 0)
	}
	fmt.Fprintf(debugFile, "[%s] %s\n", time.Now().Format("15:04:05.000"), fmt.Sprintf(format, args...))
	perfMu.Unlock()
}

func perfStart(label string) time.Time {
	if perfFile == nil {
		return time.Time{}
	}
	return time.Now()
}

func perfEnd(label string, start time.Time) {
	if perfFile == nil || start.IsZero() {
		return
	}
	perfMu.Lock()
	fmt.Fprintf(perfFile, "[%s] %s: %s\n", time.Now().Format("15:04:05.000"), label, time.Since(start))
	perfMu.Unlock()
}

// pprof control: started lazily by SIGUSR1 so already-running sidebars
// can be profiled without restart. Each process binds 127.0.0.1:0 (kernel
// picks a free port — no conflict across N sidebars) and writes the addr
// to a per-pid discovery file.
//
// Activate:    kill -USR1 <pid>
// Discover:    ls ~/.local/state/tmux-sidebar/pprof/
// Profile:     go tool pprof -http=:8080 http://<addr>/debug/pprof/profile?seconds=30
var pprofOnce sync.Once

func startPprof() {
	pprofOnce.Do(func() {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			fmt.Fprintf(os.Stderr, "pprof listen: %v\n", err)
			return
		}
		addr := ln.Addr().String()
		dir := filepath.Join(stateDir(), "pprof")
		os.MkdirAll(dir, 0o755)
		pidFile := filepath.Join(dir, strconv.Itoa(os.Getpid()))
		os.WriteFile(pidFile, []byte(addr+"\n"), 0o644)
		go func() {
			defer os.Remove(pidFile)
			_ = http.Serve(ln, nil)
		}()
		fmt.Fprintf(os.Stderr, "pprof: http://%s/debug/pprof/  pid=%d\n", addr, os.Getpid())
	})
}

// installPprofSignal arms SIGUSR1 to lazy-start pprof. Idempotent — repeated
// signals are no-ops via pprofOnce. SIGUSR1 chosen because it has no default
// terminal-control meaning (unlike SIGINT/TERM/HUP).
func installPprofSignal() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGUSR1)
	go func() {
		for range ch {
			startPprof()
		}
	}()
}

// runInteractive runs the full interactive TUI on bubbletea. The previous
// tcell event-loop implementation was retired in favor of tea.Program; this
// wrapper just sets up debug/perf logging the same way the old loop did,
// then hands off to runBubble.
//
// MUST redirect os.Stderr before installing the pprof signal handler or
// starting any goroutine that may write to stderr (context/cursor watchers,
// pprof announcement on SIGUSR1). Bubbletea owns the TTY in interactive mode,
// so any stray stderr write corrupts the alt-screen — see SIGUSR1 path in
// startPprof which prints "pprof: http://..." to stderr.
func runInteractive() error {
	initLogging()
	cleanupStateDir()
	// Re-dup2 fd 2 to the live log file every 30s so subcommand rotations
	// don't leave us writing into a deleted inode (see startLogReopener).
	startLogReopener(30 * time.Second)
	if os.Getenv("SIDEBAR_DEBUG") == "1" || tmuxOption("@tmux_sidebar_debug") == "1" {
		dir := stateDir()
		os.MkdirAll(dir, 0o755)
		debugFile, _ = openLogFile(filepath.Join(dir, "debug.log"))
		if debugFile != nil {
			defer debugFile.Close()
		}
	}
	installPprofSignal()
	// Persistent control connection: replaces per-query tmux forks. Boots
	// best-effort — tmuxQuery falls back to forking if it never comes up.
	// Torn down before re-exec (runBubble) so the child doesn't leak.
	startControlConn()
	// Background scan of ~/.claude/projects/*.jsonl for today/7d/30d API-value.
	// Drives the bottom-line footer in tea_view; first scan runs immediately,
	// then every 30s. Cheap (mtime-skipped older files, dedup by message.id).
	startUsageRefresh(30 * time.Second)
	if os.Getenv("SIDEBAR_PERF") == "1" {
		dir := stateDir()
		os.MkdirAll(dir, 0o755)
		perfFile, _ = openLogFile(filepath.Join(dir, "perf.log"))
		if perfFile != nil {
			defer perfFile.Close()
		}
	}
	return runBubble()
}

// initLogging sets up file logging for subcommands (tmux run-shell hides stderr).
// Logs go to ~/.local/state/tmux-sidebar/sidebar-go.log with size-capped
// rotation (see logging.go). Each invocation writes a short banner line so
// later debug calls (fmt.Fprintf to os.Stderr) inherit subcommand context.
//
// CRITICAL: redirect fd 2 (not just os.Stderr) via syscall.Dup2 so Go
// runtime panics and any other writer of the underlying stderr descriptor
// land in the log too. Setting `os.Stderr = f` alone only catches code
// that explicitly writes through the *os.File variable; runtime crashes
// and bubbletea internal panics go to fd 2 directly, which tmux discards.
// Without dup2 we got silent pane deaths with zero log evidence.
func initLogging() {
	dir := stateDir()
	os.MkdirAll(dir, 0o755)
	f, err := openLogFile(filepath.Join(dir, "sidebar-go.log"))
	if err != nil {
		return
	}
	if err := syscall.Dup2(int(f.Fd()), 2); err != nil {
		// Best-effort — fall back to the Go-level redirect even if dup2 failed.
		_ = err
	}
	os.Stderr = f
	fmt.Fprintf(f, "[%s] sidebar-go %s\n", time.Now().Format("15:04:05"), strings.Join(os.Args[1:], " "))
}

func main() {
	sub := ""
	if len(os.Args) > 1 {
		sub = os.Args[1]
	}

	// Enable file logging for subcommands (not interactive mode). Status-bar
	// feed subcommands (window-name) fire every refresh tick and have no
	// debug value; skipping them keeps the log file usable. See logging.go.
	if sub != "" && sub != "--dump-render" && !noLogSubcommands[sub] {
		initLogging()
	}

	switch sub {
	case "--dump-render":
		dumpRender()
	case "--version":
		fmt.Println(version)
	case "toggle":
		cmdToggle()
	case "ensure":
		pane, win := argAt(2), argAt(3)
		cmdEnsure(pane, win)
	case "close":
		pane, win := argAt(2), argAt(3)
		cmdClose(pane, win)
	case "on-focus":
		pane, win := argAt(2), argAt(3)
		cmdOnFocus(pane, win)
	case "on-exit":
		pane, win := argAt(2), argAt(3)
		cmdOnExit(pane, win)
	case "notify":
		cmdNotify()
	case "focus":
		cmdFocusSidebar()
	case "init":
		cmdInit()
	case "switch-last":
		cmdSwitchLast()
	case "switch-next":
		cmdSwitchNext()
	case "switch-prev":
		cmdSwitchPrev()
	case "focus-up":
		cmdFocusNav("up")
	case "focus-down":
		cmdFocusNav("down")
	case "goto":
		// Jump to Nth pane in sidebar order (1-9)
		cmdJumpTo(argAt(2))
	case "window-name":
		cmdWindowName(argAt(2), argAt(3))
	case "serve":
		cmdServe()
	case "display":
		cmdDisplay()
	default:
		if err := runInteractive(); err != nil {
			fmt.Fprintf(os.Stderr, "sidebar-go: %v\n", err)
			os.Exit(1)
		}
	}
}

func argAt(i int) string {
	if i < len(os.Args) {
		return os.Args[i]
	}
	return ""
}
