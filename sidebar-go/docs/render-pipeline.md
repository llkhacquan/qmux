# Render Pipeline

Every visible frame goes through a fixed five-stage pipeline. Cost is dominated by the rainbow comet on working cards; everything else is noise. This doc maps each stage to its file, names the cadence drivers, and describes the cost shape so future tuning starts from the right place.

## Why

The codebase grew enough render-time decoration (rainbow comet × N working cards, marquee on overflowing titles, lavender title banner, double-line active frame, status-label injection on every border) that "where does the cost come from?" stopped being obvious from a casual read. The data plane is also no longer the dominant draw — that moved to `loadTree`'s tmux fork burst, which `shared-state-sync.md` covers. This doc is about the render plane: turning `m.rows` into bytes that go down the bubbletea wire.

## Pipeline stages

```
rows → renderRowsBeauty → rasterize → styleCells → decorateMarch → serializeWithCache
       (uncolored runes)  (Grid)     (style ptrs) (comet overlay) (ANSI strings, row-dirty cache)
```

Entry point is `tea_render.go::composeStyledLines`. Called from `tea_view.go::refreshContent`, which is the only path that produces bytes the viewport hands to bubbletea.

| Stage | File:func | What it produces |
|-------|-----------|------------------|
| `renderRowsBeauty` | `tea_legacy.go` | One uncolored string per row. Owns marquee on overflowing titles, active-frame heavy/double-line runes, label-reserve clipping. |
| `rasterize` | `render_cell.go` | A `Grid` of `Cell{R, Style, Slot, Italic}`. `classifySlot` tags every glyph by structural role (border / corner / status indicator / plain). |
| `styleCells` | `render_decorators.go` | Assigns `*lipgloss.Style` pointers per cell. Subroutines: `pickBorderStyle` (state-to-style), `applyBorderLabel` (verb/done/asking label). Note the `isBorderFillRune` gotcha - must list every weight's glyphs (thin, heavy, double) or labels silently disappear. |
| `decorateMarch` | `render_march.go` | Rainbow comet overlay on working cards, lavender comet on active idle cards. |
| `serializeWithCache` | `render_cell.go` | Coalesces adjacent cells with `cellStyleEqual` (pointer + italic flag) into one `style.Render(segment)` call per run. Per-row dirty cache skips unchanged rows. |

Per-cell styling means future effects drop in as additional decorator passes between `rasterize` and `serialize` without touching the pipeline shape.

## Cadence drivers

`refreshContent` is invoked from many message handlers in `tea_model.go`. The four periodic ones plus the cursor-mirror are the load-bearing cases:

| Cadence | Source | Triggers `refreshContent` when… |
|---------|--------|----------------------------------|
| 150ms | `spinTickMsg` (`tea_commands.go`) | `windowActive` && (any `running` pane OR `shouldRefreshForMarch`). Also bumps `m.spinFrame` unconditionally. |
| 700ms | `blinkTickMsg` (`tea_commands.go`) | `windowActive` && any `needs-input` pane. Toggles `m.blinkOn`. |
| 1s | `tickMsg` (`tea_commands.go`) | Indirectly - fires `loadTreeCmd` (active) or `loadSharedRowsCmd` (hidden); the resulting `rowsLoadedMsg` repaints, gated on `windowActive`. |
| event | `cursorChangedMsg` (`tea_model.go`) | fsnotify on shared-state. Mirror always applied; repaint gated on `changed && windowActive`. |
| event | `binaryChangedMsg`, focus, key, mouse | One-shot, paint immediately on the visible sidebar. |

`spinTickMsg` is the dominant render-side cost: 6.6 fps is fast enough that one wasted `composeStyledLines` × 9 fleet sidebars × 3600 frames/min adds up. Hence the visibility gate (see `visibility-gating.md`).

`shouldRefreshForMarch` (in `render_march.go`) suppresses repaints during the rest portion of the comet cycle. The march has a sweep window and a quiet window inside `marchCycleFrames`; only the sweep frames + one trailing frame (so the highlight clears cleanly) force a repaint.

## Cost shape

