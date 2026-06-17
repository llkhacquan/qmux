package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

func cfgGhCacheTTL() time.Duration { return Cfg().Timing.GhCacheTTL.Duration }
func cfgGhTimeout() time.Duration  { return Cfg().Timing.GhTimeout.Duration }

// ghPRResult holds cached PR number and check details for a branch.
type ghPRResult struct {
	Number    int
	AllPass   bool     // true if all checks passed (or no checks)
	Failed    []string // names of failed checks
	Pending   []string // names of pending/running checks
	Review    string   // "approved", "changes_requested", "review_required", ""
	FetchedAt time.Time
}

var (
	ghCache     = make(map[string]*ghPRResult) // "root\tbranch" → result
	ghCacheMu   sync.Mutex
	ghCacheFile string
)

func init() {
	ghCacheFile = filepath.Join(stateDir(), "gh-cache.json")
	loadGHCacheFromDisk()
}

// ghDiskEntry is the JSON-serializable form of ghPRResult.
type ghDiskEntry struct {
	Number    int      `json:"n,omitempty"`
	AllPass   bool     `json:"ok,omitempty"`
	Failed    []string `json:"fail,omitempty"`
	Pending   []string `json:"pend,omitempty"`
	Review    string   `json:"rev,omitempty"`
	FetchedAt int64    `json:"ts"`
}

func loadGHCacheFromDisk() {
	data, err := os.ReadFile(ghCacheFile)
	if err != nil {
		return
	}
	var raw map[string]*ghDiskEntry
	if json.Unmarshal(data, &raw) != nil {
		return
	}
	ghCacheMu.Lock()
	defer ghCacheMu.Unlock()
	for k, v := range raw {
		ghCache[k] = &ghPRResult{
			Number:    v.Number,
			AllPass:   v.AllPass,
			Failed:    v.Failed,
			Pending:   v.Pending,
			Review:    v.Review,
			FetchedAt: time.UnixMilli(v.FetchedAt),
		}
	}
}

func saveGHCacheToDisk() {
	ghCacheMu.Lock()
	raw := make(map[string]*ghDiskEntry, len(ghCache))
	for k, v := range ghCache {
		raw[k] = &ghDiskEntry{
			Number:    v.Number,
			AllPass:   v.AllPass,
			Failed:    v.Failed,
			Pending:   v.Pending,
			Review:    v.Review,
			FetchedAt: v.FetchedAt.UnixMilli(),
		}
	}
	ghCacheMu.Unlock()
	data, err := json.Marshal(raw)
	if err != nil {
		return
	}
	tmp := ghCacheFile + ".tmp"
	if os.WriteFile(tmp, data, 0o644) == nil {
		os.Rename(tmp, ghCacheFile)
	}
}

func ghCacheKey(root, branch string) string {
	return root + "\t" + branch
}

// ghPRInfo returns cached PR info for a branch.
func ghPRInfo(root, branch string) *ghPRResult {
	if root == "" || branch == "" {
		return nil
	}
	key := ghCacheKey(root, branch)
	ghCacheMu.Lock()
	cached := ghCache[key]
	ghCacheMu.Unlock()
	if cached != nil && cached.Number > 0 {
		return cached
	}
	return nil
}

func skipBranch(branch string) bool {
	return branch == "main" || branch == "master" || branch == "develop" || branch == "HEAD"
}

