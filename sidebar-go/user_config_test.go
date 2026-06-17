package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDefaultConfigHasSaneValues(t *testing.T) {
	cfg := defaultConfig()
	if cfg.Sidebar.Width != 25 {
		t.Errorf("default width = %d, want 25", cfg.Sidebar.Width)
	}
	if cfg.Timing.RefreshInterval.Duration != 1*time.Second {
		t.Errorf("default refresh = %v, want 1s", cfg.Timing.RefreshInterval)
	}
	if cfg.Agent.Name != "Claude Code" {
		t.Errorf("default agent name = %q, want Claude Code", cfg.Agent.Name)
	}
	if len(cfg.Pricing) == 0 {
		t.Error("default pricing table empty")
	}
	if len(cfg.Theme.Rainbow) != 7 {
		t.Errorf("default rainbow len = %d, want 7", len(cfg.Theme.Rainbow))
	}
}

func TestLoadConfigMissingFile(t *testing.T) {
	t.Setenv("QMUX_CONFIG", filepath.Join(t.TempDir(), "nonexistent.toml"))
	cfg := loadConfig()
	if cfg.Sidebar.Width != 25 {
		t.Errorf("missing file should return defaults, width = %d", cfg.Sidebar.Width)
	}
}

func TestLoadConfigPartialOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	content := `
[sidebar]
width = 30

[icons.orgs]
myorg = "X"

[timing]
refresh_interval = "2s"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("QMUX_CONFIG", path)
	cfg := loadConfig()
	if cfg.Sidebar.Width != 30 {
		t.Errorf("width = %d, want 30", cfg.Sidebar.Width)
	}
	if cfg.Icons.Orgs["myorg"] != "X" {
		t.Errorf("org icon = %q, want X", cfg.Icons.Orgs["myorg"])
	}
	if cfg.Timing.RefreshInterval.Duration != 2*time.Second {
		t.Errorf("refresh = %v, want 2s", cfg.Timing.RefreshInterval)
	}
	if cfg.Agent.Name != "Claude Code" {
		t.Errorf("unset agent name should default, got %q", cfg.Agent.Name)
	}
}

func TestLoadConfigInvalidTOML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.toml")
	os.WriteFile(path, []byte("not [valid toml"), 0o644)
	t.Setenv("QMUX_CONFIG", path)
	cfg := loadConfig()
	if cfg.Sidebar.Width != 25 {
		t.Errorf("bad TOML should fallback to defaults, width = %d", cfg.Sidebar.Width)
	}
}

func TestLoadConfigFullPricing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	content := `
[[pricing]]
prefix = "gpt-4o"
in = 5.0
out = 15.0
cache_write = 0.0
cache_read = 0.0
`
	os.WriteFile(path, []byte(content), 0o644)
	t.Setenv("QMUX_CONFIG", path)
	cfg := loadConfig()
	if len(cfg.Pricing) != 1 {
		t.Fatalf("pricing len = %d, want 1", len(cfg.Pricing))
	}
	if cfg.Pricing[0].Prefix != "gpt-4o" {
		t.Errorf("pricing prefix = %q, want gpt-4o", cfg.Pricing[0].Prefix)
	}
}

func TestAgentDataDirDefault(t *testing.T) {
	cfg := defaultConfig()
	dir := cfg.AgentDataDir()
	home, _ := os.UserHomeDir()
	want := filepath.Join(home, ".claude", "projects")
	if dir != want {
		t.Errorf("AgentDataDir() = %q, want %q", dir, want)
	}
}

func TestAgentDataDirCustom(t *testing.T) {
	cfg := defaultConfig()
	cfg.Agent.DataDir = "~/custom/agents"
	dir := cfg.AgentDataDir()
	home, _ := os.UserHomeDir()
	want := filepath.Join(home, "custom", "agents")
	if dir != want {
		t.Errorf("AgentDataDir() = %q, want %q", dir, want)
	}
}

func TestExpandHome(t *testing.T) {
	home, _ := os.UserHomeDir()
	tests := []struct {
		in, want string
	}{
		{"~/foo", filepath.Join(home, "foo")},
		{"/absolute/path", "/absolute/path"},
		{"relative", "relative"},
		{"~nope", "~nope"},
	}
	for _, tt := range tests {
		got := expandHome(tt.in)
		if got != tt.want {
			t.Errorf("expandHome(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestConfigPathEnvOverride(t *testing.T) {
	t.Setenv("QMUX_CONFIG", "/tmp/my-config.toml")
	if got := configPath(); got != "/tmp/my-config.toml" {
		t.Errorf("configPath() = %q, want /tmp/my-config.toml", got)
	}
}

func TestConfigPathXDG(t *testing.T) {
	t.Setenv("QMUX_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", "/tmp/xdg")
	want := filepath.Join("/tmp/xdg", "qmux", "config.toml")
	if got := configPath(); got != want {
		t.Errorf("configPath() = %q, want %q", got, want)
	}
}

func TestMatchWorkspace(t *testing.T) {
	cfg := defaultConfig()
	cfg.Workspaces = []WorkspaceConfig{
		{Path: "/opt/work/orgA", Icon: "A", Name: "alpha"},
		{Path: "/opt/work/orgB", Icon: "B"},
	}

	ws := cfg.MatchWorkspace("/opt/work/orgA/repo1")
	if ws == nil {
		t.Fatal("expected match for orgA/repo1")
	}
	if ws.Icon != "A" {
		t.Errorf("icon = %q, want A", ws.Icon)
	}
	if ws.DisplayName() != "alpha" {
		t.Errorf("name = %q, want alpha", ws.DisplayName())
	}

	ws = cfg.MatchWorkspace("/opt/work/orgB/repo2")
	if ws == nil {
		t.Fatal("expected match for orgB/repo2")
	}
	if ws.DisplayName() != "orgB" {
		t.Errorf("name = %q, want orgB (fallback to base)", ws.DisplayName())
	}

	ws = cfg.MatchWorkspace("/somewhere/else")
	if ws != nil {
		t.Errorf("expected no match, got %+v", ws)
	}
}

func TestMatchWorkspaceFirstWins(t *testing.T) {
	cfg := defaultConfig()
	cfg.Workspaces = []WorkspaceConfig{
		{Path: "/opt/work/orgA/repo1", Icon: "N"},
		{Path: "/opt/work/orgA", Icon: "A"},
	}
	ws := cfg.MatchWorkspace("/opt/work/orgA/repo1/src")
	if ws == nil || ws.Icon != "N" {
		t.Errorf("more specific path should match first, got %+v", ws)
	}
}

func TestWorkspaceTOMLRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	content := `
[[workspace]]
path = "/opt/work/orgA"
icon = "A"
name = "alpha"

