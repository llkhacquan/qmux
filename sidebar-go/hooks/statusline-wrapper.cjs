#!/usr/bin/env node
'use strict';

/**
 * Statusline wrapper - sits between Claude Code and any downstream statusline renderer.
 *
 * Claude Code pipes session JSON to stdin on every render cycle (~1s). This wrapper:
 * 1. Reads the full JSON (session_id, model, context_window, cost, etc.)
 * 2. Writes an enriched context file for sidebar-go (fsnotify-watched)
 * 3. Passes stdin through to stdout (pipe to your own statusline renderer if desired)
 *
 * The enriched file includes TMUX_PANE from the environment, enabling sidebar-go
 * to map context files to tmux panes without lsof/pgrep heuristics.
 *
 * Install: copy to ~/.claude/hooks/ and register in settings.json
 * Config: settings.json statusLine.command: "node ~/.claude/hooks/statusline-wrapper.cjs"
 */

const fs = require('fs');
const os = require('os');
const path = require('path');
const { execSync } = require('child_process');

const STATE_DIR = process.env.TMUX_SIDEBAR_STATE_DIR
  || (process.env.XDG_STATE_HOME && path.join(process.env.XDG_STATE_HOME, 'tmux-sidebar'))
  || path.join(os.homedir(), '.local', 'state', 'tmux-sidebar');
const CONTEXT_DIR = path.join(STATE_DIR, 'context');

let input = '';
process.stdin.setEncoding('utf8');
process.stdin.on('data', chunk => { input += chunk; });
process.stdin.on('end', () => {
  try {
    const data = JSON.parse(input);
    const sessionId = data.session_id;
    if (sessionId) {
      fs.mkdirSync(CONTEXT_DIR, { recursive: true });

      const ctxWindow = data.context_window || {};
      const usage = ctxWindow.current_usage || {};
      const size = ctxWindow.context_window_size || 0;

      let percent = 0;
      if (size > 0) {
        const preCalc = ctxWindow.used_percentage;
        if (typeof preCalc === 'number' && preCalc >= 0) {
          percent = Math.round(preCalc);
        } else {
          const tokens = (usage.input_tokens ?? 0)
            + (usage.cache_creation_input_tokens ?? 0)
            + (usage.cache_read_input_tokens ?? 0);
          if (size > 40000) {
            percent = Math.min(100, Math.round(((tokens + 40000) / size) * 100));
          }
        }
      }

      const enriched = {
        paneId: process.env.TMUX_PANE || '',
        sessionId,
        modelName: data.model?.display_name || 'Claude',
        percent,
        size,
        timestamp: Date.now(),
        effort: data.effort?.level || '',
        sessionName: data.session_name || '',
        worktree: data.worktree || null,
        cwd: data.workspace?.current_dir || data.cwd || '',
      };

      fs.writeFileSync(
        path.join(CONTEXT_DIR, `ck-context-${sessionId}.json`),
        JSON.stringify(enriched)
      );

      // Opportunistic cleanup: purge ck-context files whose paneId is no
      // longer a live tmux pane. Runs on ~5% of renders to keep the fast path cheap.
      if (Math.random() < 0.05) {
        try {
          const aliveRaw = execSync("tmux list-panes -a -F '#{pane_id}'", { encoding: 'utf8', timeout: 1000 });
          const alivePanes = new Set(aliveRaw.split('\n').map(s => s.trim()).filter(Boolean));
          for (const f of fs.readdirSync(CONTEXT_DIR)) {
            if (!f.startsWith('ck-context-') || !f.endsWith('.json')) continue;
            const p = path.join(CONTEXT_DIR, f);
            try {
              const ctx = JSON.parse(fs.readFileSync(p, 'utf8'));
              if (ctx.paneId && !alivePanes.has(ctx.paneId)) {
                fs.unlinkSync(p);
              }
            } catch { /* ignore individual file errors */ }
          }
        } catch { /* tmux missing or list-panes failed */ }
      }
    }
  } catch {
    // Never break statusline rendering on context write failure
  }

  // Pass stdin through to stdout so downstream renderers can consume it.
  // To chain with another renderer, pipe: "node statusline-wrapper.cjs | node your-renderer.cjs"
  process.stdout.write(input);
});
