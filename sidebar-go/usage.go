package main

// Claude usage stats footer: scans ~/.claude/projects/*/*.jsonl for assistant
// messages, dedupes by message.id, multiplies tokens by per-model pricing
// to produce today's spend. Leader-only: active sidebar scans and publishes
// result to a shared cache file; hidden sidebars read the cache.
// Incremental: tracks per-file byte offset so only new appended lines are
// parsed on subsequent scans.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Pricing in USD per 1M tokens. Updated 2026-04. Fallback used for unknown models.
// Order of keys matters: longest-prefix match wins.
type modelPricing struct {
	in, out, cacheWrite, cacheRead float64
}

func priceFor(model string) modelPricing {
	for _, p := range Cfg().Pricing {
		if strings.Contains(model, p.Prefix) {
			return modelPricing{p.In, p.Out, p.CacheWrite, p.CacheRead}
		}
	}
	return modelPricing{3.0, 15.0, 3.75, 0.30}
}

// UsageStats is an aggregate across all projects for a single time window.
type UsageStats struct {
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	CacheCreate  int64   `json:"cache_create"`
	CacheRead    int64   `json:"cache_read"`
	CostUSD      float64 `json:"cost_usd"`
	Sessions     int     `json:"sessions"`
	Updated      time.Time
}

// UsagePeriods holds today / last-7-days / last-30-days aggregates.
type UsagePeriods struct {
	Today   UsageStats `json:"today"`
	Last7   UsageStats `json:"last7"`
	Last30  UsageStats `json:"last30"`
	Updated time.Time  `json:"updated"`
}

var (
	usageMu      sync.RWMutex
	currentUsage UsagePeriods
)

// getUsage returns the latest cached usage snapshot (all periods).
func getUsage() UsagePeriods {
	usageMu.RLock()
	defer usageMu.RUnlock()
	return currentUsage
}

// setUsage stores a usage snapshot. A display thin client uses this to apply
// the usage block from a daemon push, since it runs no scanner of its own.
func setUsage(p UsagePeriods) {
	usageMu.Lock()
	currentUsage = p
	usageMu.Unlock()
}

// startUsageRefresh launches a background goroutine that recomputes today's
// usage every `interval`. Leader-only: active sidebar scans and writes cache;
// hidden sidebars read the cache file instead of scanning 600+ JSONL files.
func startUsageRefresh(interval time.Duration) {
	safeGo("usage.refresh", func() {
		for {
			var s UsagePeriods
			if windowActiveCache.Load() {
				s = scanUsageIncremental()
				writeUsageCacheFile(s)
			} else if cached, ok := readUsageCacheFile(); ok {
				s = cached
			}
			usageMu.Lock()
			currentUsage = s
			usageMu.Unlock()
			time.Sleep(interval)
		}
	})
}

func usageCachePath() string {
	return filepath.Join(stateDir(), "usage-cache.json")
}

func writeUsageCacheFile(p UsagePeriods) {
	data, err := json.Marshal(p)
	if err != nil {
		return
	}
	tmp := usageCachePath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return
	}
	os.Rename(tmp, usageCachePath())
}

func readUsageCacheFile() (UsagePeriods, bool) {
	data, err := os.ReadFile(usageCachePath())
	if err != nil {
		return UsagePeriods{}, false
	}
	var p UsagePeriods
	if json.Unmarshal(data, &p) != nil {
		return UsagePeriods{}, false
	}
	return p, true
}

// --- incremental scan cache ---

type usageScanState struct {
	dayStart  time.Time
	files     map[string]fileScanEntry
	seenMsg   map[string]struct{}
	sessToday map[string]struct{}
	sess7     map[string]struct{}
	sess30    map[string]struct{}
	result    UsagePeriods
}

type fileScanEntry struct {
	modtime time.Time
	size    int64
}

var (
	scanState   *usageScanState
	scanStateMu sync.Mutex
)

