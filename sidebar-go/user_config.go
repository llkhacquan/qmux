package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/fsnotify/fsnotify"
)

// UserConfig is the top-level configuration loaded from the TOML file.
// Every field has a sensible default so the sidebar works with no config file.
type UserConfig struct {
	Workspaces []WorkspaceConfig `toml:"workspace"`
	Icons      IconsConfig       `toml:"icons"`
	Agent      AgentConfig       `toml:"agent"`
	Pricing    []PricingEntry    `toml:"pricing"`
	Theme      ThemeConfig       `toml:"theme"`
	Timing     TimingConfig      `toml:"timing"`
	Badges     BadgesConfig      `toml:"badges"`
	Sidebar    SidebarConfig     `toml:"sidebar"`
	Card       CardConfig        `toml:"card"`
}

type WorkspaceConfig struct {
	Path string `toml:"path"`
	Icon string `toml:"icon"`
	Name string `toml:"name"`
}

type IconsConfig struct {
	Orgs     map[string]string `toml:"orgs"`
	Sessions map[string]string `toml:"sessions"`
}

type AgentConfig struct {
	Name      string `toml:"name"`
	DetectRe  string `toml:"detect_regex"`
	DataDir   string `toml:"data_dir"`
}

type PricingEntry struct {
	Prefix     string  `toml:"prefix"`
	In         float64 `toml:"in"`
	Out        float64 `toml:"out"`
	CacheWrite float64 `toml:"cache_write"`
	CacheRead  float64 `toml:"cache_read"`
}

type ThemeConfig struct {
	Peach          string   `toml:"peach"`
	Green          string   `toml:"green"`
	Yellow         string   `toml:"yellow"`
	Blue           string   `toml:"blue"`
	Mauve          string   `toml:"mauve"`
	Dim            string   `toml:"dim"`
	Lavender       string   `toml:"lavender"`
	YellowStale    string   `toml:"yellow_stale"`
	YellowVeryStale string  `toml:"yellow_very_stale"`
	Rainbow        []string `toml:"rainbow"`
}

type TimingConfig struct {
	RefreshInterval       Duration `toml:"refresh_interval"`
	HiddenRefreshInterval Duration `toml:"hidden_refresh_interval"`
	InputPollMs           int      `toml:"input_poll_ms"`
	GitCacheTTL           Duration `toml:"git_cache_ttl"`
	GhCacheTTL            Duration `toml:"gh_cache_ttl"`
	GhTimeout             Duration `toml:"gh_timeout"`
	IntentStale           Duration `toml:"intent_stale"`
	HookRunningStale      Duration `toml:"hook_running_stale"`
	HookNeedsInputStale   Duration `toml:"hook_needs_input_stale"`
	DaemonBindPoll        Duration `toml:"daemon_bind_poll"`
	StandaloneDeadline    Duration `toml:"standalone_deadline"`
}

type BadgesConfig struct {
	Running    string `toml:"running"`
	NeedsInput string `toml:"needs_input"`
	Done       string `toml:"done"`
	Error      string `toml:"error"`
}

type SidebarConfig struct {
	Width          int `toml:"width"`
	Scrolloff      int `toml:"scrolloff"`
	MouseScrollLines int `toml:"mouse_scroll_lines"`
}

type CardConfig struct {
	ShowGit       bool `toml:"show_git"`
	ShowPR        bool `toml:"show_pr"`
	ShowContext   bool `toml:"show_context"`
	ShowIntent    bool `toml:"show_intent"`
	ShowShells    bool `toml:"show_shells"`
	ShowSubagents bool `toml:"show_subagents"`
	ShowLocation  bool `toml:"show_location"`
}

// Duration wraps time.Duration for TOML unmarshaling.
// Accepts strings like "10s", "5m", "25ms" or plain numbers (seconds).
type Duration struct {
	time.Duration
}

func (d *Duration) UnmarshalText(text []byte) error {
	dur, err := time.ParseDuration(string(text))
	if err != nil {
		return err
	}
	d.Duration = dur
	return nil
}

func (d Duration) MarshalText() ([]byte, error) {
	return []byte(d.Duration.String()), nil
}

