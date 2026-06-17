package main

import "testing"

// Minimal harness: extractClaudeWorkingVerb reads pc.short via getCapturedPane,
// so we seed capturedPanes directly to avoid spawning tmux.
func seedCapture(paneID, content string) {
	capturedPanes = map[string]*paneCapture{
		paneID: {short: content},
	}
	// scanClaudeTerminal memoizes per paneID across calls. Reset between
	// subtests so a previous capture's verb doesn't leak through.
	resetClaudeSignalsCache()
}

func TestExtractClaudeWorkingVerb(t *testing.T) {
	cases := []struct {
		name    string
		capture string
		want    string
	}{
		{
			"crafting with full stats line",
			"some scrollback\n" +
				"more scrollback\n" +
				"✦ Crafting… (25s · ↑ 421 tokens · esc to interrupt)\n" +
				"\n",
			"Crafting…",
		},
		{
			"pondering with ctrl+c variant",
			"✻ Pondering… (3s · ↑ 12 tokens · ctrl+c to interrupt)\n",
			"Pondering…",
		},
		{
			"verb buried near bottom of long capture",
			func() string {
				s := ""
				for i := 0; i < 50; i++ {
					s += "filler line\n"
				}
				return s + "✷ Brewing… (1m12s · ↑ 9876 tokens · esc to interrupt)\n" + "❯ "
			}(),
			"Brewing…",
		},
		{
			"no interrupt line — empty",
			"just a regular pane\nnothing happening here\n❯ \n",
			"",
		},
		{
			"interrupt line without verb shape — empty",
			"esc to interrupt some other text\n",
			"",
		},
		{
			"verb in scrollback only (out of bottom-15) — empty",
			func() string {
				s := "✦ Crafting… (25s · ↑ 1 tokens · esc to interrupt)\n"
				for i := 0; i < 30; i++ {
					s += "later line " + "\n"
				}
				return s
			}(),
			"",
		},
		{
			"new claude format — no interrupt suffix, dot glyph",
			"· Combobulating… (3m 45s · ↓ 10.9k tokens · thought for 1s)\n" +
				"\n" +
				"───────\n" +
				"❯ \n",
			"Combobulating…",
		},
		{
			"new claude format with star glyph",
			"✻ Combobulating… (3m 24s · ↓ 10.4k tokens · thought for 1s)\n",
			"Combobulating…",
		},
		{
			"capital word with paren but no tokens — empty",
			"Crafting… (some prose follows)\n",
			"",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			seedCapture("%1", c.capture)
			got := extractClaudeWorkingVerb("%1")
			if got != c.want {
				t.Fatalf("extractClaudeWorkingVerb() = %q, want %q\ncapture:\n%s",
					got, c.want, c.capture)
			}
		})
	}
}

// Snapshots of real Claude status lines observed in live tmux captures.
// Each line should resolve to its leading verb. Add new ones here when
// Claude's TUI shifts format — the matcher's correctness is anchored to
// these regression cases, not synthetic ones.
func TestMatchVerbLineLiveSamples(t *testing.T) {
	cases := []struct {
		line string
		want string
	}{
		{"✦ Crafting… (25s · ↑ 421 tokens · esc to interrupt)", "Crafting…"},
		{"✻ Combobulating… (3m 24s · ↓ 10.4k tokens · thought for 1s)", "Combobulating…"},
		{"· Combobulating… (3m 45s · ↓ 10.9k tokens · thought for 1s)", "Combobulating…"},
		{"✢ Honking… (1m 54s · ↓ 7.0k tokens · thought for 1s)", "Honking…"},
		{"✶ Ebbing… (10s · ↑ 465 tokens · thought for 1s)", "Ebbing…"},
		{"✻ Puzzling… (2m 51s · ↓ 9.5k tokens)", "Puzzling…"},
		// Past-tense (no ellipsis) — pane is idle, not running.
		{"✻ Cooked for 1m 11s", ""},
		// Prose with capital word + paren but no token-stats — must not match.
		{"Crafting… (some prose follows)", ""},
	}
	for _, c := range cases {
		got := matchVerbLine(c.line)
		if got != c.want {
			t.Errorf("matchVerbLine(%q) = %q, want %q", c.line, got, c.want)
		}
	}
}

// Simulates the new Claude layout where verb line is 8+ lines from bottom
// due to recap + dual separator + status bar.
func TestScanClaudeTerminalNewLayout(t *testing.T) {
	tests := []struct {
		name       string
		capture    string
		wantStatus string
		wantVerb   string
	}{
		{
			"verb 8 lines from bottom — new layout with recap",
			"some output\n" +
				"⏺ Running shell commands…\n" +
				"\n" +
				"✢ Transmuting… (16m · ↓ 7.6k tokens)\n" +
				"                                         ● high\n" +
				"───────────────\n" +
				"❯ \n" +
				"───────────────\n" +
				"  🤖 Opus 4.6 (1M)  ▰▱▱ 7%  📁 ~/project\n" +
				"  📝 +3234 -152\n" +
				"  -- INSERT -- ⏵⏵ auto mode on\n",
			"running",
			"Transmuting…",
		},
		{
			"open-ended question — idle, not needs-input",
			"⏺ All 21 alert rules linked.\n" +
				"\n" +
				"  Want me to skip the DB ones or create a dashboard?\n" +
				"\n" +
				"✻ Baked for 1m 34s\n" +
				"\n" +
				"───────────────\n" +
				"❯ \n" +
				"───────────────\n" +
				"  🤖 Opus 4.6  ▰▱▱ 12%\n" +
				"  -- INSERT -- ⏵⏵ auto mode on\n",
			"",
			"",
		},
		{
			"permission dialog — structured prompt is needs-input",
			"  Hook PreToolUse:Bash requires confirmation:\n" +
				"  Do you want to proceed?\n" +
				"  ❯ 1. Yes\n" +
				"    2. No\n" +
				"\n" +
				"  Esc to cancel · Tab to amend\n",
			"needs-input",
			"",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			seedCapture("%1", tt.capture)
			sig := scanClaudeTerminal("%1")
			if sig.status != tt.wantStatus {
				t.Errorf("status = %q, want %q", sig.status, tt.wantStatus)
			}
			if sig.verb != tt.wantVerb {
				t.Errorf("verb = %q, want %q", sig.verb, tt.wantVerb)
			}
		})
	}
}

func TestRainbowStylePtrStable(t *testing.T) {
	// Cycles through palette without panicking on negative or large frames.
	for _, n := range []int{0, 1, 7, 14, -1, -7, 1_000_000} {
		_ = rainbowStylePtr(n) // smoke — just ensure no panic / index OOB
	}
}