// jsonlAssistant is the minimal shape needed from an assistant record.
type jsonlAssistant struct {
	Type      string    `json:"type"`
	Timestamp time.Time `json:"timestamp"`
	SessionID string    `json:"sessionId"`
	Message   struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage struct {
			InputTokens              int64 `json:"input_tokens"`
			OutputTokens             int64 `json:"output_tokens"`
			CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

// scanUsageIncremental is the leader's scan path. It maintains a per-file
// offset cache so only newly appended bytes in changed JSONL files are parsed.
// Cache invalidates on day boundary change (midnight) or file truncation.
func scanUsageIncremental() UsagePeriods {
	root := Cfg().AgentDataDir()
	if root == "" {
		return UsagePeriods{Updated: time.Now()}
	}

	now := time.Now()
	y, m, d := now.Date()
	dayStart := time.Date(y, m, d, 0, 0, 0, 0, now.Location())
	day7Start := dayStart.AddDate(0, 0, -6)
	day30Start := dayStart.AddDate(0, 0, -29)
	scanFrom := day30Start

	scanStateMu.Lock()
	defer scanStateMu.Unlock()

	// Invalidate on day boundary change — "today" bucket shifts.
	if scanState != nil && !scanState.dayStart.Equal(dayStart) {
		scanState = nil
	}

	if scanState == nil {
		scanState = &usageScanState{
			dayStart:  dayStart,
			files:     make(map[string]fileScanEntry),
			seenMsg:   make(map[string]struct{}),
			sessToday: make(map[string]struct{}),
			sess7:     make(map[string]struct{}),
			sess30:    make(map[string]struct{}),
		}
	}
	c := scanState

	files, _ := filepath.Glob(filepath.Join(root, "*", "*.jsonl"))

	currentFiles := make(map[string]struct{}, len(files))
	needFullRebuild := false

	for _, path := range files {
		currentFiles[path] = struct{}{}
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		if info.ModTime().Before(scanFrom) {
			continue
		}

		cached, exists := c.files[path]
		if exists && cached.modtime.Equal(info.ModTime()) && cached.size == info.Size() {
			continue
		}

		if exists && info.Size() < cached.size {
			needFullRebuild = true
			break
		}

		// Seek past already-processed bytes.
		offset := int64(0)
		if exists {
			offset = cached.size
		}

		scanFileIncremental(c, path, offset, dayStart, day7Start, day30Start)
		c.files[path] = fileScanEntry{modtime: info.ModTime(), size: info.Size()}
	}

	// Detect removed files.
	if !needFullRebuild {
		for path := range c.files {
			if _, exists := currentFiles[path]; !exists {
				needFullRebuild = true
				break
			}
		}
	}

	if needFullRebuild {
		scanState = nil
		return scanUsageFull()
	}

	c.result.Today.Sessions = len(c.sessToday)
	c.result.Last7.Sessions = len(c.sess7)
	c.result.Last30.Sessions = len(c.sess30)
	c.result.Updated = now
	c.result.Today.Updated = now
	c.result.Last7.Updated = now
	c.result.Last30.Updated = now
	return c.result
}

// scanFileIncremental reads new bytes from a single JSONL file starting at
// offset and accumulates into the scan cache.
func scanFileIncremental(c *usageScanState, path string, offset int64, dayStart, day7Start, day30Start time.Time) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	if offset > 0 {
		if _, err := f.Seek(offset, 0); err != nil {
			return
		}
	}

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)

	assistantMarker := []byte(`"type":"assistant"`)

	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 || !bytesContains(line, assistantMarker) {
			continue
		}
		var rec jsonlAssistant
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		if rec.Type != "assistant" || rec.Timestamp.Before(day30Start) {
			continue
		}
		id := rec.Message.ID
		if id == "" {
			continue
		}
		if _, dup := c.seenMsg[id]; dup {
			continue
		}
		c.seenMsg[id] = struct{}{}

		u := rec.Message.Usage
		price := priceFor(rec.Message.Model)
		cost := float64(u.InputTokens)/1e6*price.in +
			float64(u.OutputTokens)/1e6*price.out +
			float64(u.CacheCreationInputTokens)/1e6*price.cacheWrite +
			float64(u.CacheReadInputTokens)/1e6*price.cacheRead

		addUsage(&c.result.Last30, u, cost)
		if rec.SessionID != "" {
			c.sess30[rec.SessionID] = struct{}{}
		}
		if !rec.Timestamp.Before(day7Start) {
			addUsage(&c.result.Last7, u, cost)
			if rec.SessionID != "" {
				c.sess7[rec.SessionID] = struct{}{}
			}
		}
		if !rec.Timestamp.Before(dayStart) {
			addUsage(&c.result.Today, u, cost)
			if rec.SessionID != "" {
				c.sessToday[rec.SessionID] = struct{}{}
			}
		}
	}
}

