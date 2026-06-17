package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/llkhacquan/qmux/sidebar-go/internal/sidebarstate"
)


const version = "sidebar-go v0.1.0"

func cfgRefreshSec() float64       { return Cfg().Timing.RefreshInterval.Seconds() }
func cfgHiddenRefreshSec() float64 { return Cfg().Timing.HiddenRefreshInterval.Seconds() }
func cfgInputPollMs() int          { return Cfg().Timing.InputPollMs }
func cfgMouseScrollLines() int     { return Cfg().Sidebar.MouseScrollLines }
func cfgSidebarWidth() int         { return Cfg().Sidebar.Width }
func cfgScrolloff() int            { return Cfg().Sidebar.Scrolloff }

var sidebarTitles = map[string]bool{
	"Sidebar":      true,
	"tmux-sidebar": true,
}

// stateDir returns the sidebar state directory path.
func stateDir() string { return sidebarstate.Dir() }

// configuredSidebarWidth reads sidebar width from saved file, or default.
func configuredSidebarWidth() int {
	data, err := os.ReadFile(filepath.Join(stateDir(), "width"))
	if err == nil {
		if n, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && n > 0 {
			return n
		}
	}
	return cfgSidebarWidth()
}

// saveSidebarWidth persists the user's preferred width.
func saveSidebarWidth(width int) {
	dir := stateDir()
	os.MkdirAll(dir, 0o755)
	tmp := filepath.Join(dir, "width.tmp")
	os.WriteFile(tmp, []byte(strconv.Itoa(width)), 0o644)
	os.Rename(tmp, filepath.Join(dir, "width"))
}

var (
	scrolloffCached   int
	scrolloffCachedAt time.Time
	scrolloffCacheMu  sync.Mutex
)

// configuredScrolloff reads scrolloff from tmux option, cached for 10s.
func configuredScrolloff() int {
	scrolloffCacheMu.Lock()
	defer scrolloffCacheMu.Unlock()
	if time.Since(scrolloffCachedAt) < 10*time.Second {
		return scrolloffCached
	}
	v := cfgScrolloff()
	if raw := tmuxOption("@tmux_sidebar_scrolloff"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n >= 0 {
			v = n
		}
	}
	scrolloffCached = v
	scrolloffCachedAt = time.Now()
	return v
}

// configuredSessionOrder returns the session sort order.
func configuredSessionOrder() []string {
	raw := tmuxOption("@tmux_sidebar_session_order")
	if raw == "" {
		return nil
	}
	var order []string
	for name := range strings.SplitSeq(raw, ",") {
		if n := strings.TrimSpace(name); n != "" {
			order = append(order, n)
		}
	}
	return order
}
