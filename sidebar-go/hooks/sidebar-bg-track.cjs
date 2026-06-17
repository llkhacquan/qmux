#!/usr/bin/env node
/**
 * sidebar-bg-track - per-pane "what subagents are running right now" state
 *
 * Claude Code's built-in agent tracking marks background agents as "completed"
 * the moment the spawn tool_result lands. We need our own ledger updated on
 * actual SubagentStart / SubagentStop events.
 *
 * Behavior:
 *   start: append { id, type, started } to bg-{TMUX_PANE}.json
 *   stop:  remove entry by id
 *
 * State file: ~/.local/state/tmux-sidebar/bg-{paneID}.json
 *   (consumed by sidebar-go context.go)
 *
 * Install: copy to ~/.claude/hooks/ and register in settings.json
 * Config: settings.json hooks.SubagentStart / SubagentStop, async: true, timeout: 3
 *
 * Fail-open - never blocks parent on any error.
 */

const fs = require('fs');
const path = require('path');
const os = require('os');

const mode = process.argv[2];
if (mode !== 'start' && mode !== 'stop') process.exit(0);

const paneID = process.env.TMUX_PANE;
if (!paneID) process.exit(0);

let payload = {};
try {
  const stdin = fs.readFileSync(0, 'utf-8').trim();
  if (stdin) payload = JSON.parse(stdin);
} catch { /* fail-open */ }

const agentId = payload.agent_id;
const agentType = payload.agent_type || 'unknown';
if (!agentId) process.exit(0);

const stateDir = process.env.TMUX_SIDEBAR_STATE_DIR
  || (process.env.XDG_STATE_HOME && path.join(process.env.XDG_STATE_HOME, 'tmux-sidebar'))
  || path.join(os.homedir(), '.local', 'state', 'tmux-sidebar');

try {
  fs.mkdirSync(stateDir, { recursive: true });
  const file = path.join(stateDir, `bg-${paneID}.json`);

  let state = { agents: [] };
  try {
    const raw = fs.readFileSync(file, 'utf-8');
    const parsed = JSON.parse(raw);
    if (Array.isArray(parsed.agents)) state = parsed;
  } catch { /* missing or corrupt - start fresh */ }

  if (mode === 'start') {
    if (!state.agents.find(a => a.id === agentId)) {
      state.agents.push({ id: agentId, type: agentType, started: Date.now() });
    }
  } else {
    state.agents = state.agents.filter(a => a.id !== agentId);
  }

  const tmp = `${file}.${process.pid}.tmp`;
  fs.writeFileSync(tmp, JSON.stringify(state));
  fs.renameSync(tmp, file);
} catch { /* fail-open */ }

process.exit(0);