func defaultConfig() UserConfig {
	return UserConfig{
		Icons: IconsConfig{
			Orgs:     map[string]string{},
			Sessions: map[string]string{},
		},
		Agent: AgentConfig{
			Name:     "Claude Code",
			DetectRe: `(?i)\bclaude\b`,
			DataDir:  "",
		},
		Pricing: []PricingEntry{
			{"claude-opus-4", 15.0, 75.0, 18.75, 1.50},
			{"claude-opus-3", 15.0, 75.0, 18.75, 1.50},
			{"claude-sonnet-4", 3.0, 15.0, 3.75, 0.30},
			{"claude-sonnet-3-7", 3.0, 15.0, 3.75, 0.30},
			{"claude-3-5-sonnet", 3.0, 15.0, 3.75, 0.30},
			{"claude-3-7-sonnet", 3.0, 15.0, 3.75, 0.30},
			{"claude-haiku-4", 1.0, 5.0, 1.25, 0.10},
			{"claude-haiku-3-5", 0.80, 4.0, 1.0, 0.08},
			{"claude-3-5-haiku", 0.80, 4.0, 1.0, 0.08},
			{"claude-haiku-3", 0.25, 1.25, 0.30, 0.03},
			{"claude-3-haiku", 0.25, 1.25, 0.30, 0.03},
		},
		Theme: ThemeConfig{
			Peach:           "#FAB387",
			Green:           "#A6E3A1",
			Yellow:          "#F9E2AF",
			Blue:            "#89B4FA",
			Mauve:           "#CBA6F7",
			Dim:             "#6C7086",
			Lavender:        "#B4BEFE",
			YellowStale:     "#C9B88A",
			YellowVeryStale: "#9A9080",
			Rainbow: []string{
				"#F38BA8", "#FAB387", "#F9E2AF", "#A6E3A1",
				"#94E2D5", "#89B4FA", "#CBA6F7",
			},
		},
		Timing: TimingConfig{
			RefreshInterval:       Duration{1 * time.Second},
			HiddenRefreshInterval: Duration{3 * time.Second},
			InputPollMs:           25,
			GitCacheTTL:           Duration{10 * time.Second},
			GhCacheTTL:            Duration{5 * time.Minute},
			GhTimeout:             Duration{2 * time.Second},
			IntentStale:           Duration{30 * time.Second},
			HookRunningStale:      Duration{60 * time.Second},
			HookNeedsInputStale:   Duration{120 * time.Second},
			DaemonBindPoll:        Duration{25 * time.Millisecond},
			StandaloneDeadline:    Duration{3 * time.Second},
		},
		Badges: BadgesConfig{
			Running:    "⏳",
			NeedsInput: "❓",
			Done:       "✅",
			Error:      "❌",
		},
		Sidebar: SidebarConfig{
			Width:            25,
			Scrolloff:        8,
			MouseScrollLines: 3,
		},
		Card: CardConfig{
			ShowGit:       true,
			ShowPR:        true,
			ShowContext:   true,
			ShowIntent:    true,
			ShowShells:    true,
			ShowSubagents: true,
			ShowLocation:  true,
		},
	}
}

// configPath returns the TOML config file path.
// Order: QMUX_CONFIG > XDG_CONFIG_HOME/qmux/config.toml > ~/.config/qmux/config.toml
func configPath() string {
	if p := os.Getenv("QMUX_CONFIG"); p != "" {
		return p
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "qmux", "config.toml")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "qmux", "config.toml")
}

var (
	globalCfg   UserConfig
	globalCfgMu sync.RWMutex
	cfgLoaded   bool
)

// Cfg returns the global config. Loaded on first access, reloadable via ReloadConfig.
func Cfg() *UserConfig {
	globalCfgMu.RLock()
	if cfgLoaded {
		c := &globalCfg
		globalCfgMu.RUnlock()
		return c
	}
	globalCfgMu.RUnlock()
	globalCfgMu.Lock()
	defer globalCfgMu.Unlock()
	if !cfgLoaded {
		globalCfg = loadConfig()
		cfgLoaded = true
	}
	return &globalCfg
}

// ReloadConfig re-reads the config file. Called by the daemon on fsnotify.
func ReloadConfig() {
	globalCfgMu.Lock()
	defer globalCfgMu.Unlock()
	globalCfg = loadConfig()
	cfgLoaded = true
	cachedAgentRe = nil
	rebuildStylesFrom(globalCfg.Theme)
}

var (
	cachedAgentRe      *regexp.Regexp
	cachedAgentPattern string
)

// agentDetectRegexp returns the compiled agent detection regex from config.
// Cached and recompiled only when the pattern changes (config reload).
func agentDetectRegexp() *regexp.Regexp {
	pattern := Cfg().Agent.DetectRe
	if cachedAgentRe != nil && cachedAgentPattern == pattern {
		return cachedAgentRe
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		re = regexp.MustCompile(`(?i)\bclaude\b`)
	}
	cachedAgentRe = re
	cachedAgentPattern = pattern
	return re
}

func loadConfig() UserConfig {
	cfg := defaultConfig()
	path := configPath()
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg
	}
	if err := toml.Unmarshal(data, &cfg); err != nil {
		fmt.Fprintf(os.Stderr, "config: parse error %s: %v\n", path, err)
		return defaultConfig()
	}
	applyDefaults(&cfg)
	return cfg
}

