package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/fsnotify/fsnotify"
)

// gitDirtyWatcher monitors the gitDir (.git) to gate expensive
// git-status forks. When no files change, dirty stays false and
// prefetchGitStatusCounts skips the fork entirely.
type gitDirtyWatcher struct {
	mu    sync.RWMutex
	dirty map[string]bool     // gitRoot -> needs re-query
	roots map[string]struct{} // tracked git roots
	// pathToRoot maps watched directory/file prefixes back to git roots.
	// Built at addRoot() time so the event loop never forks git.
	pathToRoot map[string]string // watched path prefix -> gitRoot
	watcher    *fsnotify.Watcher
}

var globalGitWatcher atomic.Pointer[gitDirtyWatcher]

// newGitDirtyWatcher creates the watcher. Call addRoot() to start tracking repos.
func newGitDirtyWatcher() (*gitDirtyWatcher, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	gw := &gitDirtyWatcher{
		dirty:      make(map[string]bool),
		roots:      make(map[string]struct{}),
		pathToRoot: make(map[string]string),
		watcher:    w,
	}
	go gw.loop()
	return gw, nil
}

// resolveGitDir returns the actual .git directory for a repo root.
// Handles worktrees where .git is a file containing "gitdir: <path>".
func resolveGitDir(root string) string {
	dotgit := filepath.Join(root, ".git")
	info, err := os.Lstat(dotgit)
	if err != nil {
		return dotgit
	}
	if info.IsDir() {
		return dotgit
	}
	// Worktree: .git is a file, read the gitdir pointer
	data, err := os.ReadFile(dotgit)
	if err != nil {
		return dotgit
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if target, ok := strings.CutPrefix(line, "gitdir: "); ok {
			if !filepath.IsAbs(target) {
				target = filepath.Join(root, target)
			}
			return target
		}
	}
	return dotgit
}

// addRoot starts watching a git root if not already tracked.
// Marks it dirty on first add so the initial query runs.
// Resolves gitDir once and caches the mapping for the event loop.
//
// Only the gitDir is watched. refs/ is NOT: branch refs nest
// (refs/heads/user/x, refs/remotes/origin/x) and kqueue dir watches
// are non-recursive, so a refs watch never sees them anyway. Commits
// rewrite index (caught here); push/fetch freshness rides the
// gitWatcherMaxStaleness fallback.
func (gw *gitDirtyWatcher) addRoot(root string) {
	gw.mu.Lock()
	defer gw.mu.Unlock()
	if _, ok := gw.roots[root]; ok {
		return
	}
	gw.roots[root] = struct{}{}
	gw.dirty[root] = true // force initial query

	gitDir := resolveGitDir(root)

	// Watch the directory (not files): kqueue watches the inode, so a
	// rename-over (git's write-tmp+rename pattern for index/HEAD)
	// kills a file watch. Directory watches survive child renames.
	gw.pathToRoot[gitDir] = root
	if err := gw.watcher.Add(gitDir); err != nil {
		debugLog("git-watch: add %s: %v", gitDir, err)
	}
}

// IsDirty returns whether a git root has unseen changes.
func (gw *gitDirtyWatcher) IsDirty(root string) bool {
	gw.mu.RLock()
	defer gw.mu.RUnlock()
	return gw.dirty[root]
}

// ClearDirty marks a root as clean after git status has been fetched.
func (gw *gitDirtyWatcher) ClearDirty(root string) {
	gw.mu.Lock()
	gw.dirty[root] = false
	gw.mu.Unlock()
}

// gitDirRelevantFiles filters gitDir events to only the files that
// indicate repo state changes. Ignores lock files, editor temps, etc.
var gitDirRelevantFiles = map[string]bool{
	"index":       true, // staging area
	"HEAD":        true, // branch switch
	"MERGE_HEAD":  true, // merge in progress
	"FETCH_HEAD":  true, // fetch/pull (refreshes unpushed count)
	"packed-refs": true, // git pack-refs / some fetch paths
}

// loop processes fsnotify events and sets dirty flags.
func (gw *gitDirtyWatcher) loop() {
	for {
		select {
		case ev, ok := <-gw.watcher.Events:
			if !ok {
				return
			}
			// All watches are gitDir directories, so every event is a
			// direct child: filter by basename allowlist.
			base := filepath.Base(ev.Name)
			if !gitDirRelevantFiles[base] {
				continue
			}
			root := gw.rootForPath(ev.Name)
			if root == "" {
				continue
			}
			gw.mu.Lock()
			gw.dirty[root] = true
			gw.mu.Unlock()
			debugLog("git-watch: dirty %s (%s %s)", root, base, ev.Op)
		case err, ok := <-gw.watcher.Errors:
			if !ok {
				return
			}
			debugLog("git-watch: error: %v", err)
		}
	}
}

// rootForPath maps a filesystem event path back to a tracked git root
// using the cached pathToRoot map. No git forks - pure string matching.
func (gw *gitDirtyWatcher) rootForPath(path string) string {
	gw.mu.RLock()
	defer gw.mu.RUnlock()
	// Event for the gitDir itself
	if root, ok := gw.pathToRoot[path]; ok {
		return root
	}
	// Child of a watched gitDir (e.g. .git/index)
	dir := filepath.Dir(path)
	if root, ok := gw.pathToRoot[dir]; ok {
		return root
	}
	return ""
}

// startGitWatcherCmd boots the global git dirty watcher.
func startGitWatcherCmd() tea.Cmd {
	return func() tea.Msg {
		gw, err := newGitDirtyWatcher()
		if err != nil {
			debugLog("git watcher: %v", err)
			return gitWatcherReadyMsg{}
		}
		globalGitWatcher.Store(gw)
		return gitWatcherReadyMsg{}
	}
}

type gitWatcherReadyMsg struct{}

// filterDirtyGitPaths returns only paths whose git root is dirty.
// Also registers new roots with the watcher and clears dirty flags
// for roots that will be queried.
func filterDirtyGitPaths(paths []string) []string {
	gw := globalGitWatcher.Load()
	if gw == nil {
		return paths // no watcher, always query
	}

	// Register any new roots
	for _, p := range paths {
		if p == "" {
			continue
		}
		root := gitRootFor(p)
		gw.addRoot(root)
	}

	// Collect dirty paths (watcher-dirty or stale-cache fallback)
	seen := make(map[string]bool)
	var dirty []string
	for _, p := range paths {
		if p == "" {
			continue
		}
		root := gitRootFor(p)
		if seen[root] {
			continue
		}
		seen[root] = true
		if gw.IsDirty(root) || isGitStatusStale(root) {
			dirty = append(dirty, p)
			gw.ClearDirty(root)
		}
	}
	return dirty
}

// gitStatusCacheTTL gate: even when the watcher says clean, periodically
// re-query to catch events the watcher might have missed (macOS kqueue
// coalescing, deleted-then-recreated .git dirs, etc.).
const gitWatcherMaxStaleness = 30 * time.Second

// isGitStatusStale returns true if cached status for root is older than
// the max staleness window, forcing a periodic refresh.
func isGitStatusStale(root string) bool {
	gitStatusCacheMu.Lock()
	defer gitStatusCacheMu.Unlock()
	cached, ok := gitStatusCache[root]
	if !ok {
		return true
	}
	return time.Since(cached.FetchedAt) > gitWatcherMaxStaleness
}