// ghPRViewJSON is the subset we parse from gh pr view --json.
type ghPRViewJSON struct {
	Number            int    `json:"number"`
	ReviewDecision    string `json:"reviewDecision"`
	StatusCheckRollup []struct {
		Name       string `json:"name"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
	} `json:"statusCheckRollup"`
}

// shortCheckName trims a check name to its most recognizable part.
// e.g. "pre_job" stays, "Security Scan / security-scan" → "security-scan"
func shortCheckName(name string) string {
	// If it has " / ", use the part after the slash (GitHub composite action names)
	if idx := strings.LastIndex(name, " / "); idx >= 0 {
		name = name[idx+3:]
	}
	// Trim common prefixes
	name = strings.TrimPrefix(name, "pre_job / ")
	return name
}

// isMeaningfulCheck filters out checks we don't care about (skipped, claude bots, etc.)
func isMeaningfulCheck(name, status, conclusion string) bool {
	lower := strings.ToLower(name)
	// Skip Claude Code bot checks (always skipped, just noise)
	if strings.Contains(lower, "claude") {
		return false
	}
	// Skip completed+skipped checks
	if status == "COMPLETED" && conclusion == "SKIPPED" {
		return false
	}
	return true
}

// parseCheckDetails extracts failed and pending check names from PR data.
func parseCheckDetails(pr *ghPRViewJSON) (allPass bool, failed, pending []string) {
	if len(pr.StatusCheckRollup) == 0 {
		return true, nil, nil
	}
	meaningful := 0
	for _, c := range pr.StatusCheckRollup {
		if !isMeaningfulCheck(c.Name, c.Status, c.Conclusion) {
			continue
		}
		meaningful++
		if c.Status != "COMPLETED" {
			pending = append(pending, shortCheckName(c.Name))
			continue
		}
		switch c.Conclusion {
		case "FAILURE", "CANCELLED", "TIMED_OUT", "ACTION_REQUIRED":
			failed = append(failed, shortCheckName(c.Name))
		}
	}
	allPass = meaningful > 0 && len(failed) == 0 && len(pending) == 0
	return
}

// BranchRef holds a git root + branch pair for prefetching.
type BranchRef struct {
	Root   string
	Branch string
}

// prefetchGHPR fetches PR info for branches in parallel with bounded concurrency.
// Checks disk cache first so other sidebar instances' fetches are reused.
func prefetchGHPR(refs []BranchRef) {
	now := time.Now()

	// Reload disk cache to pick up writes from other sidebar instances
	loadGHCacheFromDisk()

	seen := make(map[string]bool)
	var uncached []BranchRef
	ghCacheMu.Lock()
	for _, ref := range refs {
		if skipBranch(ref.Branch) || ref.Root == "" {
			continue
		}
		key := ghCacheKey(ref.Root, ref.Branch)
		if seen[key] {
			continue
		}
		seen[key] = true
		if cached, ok := ghCache[key]; ok && now.Sub(cached.FetchedAt) < cfgGhCacheTTL() {
			continue
		}
		uncached = append(uncached, ref)
	}
	ghCacheMu.Unlock()

	if len(uncached) == 0 {
		return
	}

	workers := min(len(uncached), 4)
	ch := make(chan BranchRef, len(uncached))
	for _, ref := range uncached {
		ch <- ref
	}
	close(ch)

	type fetchResult struct {
		key string
		res *ghPRResult
	}

	var wg sync.WaitGroup
	results := make(chan fetchResult, len(uncached))

	for range workers {
		wg.Go(func() {
			for ref := range ch {
				key := ghCacheKey(ref.Root, ref.Branch)

				ctx, cancel := context.WithTimeout(context.Background(), cfgGhTimeout())
				cmd := exec.CommandContext(ctx, cachedLookPath("gh"), "pr", "view", ref.Branch,
					"--json", "number,statusCheckRollup,reviewDecision")
				cmd.Dir = ref.Root
				cmd.Stderr = nil
				out, err := cmd.Output()
				cancel()

				if err != nil {
					// Failed (wrong token, no PR, timeout) — don't cache empty result
					// to avoid overwriting good data from another sidebar instance
					continue
				}
				var pr ghPRViewJSON
				if json.Unmarshal(out, &pr) != nil || pr.Number == 0 {
					continue
				}
				allPass, failed, pending := parseCheckDetails(&pr)
				res := &ghPRResult{
					Number:    pr.Number,
					AllPass:   allPass,
					Failed:    failed,
					Pending:   pending,
					Review:    strings.ToLower(pr.ReviewDecision),
					FetchedAt: now,
				}
				results <- fetchResult{key, res}
			}
		})
	}
	wg.Wait()
	close(results)

	ghCacheMu.Lock()
	for r := range results {
		ghCache[r.key] = r.res
	}
	ghCacheMu.Unlock()

	saveGHCacheToDisk()
}