// applyDefaults fills zero-value fields that TOML left empty.
// TOML unmarshaling into a pre-filled struct preserves defaults for
// missing keys, but zeroes out fields in sections that ARE present
// but incomplete. This backfills those gaps.
func applyDefaults(cfg *UserConfig) {
	def := defaultConfig()
	if cfg.Icons.Orgs == nil {
		cfg.Icons.Orgs = def.Icons.Orgs
	}
	if cfg.Icons.Sessions == nil {
		cfg.Icons.Sessions = def.Icons.Sessions
	}
	if cfg.Agent.Name == "" {
		cfg.Agent.Name = def.Agent.Name
	}
	if cfg.Agent.DetectRe == "" {
		cfg.Agent.DetectRe = def.Agent.DetectRe
	}
	if len(cfg.Pricing) == 0 {
		cfg.Pricing = def.Pricing
	}
	if cfg.Theme.Peach == "" {
		cfg.Theme = def.Theme
	}
	if len(cfg.Theme.Rainbow) == 0 {
		cfg.Theme.Rainbow = def.Theme.Rainbow
	}
	if cfg.Timing.RefreshInterval.Duration == 0 {
		cfg.Timing = def.Timing
	}
	if cfg.Badges.Running == "" {
		cfg.Badges = def.Badges
	}
	if cfg.Sidebar.Width == 0 {
		cfg.Sidebar.Width = def.Sidebar.Width
	}
	if cfg.Sidebar.Scrolloff == 0 {
		cfg.Sidebar.Scrolloff = def.Sidebar.Scrolloff
	}
	if cfg.Sidebar.MouseScrollLines == 0 {
		cfg.Sidebar.MouseScrollLines = def.Sidebar.MouseScrollLines
	}
}

// AgentDataDir returns the resolved agent data directory.
// Falls back to ~/.claude/projects if not configured.
func (c *UserConfig) AgentDataDir() string {
	if c.Agent.DataDir != "" {
		return expandHome(c.Agent.DataDir)
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "projects")
}

func expandHome(path string) string {
	if len(path) >= 2 && path[:2] == "~/" {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, path[2:])
	}
	return path
}

// MatchWorkspace returns the first workspace whose resolved absolute path is
// a prefix of cwd. Both sides are resolved through expandHome and
// filepath.EvalSymlinks so symlinks and ~ paths match correctly.
func (c *UserConfig) MatchWorkspace(cwd string) *WorkspaceConfig {
	realCwd := resolvePath(cwd)
	for i := range c.Workspaces {
		ws := &c.Workspaces[i]
		prefix := resolvePath(expandHome(ws.Path))
		if strings.HasPrefix(realCwd, prefix) && (len(realCwd) == len(prefix) || realCwd[len(prefix)] == '/') {
			return ws
		}
	}
	return nil
}

// resolvePath expands ~ and resolves symlinks to a canonical absolute path.
func resolvePath(path string) string {
	expanded := expandHome(path)
	if resolved, err := filepath.EvalSymlinks(expanded); err == nil {
		return resolved
	}
	if abs, err := filepath.Abs(expanded); err == nil {
		return abs
	}
	return expanded
}

// DisplayName returns the workspace name, falling back to the last path segment.
func (w *WorkspaceConfig) DisplayName() string {
	if w.Name != "" {
		return w.Name
	}
	return filepath.Base(w.Path)
}

// WatchConfigFile watches the config TOML for changes and signals out on
// each write/create/rename. Runs in a background goroutine via safeGo.
// Both daemon and display clients call this so either side can react to
// config edits. The watcher creates the parent directory if missing so
// the user can create the file after startup.
func WatchConfigFile(out chan<- struct{}) {
	path := configPath()
	dir := filepath.Dir(path)
	os.MkdirAll(dir, 0o755)
	base := filepath.Base(path)

	w, err := fsnotify.NewWatcher()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: watch setup failed: %v\n", err)
		return
	}
	if err := w.Add(dir); err != nil {
		w.Close()
		fmt.Fprintf(os.Stderr, "config: watch dir %s failed: %v\n", dir, err)
		return
	}
	safeGo("config.watcher", func() {
		defer w.Close()
		for {
			select {
			case ev, ok := <-w.Events:
				if !ok {
					return
				}
				if filepath.Base(ev.Name) != base {
					continue
				}
				if ev.Has(fsnotify.Create) || ev.Has(fsnotify.Write) || ev.Has(fsnotify.Rename) {
					select {
					case out <- struct{}{}:
					default:
					}
				}
			case _, ok := <-w.Errors:
				if !ok {
					return
				}
			}
		}
	})
}
