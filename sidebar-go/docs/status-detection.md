# Claude/Codex Status Detection

Sidebar shows a per-card status (running / needs-input / done / idle) and Claude's live verb (`Crafting…`, `Pondering…`, …). Neither comes from an API — Claude Code and Codex don't expose one. Everything is inferred from terminal capture + pane title heuristics.

## Why

Claude's TUI renders directly to the terminal — no IPC socket, no log file, no signal. The only stable observation surface is the rendered output. Sidebar treats every Claude pane like a black box and reads its surface to infer state.

This is intentionally heuristic: patterns may shift across Claude Code versions. The trade-off is zero coupling — the sidebar works against any version of Claude that follows the broad rendering conventions, no plugin protocol to keep in sync.

## What states we detect

| Status | Trigger | Border / badge |
|--------|---------|-----------------|
| **running** | Hook event, `ctrl+c to interrupt` / `esc to interrupt` line in pane bottom, or braille spinner in pane title | Rainbow ` <verb> ↗ ` border label, green stripe |
| **needs-input** | Hook event, permission prompts, `[y/n]`, numbered options, `Esc to cancel` dialogs | Peach ` asking ? ` border, blink |
| **done** | Status file marks transition `running -> idle` while user wasn't looking | Yellow ` done ● ` border |
| **error** | Error detected in pane | Red border |
| **idle** | None of the above | No badge |

## How — running detection

`status.go:effectivePaneStatus` resolves status through a priority chain:

1. **Hook-based status** (highest priority) - `sidebar-status-hook.sh` writes `hook-status-<paneID>.json` on Claude Code lifecycle events (PreToolUse, Stop, Notification). If fresh (within `hook_running_stale` / `hook_needs_input_stale` durations), used directly.
2. **Terminal scan** - `claudeTerminalStatus` runs against the cached pane capture (long form, `-S -100`):
   - **Bottom-15-line slice** - avoids false hits from stale text in scrollback.
   - **`hasInterrupt`** - any bottom line containing `ctrl+c` and `interrupt` means Claude is mid-response.
   - **Permission patterns** - `Do you want to proceed?`, `[y/n]`, numbered options + `Esc to cancel`, box-drawing dialog - needs-input.
3. **Pane title** - braille spinner (`⠐⠒⠖`) in title as backup running signal.
4. **Verb-as-running** - if `extractClaudeWorkingVerb` finds a verb but no status was detected, treat as running.

Inspired by [claude-tmux](https://github.com/nielsgroen/claude-tmux)'s structural detection approach.

## How — verb extraction

`status.go:extractClaudeWorkingVerb`. Claude prints the rotating status verb on its live status line. The format has shifted across versions:

```
# legacy
✦ Crafting… (25s · ↑ 421 tokens · esc to interrupt)

# newer (no "esc to interrupt" suffix, may use "·" glyph)
· Combobulating… (3m 45s · ↓ 10.9k tokens · thought for 1s)
```

Algorithm:

1. Read the cached pane capture (long form, `-S -100`; falls back to short).
2. Slice to the bottom 15 lines.
3. Walk **bottom-up** (newest first) — the freshest line wins.
4. Find a line where `… (` appears AND the parens contain `token`.
5. Take the substring up through `…` (inclusive).
6. The last whitespace-delimited token of that substring is the verb.
7. Sanity-check it ends in `…` and has letters before the ellipsis.

The `Word… (... tokens ...)` shape is the anchor — narrative prose almost never matches. Versions of Claude that include or omit `esc to interrupt` both work because we don't depend on it.

No regex. Reuses already-fetched capture data — zero extra tmux calls. Returns `""` if no match. Caller falls back to title-derived intent or `"Claude Code"`.

### Verb also drives status

When `claudeTerminalStatus` and `claudeTitleStatus` both come up empty (which they do on the new Claude format — no `ctrl+c to interrupt` text, no braille spinner in title), the verb's presence is itself the running signal. `tree.go` overrides:

```go
verb := extractClaudeWorkingVerb(pane.PaneID)
if verb != "" && status == "" {
    status = "running"
}
```

So the rainbow border + spinner badge fire correctly even when the legacy detection paths miss.

The verb shows up in two places:

- **Intent line** — `Crafting…` (verb verbatim, with trailing ellipsis).
- **Top border label** — ` crafting ↗ ` (lowercased, ellipsis stripped, alongside the rotating arrow), see [card-rendering.md](card-rendering.md).

## Why scraping, not statusline

`statusline-wrapper.cjs` (the Claude Code statusline hook) receives stdin JSON with `sessionId`, `model`, `cwd`, and other fields. The live verb is **not** included — it's rotated client-side in Claude's TUI render loop and never persisted. Statusline can't get it without doing the same scrape from a different process.

Capture-based scraping wins: data is already cached for `claudeTerminalStatus`, no new IPC, no new file.

## Tests

`status_test.go:TestExtractClaudeWorkingVerb` covers:
- Full status line (`✦ Crafting… (25s · ↑ 421 tokens · esc to interrupt)`)
- `ctrl+c to interrupt` variant
- Verb buried near the end of a long capture (within bottom-15)
- No interrupt line → empty
- Interrupt line without verb shape → empty
- Verb in scrollback only (out of bottom-15) → empty

## Implementation

| File | Symbol | Responsibility |
|------|--------|----------------|
| `status.go` | `effectivePaneStatus` | Top-level: hookStatus + title + terminal scan priority chain |
| `status.go` | `claudeTerminalStatus` | Bottom-line scan for running/needs-input |
| `status.go` | `extractClaudeWorkingVerb` | Verb extraction from interrupt-line |
| `status.go` | `claudeTitleStatus` | Pane title suffix parser |
| `context.go` | `hookStatusByPane` | Hook-based status from sidebar-status-hook.sh |
| `tree.go` | `loadTree` (intent + verb assembly) | Wires verb into card rows |

## Pitfalls observed

- **Don't scan the full long capture for the verb** — code samples or earlier turns containing `Word… (... tokens ...)` can false-hit. Bottom-15 slice + interrupt-line anchor is what makes it reliable.
- **Don't trust pane title for the verb** — Claude usually sets title to the user's prompt summary, not the rotating verb.
- **Don't treat `running` as "title is verb"** — title can be the user's prompt while the rendered TUI shows the verb. They're different sources.
