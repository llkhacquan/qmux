package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

func cfgGitCacheTTL() time.Duration { return Cfg().Timing.GitCacheTTL.Duration }

var (
	gitCache     = make(map[string][2]string) // path → [repo_name, branch]
	gitCacheMu   sync.Mutex
	gitCacheExpAt time.Time
	gitCacheFile  string // set in init
)

func init() {
	gitCacheFile = filepath.Join(stateDir(), "git-cache.json")
	loadGitCacheFromDisk() // warm up from disk on startup only
}

// gitInfoSingle runs git to get repo name and branch for a path.
func gitInfoSingle(path string) (repoName, branch string) {
	cmd := exec.Command(cachedLookPath("git"), "-C", path, "rev-parse", "--show-toplevel", "--abbrev-ref", "HEAD")
	cmd.Stderr = nil
	out, err := cmd.Output()
	if err != nil {
		return filepath.Base(path), ""
	}
	lines := strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)
	if len(lines) >= 1 {
		repoName = filepath.Base(lines[0])
	}
	if len(lines) >= 2 {
		branch = lines[1]
	}
	return
}

// gitInfo returns cached (repo_name, branch) for a path.
func gitInfo(path string) (string, string) {
	if path == "" {
		return "", ""
	}
	gitCacheMu.Lock()
	defer gitCacheMu.Unlock()
	if v, ok := gitCache[path]; ok {
		return v[0], v[1]
	}
	repo, br := gitInfoSingle(path)
	gitCache[path] = [2]string{repo, br}
	return repo, br
}

// loadGitCacheFromDisk reads the shared git cache file.
func loadGitCacheFromDisk() {
	data, err := os.ReadFile(gitCacheFile)
	if err != nil {
		return
	}
	var raw map[string][]string
	if err := json.Unmarshal(data, &raw); err != nil {
		return
	}
	gitCacheMu.Lock()
	defer gitCacheMu.Unlock()
	for k, v := range raw {
		if len(v) >= 2 {
			gitCache[k] = [2]string{v[0], v[1]}
		}
	}
}

// saveGitCacheToDisk persists the cache for other sidebar instances.
func saveGitCacheToDisk() {
	gitCacheMu.Lock()
	raw := make(map[string][]string, len(gitCache))
	for k, v := range gitCache {
		raw[k] = v[:]
	}
	gitCacheMu.Unlock()

	data, err := json.Marshal(raw)
	if err != nil {
		return
	}
	dir := stateDir()
	os.MkdirAll(dir, 0o755)
	tmp := gitCacheFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return
	}
	os.Rename(tmp, gitCacheFile)
}

// prefetchGitInfo fetches git info for uncached paths in parallel.
func prefetchGitInfo(paths []string) {
	// Expire cache periodically — force fresh git fetch, don't reload stale disk cache
	now := time.Now()
	gitCacheMu.Lock()
	if now.After(gitCacheExpAt) {
		gitCache = make(map[string][2]string)
		gitCacheExpAt = now.Add(cfgGitCacheTTL())
	}
	gitCacheMu.Unlock()

	// Find uncached unique paths
	seen := make(map[string]bool)
	var uncached []string
	gitCacheMu.Lock()
	for _, p := range paths {
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		if _, ok := gitCache[p]; !ok {
			uncached = append(uncached, p)
		}
	}
	gitCacheMu.Unlock()

	if len(uncached) == 0 {
		return
	}

	// Parallel fetch with bounded concurrency
	workers := min(len(uncached), 8)
	ch := make(chan string, len(uncached))
	for _, p := range uncached {
		ch <- p
	}
	close(ch)

	var wg sync.WaitGroup
	type result struct {
		path string
		info [2]string
	}
	results := make(chan result, len(uncached))

	for range workers {
		wg.Go(func() {
			for p := range ch {
				repo, br := gitInfoSingle(p)
				results <- result{p, [2]string{repo, br}}
			}
		})
	}
	wg.Wait()
	close(results)

	gitCacheMu.Lock()
	for r := range results {
		gitCache[r.path] = r.info
	}
	gitCacheMu.Unlock()

	saveGitCacheToDisk()
}

// gitStatusResult holds cached git status counts.
type gitStatusResult struct {
	Changed   int
	Unpushed  int
	FetchedAt time.Time
}

var (
	gitStatusCache   = make(map[string]*gitStatusResult) // git root → result
	gitStatusCacheMu sync.Mutex
)

var gitRootCache sync.Map // pane path → git toplevel

// gitRootFor resolves the git toplevel for a path, cached to avoid repeated process spawns.
func gitRootFor(path string) string {
	if v, ok := gitRootCache.Load(path); ok {
		return v.(string)
	}
	cmd := exec.Command(cachedLookPath("git"), "-C", path, "rev-parse", "--show-toplevel")
	cmd.Stderr = nil
	root := path
	if out, err := cmd.Output(); err == nil {
		root = strings.TrimSpace(string(out))
	}
	gitRootCache.Store(path, root)
	return root
}

// prefetchGitStatusCounts fetches git status for unique repo roots in parallel.
// Uses --no-optional-locks to avoid blocking concurrent git operations.
// Caller (filterDirtyGitPaths) decides which roots need re-query via the
// git watcher dirty flags; no TTL gating here.
func prefetchGitStatusCounts(paths []string) {
	now := time.Now()
	roots := make(map[string]struct{})
	for _, p := range paths {
		if p == "" {
			continue
		}
		roots[gitRootFor(p)] = struct{}{}
	}

	var wg sync.WaitGroup
	type statusResult struct {
		root string
		res  *gitStatusResult
	}
	results := make(chan statusResult, len(roots))

	for root := range roots {
		wg.Add(1)
		r := root
		safeGo("git.statusFanout", func() {
			defer wg.Done()
			res := &gitStatusResult{FetchedAt: now}
			// --no-optional-locks prevents acquiring .git/index.lock
			cmd := exec.Command(cachedLookPath("git"), "--no-optional-locks", "-C", r, "status", "--porcelain")
			cmd.Stderr = nil
			if out, err := cmd.Output(); err == nil {
				for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
					if line != "" {
						res.Changed++
					}
				}
			}
			cmd = exec.Command(cachedLookPath("git"), "--no-optional-locks", "-C", r, "rev-list", "--count", "@{upstream}..HEAD")
			cmd.Stderr = nil
			if out, err := cmd.Output(); err == nil {
				s := strings.TrimSpace(string(out))
				if n, err := strconv.Atoi(s); err == nil {
					res.Unpushed = n
				}
			}
			results <- statusResult{r, res}
		})
	}
	wg.Wait()
	close(results)

	gitStatusCacheMu.Lock()
	for r := range results {
		gitStatusCache[r.root] = r.res
	}
	gitStatusCacheMu.Unlock()
}

// gitStatusCounts returns the number of changed files and unpushed commits.
func gitStatusCounts(path string) (changed, unpushed int) {
	root := gitRootFor(path)
	gitStatusCacheMu.Lock()
	cached := gitStatusCache[root]
	gitStatusCacheMu.Unlock()
	if cached != nil {
		return cached.Changed, cached.Unpushed
	}
	return 0, 0
}
