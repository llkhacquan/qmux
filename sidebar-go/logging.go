package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"syscall"
	"time"
)

// safeGo runs fn in a goroutine that recovers panics, logs the stack to
// the sidebar log (fd 2 after dup2), and exits the goroutine. Use for
// every naked `go func()` that runs OUTSIDE bubbletea's Cmd worker pool
// — bubbletea wraps Cmds in its own recover, but inner goroutines spawned
// from a Cmd are NOT under that umbrella, and Go's recover doesn't
// propagate across goroutine boundaries. A single panic in one of those
// inner workers (e.g. a tree.go prefetch fan-out) takes the whole sidebar
// process down.
//
// `where` identifies the caller in the log so future crashes are
// attributable to a specific subsystem.
func safeGo(where string, fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				fmt.Fprintf(os.Stderr, "[%s] safeGo PANIC in %s: %v\n%s\n",
					time.Now().Format("15:04:05"), where, r, debug.Stack())
				_ = os.Stderr.Sync()
			}
		}()
		fn()
	}()
}

// Log rotation caps. Each log file rotates when it crosses logMaxBytes;
// up to logKeep older copies are retained as `<path>.1` … `<path>.N`.
// Total worst-case disk per log = logMaxBytes * (logKeep + 1).
const (
	logMaxBytes int64 = 10 << 20 // 10 MiB
	logKeep           = 3        // keeps .1 .2 .3, so ~40 MiB cap per log
)

// noLogSubcommands is the set of CLI subcommands that should NOT open the
// shared log file at all. These run on every status-bar refresh tick (1 Hz
// per pane) and previously accounted for ~98% of the log volume — purely
// catppuccin status feed, no debug value. `notify` is essentially a no-op
// (commands.go) wired to after-split-window/after-resize-pane/
// after-rename-window — fires every resize, log spam.
var noLogSubcommands = map[string]bool{
	"window-name": true,
	"notify":      true,
}

// openLogFile opens path for append, rotating first if it has grown past
// logMaxBytes. Multiple sidebar-go processes (one per tmux hook firing)
// can call this concurrently; the rotation race is intentionally accepted
// because:
//   - rename is atomic per-fs, so the loser of `path -> path.1` simply
//     overwrites the winner with the same just-rotated content, which is
//     a no-op data-wise;
//   - the freshly-created path is then appended to by all writers in
//     parallel — line writes from each fmt.Fprintf are small enough that
//     OS-level append guarantees keep them intact on Linux/macOS.
//
// Caller owns closing the returned file.
func openLogFile(path string) (*os.File, error) {
	if fi, err := os.Stat(path); err == nil && fi.Size() >= logMaxBytes {
		rotateLogFile(path, logKeep)
	}
	return os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
}

// rotateLogFile shifts <path>, <path>.1, … <path>.{keep-1} down by one,
// dropping <path>.{keep}. Errors are intentionally swallowed — log rotation
// must never break the calling subcommand. A missing intermediate file is
// expected on the first few rotations and is not an error.
func rotateLogFile(path string, keep int) {
	os.Remove(fmt.Sprintf("%s.%d", path, keep))
	for i := keep - 1; i >= 1; i-- {
		os.Rename(fmt.Sprintf("%s.%d", path, i), fmt.Sprintf("%s.%d", path, i+1))
	}
	os.Rename(path, path+".1")
}

// cleanupStateDir is a one-shot at-startup pass that removes orphaned
// state files left behind by retired code paths or crashed processes.
// Called from runInteractive (only the long-lived sidebar process pays
// the cost; subcommands skip).
//
// Targets:
//   - rowmap-*.json: emitted by the retired tcell renderer; no current
//     writer. ~500 files accumulate over months.
//   - pprof/<pid>: defer-removed when http.Serve exits cleanly, but
//     SIGKILL/OOM leaves them. Stat each, drop if pid is gone.
//
// Best-effort: errors are swallowed. A failed cleanup must never block
// sidebar startup.
func cleanupStateDir() {
	dir := stateDir()
	if rowmaps, err := filepathGlob(dir, "rowmap-*.json"); err == nil {
		for _, p := range rowmaps {
			os.Remove(p)
		}
	}
	pprofDir := dir + "/pprof"
	if entries, err := os.ReadDir(pprofDir); err == nil {
		for _, e := range entries {
			pid, perr := strconv.Atoi(e.Name())
			if perr != nil {
				continue
			}
			if !pidAlive(pid) {
				os.Remove(pprofDir + "/" + e.Name())
			}
		}
	}
}

// filepathGlob is a tiny wrapper to keep cleanupStateDir's call sites
// short. Returns nil slice on error.
func filepathGlob(dir, pattern string) ([]string, error) {
	return filepath.Glob(dir + "/" + pattern)
}

// pidAlive reports whether the given pid currently exists. syscall.Kill
// with signal 0 is the standard probe — it doesn't deliver a signal,
// just returns ESRCH if the process is gone, EPERM if alive but owned
// by another user (still alive), nil if alive and ours.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// startLogReopener launches a background goroutine that re-dup2s fd 2
// onto the live log file every interval. Subcommand processes rotate
// sidebar-go.log on size threshold (logging.go), but the long-lived
// interactive sidebar's fd 2 still points at the OLD inode after
// rotation — and after `logKeep` more rotations that inode is unlinked,
// making panic dumps land in a deleted-but-still-open file. This
// reopens the path periodically so fd 2 always tracks the live file.
//
// Uses safeGo so a panic in the reopener doesn't kill the sidebar.
func startLogReopener(interval time.Duration) {
	dir := stateDir()
	path := dir + "/sidebar-go.log"
	safeGo("logging.reopener", func() {
		for {
			time.Sleep(interval)
			f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
			if err != nil {
				continue
			}
			// Compare inode against current fd 2 — only dup2 if rotation
			// actually swapped the file. Avoids needless syscalls every
			// interval when nothing changed.
			var liveStat, fdStat syscall.Stat_t
			if syscall.Fstat(int(f.Fd()), &liveStat) == nil && syscall.Fstat(2, &fdStat) == nil {
				if liveStat.Ino == fdStat.Ino {
					f.Close()
					continue
				}
			}
			_ = syscall.Dup2(int(f.Fd()), 2)
			os.Stderr = f
			fmt.Fprintf(os.Stderr, "[%s] log reopen: fd 2 re-pointed at fresh log\n",
				time.Now().Format("15:04:05"))
		}
	})
}
