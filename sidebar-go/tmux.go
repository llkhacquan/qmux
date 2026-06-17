package main

import (
	"expvar"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

// tmuxForks counts fork+exec of tmux via runTmux. Published on the pprof
// mux at /debug/vars (SIGUSR1 to start the server). Lets us measure the
// fork-rate reduction from control mode without dtrace: sample twice, diff.
var tmuxForks = expvar.NewInt("tmux_forks")

// cachedLookPath resolves a binary name once and caches the result.
// exec.LookPath does a full PATH stat walk on every call — 10-18% of CPU.
var (
	lookPathCache   = make(map[string]string)
	lookPathCacheMu sync.Mutex
)

func cachedLookPath(name string) string {
	lookPathCacheMu.Lock()
	defer lookPathCacheMu.Unlock()
	if p, ok := lookPathCache[name]; ok {
		return p
	}
	p, err := exec.LookPath(name)
	if err != nil {
		p = name
	}
	lookPathCache[name] = p
	return p
}

// runTmux executes a tmux command and returns trimmed stdout.
func runTmux(args ...string) (string, error) {
	tmuxForks.Add(1)
	cmd := exec.Command(cachedLookPath("tmux"), args...)
	cmd.Stderr = nil
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(out), "\n"), nil
}

// tmuxOption reads a global tmux option value. Returns "" on error.
// Global option, target-independent — safe over the control conn.
func tmuxOption(name string) string {
	out, err := tmuxQuery("show-options", "-gv", name)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// tmuxPaneID returns $TMUX_PANE or "".
func tmuxPaneID() string {
	return os.Getenv("TMUX_PANE")
}

// windowPaneCount returns the number of panes in the current tmux window.
func windowPaneCount() int {
	out, err := runTmux("display-message", "-p", "#{window_panes}")
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(out))
	return n
}