[[workspace]]
path = "/opt/projects"
icon = "P"
`
	os.WriteFile(path, []byte(content), 0o644)
	t.Setenv("QMUX_CONFIG", path)
	cfg := loadConfig()
	if len(cfg.Workspaces) != 2 {
		t.Fatalf("workspaces len = %d, want 2", len(cfg.Workspaces))
	}
	if cfg.Workspaces[0].Path != "/opt/work/orgA" {
		t.Errorf("ws[0].path = %q", cfg.Workspaces[0].Path)
	}
	if cfg.Workspaces[1].Icon != "P" {
		t.Errorf("ws[1].icon = %q", cfg.Workspaces[1].Icon)
	}
}

func TestTruncatePathWorkspaceCollapse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	home, _ := os.UserHomeDir()
	content := fmt.Sprintf(`
[[workspace]]
path = "%s/work/orgA"
icon = "A"
`, home)
	os.WriteFile(path, []byte(content), 0o644)
	t.Setenv("QMUX_CONFIG", path)
	ReloadConfig()

	tests := []struct {
		input string
		want  string
	}{
		{"~/work/orgA/repo1", "A/repo1"},
		{"~/work/orgA/repo1/src/main.go", "A/repo1/src/main.go"},
		{"~/work/orgA", "A"},
		{"~/other/path", "~/other/path"},
	}
	for _, tt := range tests {
		got := truncatePath(tt.input, 100)
		if got != tt.want {
			t.Errorf("truncatePath(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