Total bytes per frame scales roughly linearly with row count. The dominant per-frame cost is the rainbow comet: it overrides ~2W+2H perimeter cells per working card, and `cellStyleEqual` segments a styled run every time the band index changes — three intensity bands per comet, plus the existing border style on either side, so a comet sweep adds ~6 segment boundaries per card per frame. Each boundary is one extra `style.Render` call producing ~50 bytes of ANSI escapes.

Other recent additions are visually noisy but cost-cheap:

- **Marquee** (`tea_legacy.go`, `marqueeWindow`) - only changes the *content* of one row when the title overflows its slot; segment count is unchanged. ~+1 changed line per ~300ms.
- **Lavender title banner** (`pickBorderStyle`, `styleActiveTitleBanner`) - fg-only style swap on the active card's top border. Same segment shape as a thin border with a label. Zero byte impact.
- **Double-line active frame** (`activeBorderSet = doubleBorderSet`, `tea_legacy.go`) - rune swap, not style swap. Same segment count as thin frames. Zero byte impact.

bubbletea diffs the renderer output at the line level, so what matters operationally is **which rows changed this frame**, not total bytes. A working card with the comet sweeping changes its top-border row + both walls + bottom-border row every spinTick. An idle card with no marquee changes nothing between spinTicks. So the per-tick wire cost is `running_card_count * 4 lines * ~300 bytes` plus marquee changes. With zero working cards and no marquee, spinTicks send nothing - the visibility gate in `tea_model.go` is what makes that cheap.

Source data note: this session ran perf tests on a 25-row fixture at width=40 to confirm the cost shape, then deleted the test files. Re-run if a numeric claim is needed; the qualitative shape (linear in rows, dominated by rainbow comet, marquee + banner + double-line are free) is what's load-bearing here.

## Implementation

| File | Symbol | Responsibility |
|------|--------|----------------|
| `tea_render.go` | `composeStyledLines` | Pipeline orchestrator |
| `tea_render.go` | `buildLabelReserves` | Per-pane right-edge reservation so marquee stops at label start |
| `tea_legacy.go` | `renderRowsBeauty` | Uncolored rune layout + marquee + active-frame runes |
| `tea_legacy.go` | `marqueeWindow` | Slot-rune-wide window into a wrapping title |
| `render_cell.go` | `rasterize`, `serializeWithCache`, `cellStyleEqual` | Grid + ANSI coalescing + row-dirty cache |
| `render_decorators.go` | `styleCells`, `pickBorderStyle`, `applyBorderLabel` | State→style mapping, label injection |
| `render_decorators.go` | `isBorderFillRune`, `activeBorderRune` | Border-rune set membership; missing entries silently drop labels |
| `render_march.go` | `decorateMarch`, `paintMarchPerimeter`, `shouldRefreshForMarch` | Comet overlay + repaint-skip gate |
| `render_state.go` | `buildRenderState`, `computeCardRanges` | Precomputed maps shared across decorators |
| `tea_view.go` | `refreshContent` | Composes lines + writes to viewport, re-asserts pinnedOffset |

## Pitfalls observed

- **`isBorderFillRune` must list every active border weight's glyphs.** Heavy and double-line variants were each added as separate gotcha incidents. If the active card uses heavy/double-line border runes and those aren't in the membership check, every fill cell counts as title text, `titleEnd` overshoots `labelStart`, and the verb/done/asking label silently disappears on every active card. See the comment block near `isBorderFillRune` in `render_decorators.go`.
- **Don't add a sixth decorator pass without measuring.** The pipeline is shallow today (rasterize - style - march - serialize) and that shallowness is what keeps the per-frame cost predictable. New effects that need to override styles should layer as a decorator between `styleCells` and `serialize`.
- **`cellStyleEqual` compares pointers, not values.** All canonical styles live as package-level vars in `tea_styles.go`. Constructing a fresh `lipgloss.Style` per cell would defeat segment coalescing - bytes per frame would explode 3-5x.
- **Marquee phase advances every 2 spinTicks (~300ms/rune)**, hardcoded in `composeStyledLines` as `m.spinFrame/2`. Faster makes long branches unreadable; slower feels stuck.