// scanUsageFull does a complete scan from scratch. Used on first run and
// when the incremental cache is invalidated (day boundary, file truncation).
func scanUsageFull() UsagePeriods {
	root := Cfg().AgentDataDir()
	if root == "" {
		return UsagePeriods{Updated: time.Now()}
	}

	now := time.Now()
	y, m, d := now.Date()
	dayStart := time.Date(y, m, d, 0, 0, 0, 0, now.Location())
	day7Start := dayStart.AddDate(0, 0, -6)
	day30Start := dayStart.AddDate(0, 0, -29)
	scanFrom := day30Start

	// Rebuild the scan cache from scratch.
	c := &usageScanState{
		dayStart:  dayStart,
		files:     make(map[string]fileScanEntry),
		seenMsg:   make(map[string]struct{}),
		sessToday: make(map[string]struct{}),
		sess7:     make(map[string]struct{}),
		sess30:    make(map[string]struct{}),
	}

	files, _ := filepath.Glob(filepath.Join(root, "*", "*.jsonl"))
	for _, path := range files {
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		if info.ModTime().Before(scanFrom) {
			continue
		}
		scanFileIncremental(c, path, 0, dayStart, day7Start, day30Start)
		c.files[path] = fileScanEntry{modtime: info.ModTime(), size: info.Size()}
	}

	c.result.Today.Sessions = len(c.sessToday)
	c.result.Last7.Sessions = len(c.sess7)
	c.result.Last30.Sessions = len(c.sess30)
	c.result.Updated = now
	c.result.Today.Updated = now
	c.result.Last7.Updated = now
	c.result.Last30.Updated = now

	scanState = c
	return c.result
}

// addUsage accumulates one message's tokens + cost into s.
func addUsage(s *UsageStats, u struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}, cost float64) {
	s.InputTokens += u.InputTokens
	s.OutputTokens += u.OutputTokens
	s.CacheCreate += u.CacheCreationInputTokens
	s.CacheRead += u.CacheReadInputTokens
	s.CostUSD += cost
}

// bytesContains is a tiny alloc-free substring check.
func bytesContains(b, sub []byte) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i+len(sub) <= len(b); i++ {
		match := true
		for j := 0; j < len(sub); j++ {
			if b[i+j] != sub[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// formatUsageFooter produces a width-adaptive one-liner comparing today / 7d / 30d.
// Returns empty string if stats not ready yet.
func formatUsageFooter(width int) string {
	p := getUsage()
	if p.Updated.IsZero() {
		return ""
	}
	t := p.Today

	// fresh = input the model actually read uncached; cached = cache-read (huge, cheap).
	fresh := t.InputTokens + t.CacheCreate
	cached := t.CacheRead
	total := fresh + cached + t.OutputTokens

	// "API-value" = what this usage would cost at Anthropic's public API rates.
	// Actual billing depends on your plan (Max/Team subscriptions are flat).
	xl := fmt.Sprintf("API-value 1d $%.2f · 7d $%.0f · 30d $%.0f · today: in %s · cache %s · out %s · %d sess",
		t.CostUSD, p.Last7.CostUSD, p.Last30.CostUSD,
		shortTokens(fresh), shortTokens(cached), shortTokens(t.OutputTokens), t.Sessions,
	)
	long := fmt.Sprintf("API-value 1d $%.2f · 7d $%.0f · 30d $%.0f · %s tok today",
		t.CostUSD, p.Last7.CostUSD, p.Last30.CostUSD, shortTokens(total),
	)
	medium := fmt.Sprintf("API-value 1d $%.2f · 7d $%.0f · 30d $%.0f",
		t.CostUSD, p.Last7.CostUSD, p.Last30.CostUSD,
	)
	short := fmt.Sprintf("API-value $%.2f today", t.CostUSD)

	switch {
	case width >= len([]rune(xl)):
		return xl
	case width >= len([]rune(long)):
		return long
	case width >= len([]rune(medium)):
		return medium
	default:
		return short
	}
}

// shortTokens formats 1234567 → "1.2M", 12345 → "12K".
func shortTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.0fK", float64(n)/1e3)
	default:
		return fmt.Sprintf("%d", n)
	}
}
