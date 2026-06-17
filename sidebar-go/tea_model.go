package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
)

// teaModel is the bubbletea root model for the sidebar.
type teaModel struct {
	rows         []Row
	paneRows     []Row // subset of rows that represent a focusable pane
	width        int
	height       int
	activePaneID string
	// activeWindowID mirrors sharedState.ActiveWindow — the tmux window_id
	// holding focus. A Claude card in this window that isn't activePaneID
	// gets the quiet "window-active" frame.
	activeWindowID string
	cursorPaneID   string

	keys     keyMap
	viewport viewport.Model
	pendingG bool // tracks "g" → "gg" (top) sequence

	// styledLines caches the output of composeStyledLines. View() slices
	// this directly using viewport.YOffset, bypassing viewport.View()'s
	// lipgloss wrapping which ANSI-parses every visible line per frame.
	styledLines []string

	// gridCache stores the previous frame's styled cells + serialized output
	// so serializeWithCache can skip lipgloss.Render calls on unchanged rows.
	// On animation ticks, typically only 2-4 rows change (spinner, march
	// highlight), skipping ~80% of the serialize cost (83% of total frame).
	gridCache *cachedGrid

	// Search state. searchActive == true while typing; once Enter is pressed
	// the input is dismissed but query/matches stay so n/N can walk them.
	searchActive  bool
	searchInput   textinput.Model
	searchMatches map[int]bool

	// Status overlay state.
	unreadPanes      map[string]bool   // paneID → has unseen update since last selection
	prevStatus       map[string]string // paneID → last seen Status, for transition detection
	lastActivePaneID string            // mirror of sharedState.LastActive — single global "previous active" so all sidebars agree on the ◂ marked card
	spinFrame        int               // monotonically incrementing spinner frame counter
	blinkOn          bool              // peach blink phase for needs-input borders
	lastPaneCount    int               // stale-ActiveWindow fallback: repaint on structural change

	// ctxNotify is captured from the bootstrap Cmd so Update() can re-issue
	// waitCtxCmd after each notification without re-creating the watcher.
	ctxNotify <-chan struct{}

	// cursorNotify carries cursor + active pane IDs from peer sidebar instances.
	// Same re-arm pattern as ctxNotify.
	cursorNotify <-chan sharedStateSnapshot

	// focused tracks whether the sidebar tmux pane currently holds focus,
	// fed by tea.FocusMsg/BlurMsg (terminal CSI I/O sequences forwarded by
	// tmux when focus-events is on). cursorVisible additionally gates the
	// cursor highlight: focus alone does NOT show the cursor — the user must
	// press a navigation key (j/k/arrows/gg/G/digit/n/N) so the cursor
	// "appears" on demand. Loss of focus resets cursorVisible.
	focused       bool
	cursorVisible bool

	// scrolloff is cached from `tmuxOption("@tmux_sidebar_scrolloff")` to
	// avoid forking `tmux show-options` on every refreshContent (called
	// 6.6×/sec under spinTick). Refreshed on each 1s tickMsg so live edits
	// to the option take effect within a second.
	scrolloff int

	// windowActive caches sidebarWindowIsActive(), polled once per tickMsg.
	// Hidden sidebars skip loadTreeCmd (saves ~10 tmux capture-pane forks
	// per tick); on hidden→visible edge we fire loadTreeCmd immediately so
	// the user sees fresh state within ~1s of switching windows.
	windowActive bool

	// viewportPinned: user scrolled the viewport with the wheel (or Ctrl+D/U)
	// so the cursor row is no longer the anchor. While pinned, the periodic
	// refreshes (tickMsg, spinTickMsg, blinkTickMsg) skip ensureCursorVisible
	// — otherwise the viewport snaps back to the cursor within ≤1s and the
	// user can never look above/below their selection. Cleared by any
	// explicit cursor mutation (j/k/gg/G/digit/click/search/Enter/Esc).
	viewportPinned bool

	// pinnedOffset is the user's intended YOffset, persisted independently
	// of viewport.YOffset. Reason: viewport.SetYOffset clamps to the current
	// content's maxYOffset (len(lines)-Height), which is 0 at boot before
	// rowsLoadedMsg arrives. Storing intent here lets refreshContent
	// re-apply SetYOffset after each SetContent so a peer-sourced offset
	// "settles" once content actually loads. Also survives content shrinks
	// (sessions removed → grew back) without losing the user's view.
	pinnedOffset int

	// clientMode marks a `sidebar-go display` thin client. State arrives as
	// daemon snapshots (snapshotCh) instead of local loadTree; user actions
	// emit intents (intentTx) instead of writing the shared-state file; and
	// Init() starts NONE of the engine Cmds (loadTree, control conn, git/
	// usage/binary watchers). Skipping the binary watcher is deliberate: only
	// the daemon re-execs on upgrade, collapsing the fleet re-exec burst to
	// one. See display.go + architecture-daemon-thin-clients.md.
	clientMode bool
	snapshotCh <-chan StateSnapshot
	intentTx   chan<- intentMsg
	// windowID is this pane's tmux window_id, captured once at boot. Lets
	// windowActive derive from a snapshot's ActiveWindow without a per-tick
	// tmux fork (the whole point of the thin client).
	windowID string
	// gotSnapshot flips true on the first daemon snapshot (clientMode). Until
	// then View renders the connecting spinner instead of an empty card list,
	// and the standalone-fallback deadline can still fire.
	gotSnapshot bool

	// Per-workspace hide. fullRows is the unfiltered row set (kept so a hide
	// toggle can re-filter without a reload, and so the menu can list hidden
	// sessions); m.rows holds the visible subset that View/click/cursor use.
	// hiddenSessions is the current hidden set; workspaces backs the menu.
	fullRows       []Row
	hiddenSessions map[string]bool
	workspaces     []workspaceItem
	menuOpen       bool // right-click workspace show/hide menu is showing
}

// sendIntent forwards a user action to the daemon (clientMode). Non-blocking
// drop on a full channel — the daemon's next snapshot reconciles canonical
// state, so a dropped intent self-heals rather than stalling the UI.
func (m teaModel) sendIntent(in intentMsg) { sendIntentTo(m.intentTx, in) }

func sendIntentTo(tx chan<- intentMsg, in intentMsg) {
	if tx == nil {
		return
	}
	select {
	case tx <- in:
	default:
	}
}

// toggleHiddenSession flips a workspace's hidden bit. The local set updates
// immediately for instant feedback; persistence + fleet sync differ by mode:
// a thin client sends an intent (the daemon owns the tmux option + snapshot
// fan-out), a standalone sidebar writes the option itself. Either way we
// re-filter from fullRows so the view reacts without waiting for a reload.
func (m *teaModel) toggleHiddenSession(name string) {
	if name == "" {
		return
	}
	if m.hiddenSessions == nil {
		m.hiddenSessions = make(map[string]bool)
	}
	if m.hiddenSessions[name] {
		delete(m.hiddenSessions, name)
	} else {
		m.hiddenSessions[name] = true
	}
	if m.clientMode {
		m.sendIntent(intentMsg{Action: actionToggleHidden, Session: name})
	} else {
		writeHiddenOption(sortedHiddenSlice(m.hiddenSessions))
	}
	m.reapplyHidden()
}

// reapplyHidden recomputes the visible rows + menu list from fullRows against
// the current hidden set, keeping the cursor on a live pane.
func (m *teaModel) reapplyHidden() {
	m.workspaces = summarizeWorkspaces(m.fullRows, m.hiddenSessions)
	m.rows = filterHiddenRows(m.fullRows, m.hiddenSessions)
	m.paneRows = paneRowsFor(m.rows)
	m.cursorPaneID = reconcileSelectedPane(m.cursorPaneID, m.paneRows)
	m.refreshContent()
}

// publishCursor moves the global selection: an intent in clientMode, a
// shared-state write otherwise. Single funnel so the file-vs-daemon split
// stays out of the key handlers.
func (m *teaModel) publishCursor(paneID string) {
	if m.clientMode {
		m.sendIntent(intentMsg{Action: actionCursor, PaneID: paneID})
		return
	}
	writeCursorFile(paneID)
}

// syncSharedView publishes the local viewport state so peer sidebars can
// mirror it on the next cursorChangedMsg pump. Call after every mutation
// of m.viewportPinned or m.pinnedOffset that the user initiated. The
// underlying writer dedupes against shared state, so harmless calls
// (j/k repeats while already unpinned) cost only one flock+read.
//
// We publish m.pinnedOffset (user intent) rather than m.viewport.YOffset
// (which can be transiently clamped by SetContent during content shrink)
// so peer instances get the value the user actually wheel-scrolled to.
func (m *teaModel) syncSharedView() {
	off := 0
	if m.viewportPinned {
		off = m.pinnedOffset
	}
	if m.clientMode {
		m.sendIntent(intentMsg{Action: actionScroll, YOffset: off, Pinned: m.viewportPinned})
		return
	}
	writeSharedView(off, m.viewportPinned)
}

// spinnerFrameActive returns the focused-pane glyph (snowflake set).
// Caller picks active vs inactive so the focused card sparkles while
// background cards keep the calmer braille dots.
func (m teaModel) spinnerFrameActive() rune {
	if len(spinnerFramesActive) == 0 {
		return ' '
	}
	return spinnerFramesActive[m.spinFrame%len(spinnerFramesActive)]
}

// spinnerFrameInactive returns the background-pane glyph (braille set,
// matches Claude's own in-pane spinner).
func (m teaModel) spinnerFrameInactive() rune {
	if len(spinnerFramesInactive) == 0 {
		return ' '
	}
	return spinnerFramesInactive[m.spinFrame%len(spinnerFramesInactive)]
}

func newTeaModel() teaModel {
	ti := textinput.New()
	ti.Prompt = "/"
	ti.CharLimit = 256
	return teaModel{
		keys:           defaultKeyMap(),
		searchInput:    ti,
		unreadPanes:    make(map[string]bool),
		prevStatus:     make(map[string]string),
		blinkOn:        true,
		scrolloff:      configuredScrolloff(),
		hiddenSessions: loadHiddenSessions(),
	}
}

func (m teaModel) Init() tea.Cmd {
	if m.clientMode {
		// Thin client: the daemon owns all I/O. Run only local UI Cmds —
		// snapshot intake, animation ticks, and the one-shot focus probe.
		// No loadTree, no control conn, no git/usage watcher, and crucially
		// NO binary watcher (only the daemon re-execs on upgrade; clients
		// reconnect, which is what collapses the fleet re-exec burst to one).
		return tea.Batch(
			waitSnapshotCmd(m.snapshotCh),
			spinTickCmd(),
			blinkTickCmd(),
			initialFocusCmd(),
			standaloneFallbackCmd(),
		)
	}
	// Initial tick + first tree load + watcher boot. Each Cmd runs on its
	// own goroutine; the order Msgs arrive in is irrelevant because every
	// handler is idempotent w.r.t. the others.
	// initialFocusCmd seeds m.focused before the first FocusMsg/BlurMsg
	// arrives (tmux only emits one on transition, not on startup) so a
	// sidebar that boots already-focused gets the right initial state.
	return tea.Batch(
		// Boot-time load: hit shared state first (free) and fall back to
		// loadTree only if no leader has published yet. After hot-swap
		// re-exec, this avoids N sidebars all running loadTree at once.
		loadSharedRowsCmd(),
		// Boot-time view sync: peer instances may have a pinned scroll
		// offset; without this one-shot, the new sidebar shows its own
		// cursor-tracked viewport until the next peer write fires the
		// watcher. Emits as cursorChangedMsg so Update's existing apply
		// path runs.
		loadSharedSnapshotCmd(),
		tickCmd(),
		startContextWatcherCmd(),
		startGitWatcherCmd(),
		startCursorWatcherCmd(),
		spinTickCmd(),
		blinkTickCmd(),
		startBinaryWatcherCmd(),
		initialFocusCmd(),
		waitControlNotifyCmd(),
	)
}

func (m teaModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		// While typing in the search bar, every key (except Esc/Enter) is
		// fed to the input widget. Other keys still get the normal handler.
		if m.searchActive {
			return m.handleSearchKey(msg)
		}
		return m.handleKey(msg)

	case tea.MouseMsg:
		return m.handleMouse(msg)

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.viewport.Width = msg.Width
		m.viewport.Height = m.viewportHeight()
		m.searchInput.Width = msg.Width - 2 // leave room for "/" prompt
		m.refreshContent()
		return m, nil

	case snapshotMsg:
		return m.applySnapshot(msg.snap)

	case standaloneFallbackMsg:
		// Deadline reached with no daemon snapshot: the daemon can't start.
		// Quit so runDisplay hands off to the in-process engine. Harmless once
		// connected — gotSnapshot gates it, so a slow-but-alive daemon doesn't
		// trip the fallback.
		if m.clientMode && !m.gotSnapshot {
			standaloneRequested = true
			return m, tea.Quit
		}
		return m, nil

	case rowsLoadedMsg:
		// lastActivePaneID is sourced from shared-state (single writer in
		// writeSharedCursorActive). Per-instance derivation diverged across
		// sidebars depending on which instance saw which transition first.
		m.fullRows = msg.rows
		m.workspaces = summarizeWorkspaces(m.fullRows, m.hiddenSessions)
		m.rows = filterHiddenRows(m.fullRows, m.hiddenSessions)
		m.paneRows = paneRowsFor(m.rows)
		m.activePaneID = msg.activePaneID
		m.activeWindowID = msg.activeWindowID
		m.lastActivePaneID = msg.lastActivePaneID
		// Keep cursor on a live pane; first load picks the active one.
		m.cursorPaneID = reconcileSelectedPane(m.cursorPaneID, m.paneRows)
		if m.cursorPaneID == "" {
			m.cursorPaneID = m.activePaneID
		}
		m.applyStatusTransitions()
		paneCountChanged := len(m.paneRows) != m.lastPaneCount
		m.lastPaneCount = len(m.paneRows)
		if m.windowActive || paneCountChanged {
			m.refreshContent()
		}
		// Republish Rows to shared-state so cmdWindowName (catppuccin
		// window text formatter) can render the spinning arrow / ❗ icon
		// per window. Only the sidebar whose window is active writes —
		// hidden sidebars skip publishing entirely (their data came from
		// the leader's last write anyway, republishing it would just be
		// busywork) and avoid the 2 sidebarWindowIsActive forks/tick.
		if !m.windowActive {
			return m, nil
		}
		// Publish the UNFILTERED rows/paneRows: hiding a workspace is a
		// view-only filter for this sidebar, but the catppuccin window-tab
		// formatter (cmdWindowName, fed from shared PaneRows) must still show
		// every working/❗ window. Local nav uses the filtered m.paneRows.
		return m, publishRowsCmd(m.fullRows, paneRowsFor(m.fullRows), m.windowActive)

	case spinTickMsg:
		// Only repaint when at least one pane is actively animating —
		// composeStyledLines isn't free at 6.6 fps × N sidebars. The
		// active-card march also drives a redraw during its 1s sweep
		// (and the single tick that exits the sweep so the highlight
		// clears cleanly), but stays quiet during the 2s rest portion.
		//
		// windowActive guard: hidden sidebars push their View() into a
		// tmux pane nobody's looking at — the bytes sit in pane scrollback
		// until the user switches windows, at which point tmux replays the
		// LATEST frame anyway. Recomputing 6.6×/sec across 8 hidden peers
		// just burns CPU. The visible sidebar still animates.
		m.spinFrame++
		if m.windowActive && (hasStatus(m.rows, "running") || m.shouldRefreshForMarch()) {
			m.refreshContent()
		}
		return m, spinTickCmd()
	case blinkTickMsg:
		m.blinkOn = !m.blinkOn
		if m.windowActive && hasStatus(m.rows, "needs-input") {
			m.refreshContent()
		}
		return m, blinkTickCmd()

	case tickMsg:
		// Re-arm the tick and reload — refresh cadence is intentionally
		// driven by Update() (not a bare goroutine) so a slow loadTree
		// can't queue up overlapping reloads. checkBinaryMtimeCmd is the
		// fsnotify backstop: macOS drops bursty Create/Rename events on
		// some watchers, so this stat-based check guarantees a stale
		// sidebar swaps within ~1s of any install.
		//
		// Hidden-sidebar gating: profile shows the per-tick capture-pane
		// fork storm (~10 forks/sec) is the dominant CPU cost. Hidden
		// sidebars tick at 3s and skip loadTree. On window switch the
		// cursorChangedMsg handler re-checks visibility for instant refresh.
		m.scrolloff = configuredScrolloff()
		m.windowActive = sidebarWindowIsActive()
		windowActiveCache.Store(m.windowActive)
		// Re-sync viewport height with footer state. hasUsageFooter() flips
		// from false → true once startUsageRefresh produces its first
		// snapshot, but no message announces that transition. Without this
		// catch-up, View() starts appending the footer line while the
		// viewport still claims the full sidebar height — total render
		// overflows the pane by one row and tmux scrolls the top off.
		if h := m.viewportHeight(); h != m.viewport.Height {
			m.viewport.Height = h
			m.refreshContent()
		}
		interval := cfgRefreshSec()
		if !m.windowActive {
			interval = cfgHiddenRefreshSec()
		}
		cmds := []tea.Cmd{tickCmdWithInterval(interval), checkBinaryMtimeCmd()}
		// Leader pattern: only the active sidebar runs loadTree. Hidden
		// sidebars read shared state (no forks). cursorChangedMsg detects
		// hidden-to-visible edge for immediate refresh on window switch.
		if m.windowActive {
			cmds = append(cmds, loadTreeCmd())
		} else {
			cmds = append(cmds, loadSharedRowsCmd())
		}
		return m, tea.Batch(cmds...)

	case ctxWatcherReadyMsg:
		if msg.watcher == nil {
			return m, nil
		}
		m.ctxNotify = msg.notify
		return m, waitCtxCmd(msg.notify)

	case ctxChangedMsg:
		// Hidden sidebars skip the reload — they'd just throw away the
		// result on next tick anyway. Stay armed on the watcher so the
		// ctx burst doesn't get dropped.
		if !m.windowActive {
			return m, waitCtxCmd(m.ctxNotify)
		}
		return m, tea.Batch(loadTreeCmd(), waitCtxCmd(m.ctxNotify))

	case controlNotifyMsg:
		// tmux pushed a structural change (pane/window/session topology).
		// Leader reloads immediately instead of waiting out the 1s tick;
		// a hidden sidebar re-checks visibility so a window switch flips it
		// active without waiting for its slow 3s tick. Always re-arm.
		if m.windowActive {
			return m, tea.Batch(loadTreeCmd(), waitControlNotifyCmd())
		}
		if sidebarWindowIsActive() {
			m.windowActive = true
			windowActiveCache.Store(true)
			m.refreshContent()
			return m, tea.Batch(loadTreeCmd(), waitControlNotifyCmd())
		}
		return m, waitControlNotifyCmd()

	case cursorWatcherReadyMsg:
		if msg.notify == nil {
			return m, nil
		}
		m.cursorNotify = msg.notify
		return m, waitCursorCmd(msg.notify)

	case binaryChangedMsg:
		// Rebuild detected — re-exec ourselves. tmux pane keeps its
		// connection; the new binary takes over the alt-screen on
		// startup. tea.Quit ensures bubbletea unwinds the terminal
		// cleanly (cursor on, alt-screen off) before we Exec away,
		// otherwise the new process inherits a half-restored terminal.
		//
		// MUST set the re-exec flag SYNCHRONOUSLY here, not inside a
		// tea.Sequence(tea.Quit, fn) tail. tea.Quit terminates the
		// program loop and any subsequent commands in the sequence
		// silently never run — the previous shape lost reExecRequested
		// on every rebuild, so runBubble saw a clean exit, returned nil,
		// and the tmux pane died. After ~4 rebuilds in one session every
		// sidebar in the fleet vanished. Setting the flag here
		// guarantees runBubble's loop sees it once tea.Run returns.
		reExecRequested = true
		return m, tea.Quit

	case gitWatcherReadyMsg:
		return m, nil

	case initialFocusMsg:
		// Only seed; ignore once a real FocusMsg/BlurMsg has flipped state.
		// Cheap because Update() is single-threaded — first arrival wins.
		m.focused = msg.focused
		// cursorVisible stays false: focus alone doesn't reveal the cursor;
		// the user must press a nav key (matches user spec).
		m.refreshContent()
		return m, nil
	case tea.FocusMsg:
		m.focused = true
		// Client mode: the latest daemon snapshot already settled view state
		// into m.pinnedOffset/m.viewportPinned, so there's no file to re-read
		// and no cross-instance race to patch up — just repaint.
		if m.clientMode {
			m.refreshContent()
			return m, nil
		}
		// Race fix: a peer's last wheel may have written shared state but
		// the cursorChangedMsg is still queued behind this FocusMsg. The
		// view-apply path is gated on !m.focused so that queued msg would
		// then be skipped — user would land on this sidebar at a stale
		// viewport. Reading directly here uses the authoritative shared
		// file, so the just-focused user always sees the latest pinned
		// scroll position any sidebar agreed on.
		st := readSharedState()
		if st.ViewPinned != m.viewportPinned {
			m.viewportPinned = st.ViewPinned
		}
		if st.ViewPinned && st.ViewYOffset != m.pinnedOffset {
			m.pinnedOffset = st.ViewYOffset
			m.viewport.SetYOffset(m.pinnedOffset)
		}
		m.refreshContent()
		return m, nil
	case tea.BlurMsg:
		m.focused = false
		m.cursorVisible = false
		m.refreshContent()
		return m, nil

	case cursorChangedMsg:
		// Active pane mirrors immediately so peer sidebars repaint the
		// "▶" highlight as soon as a click lands — without waiting for
		// the 1s tickMsg / loadTreeCmd round-trip through tmux options.
		changed := false
		if msg.active != "" && msg.active != m.activePaneID {
			m.activePaneID = msg.active
			changed = true
		}
		if msg.activeWindow != "" && msg.activeWindow != m.activeWindowID {
			m.activeWindowID = msg.activeWindow
			changed = true
		}
		// LastActive comes from the same shared-state snapshot the
		// writer just published, so all sidebars render the ◂ on the
		// same card.
		if msg.last != m.lastActivePaneID {
			m.lastActivePaneID = msg.last
			changed = true
		}
		// Cursor only adopts when this sidebar isn't focused — focused
		// instance owns its own cursor. m.focused is fed by tea.FocusMsg
		// /BlurMsg + initialFocusMsg so we avoid a tmux roundtrip per
		// peer event.
		if !m.focused && msg.cursor != "" && msg.cursor != m.cursorPaneID {
			m.cursorPaneID = msg.cursor
			delete(m.unreadPanes, msg.cursor)
			changed = true
		}
		// View (scroll) state: skip here — applying view offsets from every
		// shared-state write is racy (publishRowsCmd's RMW can echo stale
		// viewPinned=false between the user's scroll and its sync write,
		// causing a visible bounce). View state is applied authoritatively
		// on FocusMsg, which is the only moment the user actually looks at
		// a different sidebar instance.
		// Hidden peers absorb the cursor/active mirror into in-memory state
		// (so the next visibility flip paints correctly) but skip the
		// repaint — same rationale as rowsLoadedMsg / spinTickMsg gates.
		if changed && m.windowActive {
			m.refreshContent()
		}
		// Status (green "working" border, "needs-input" peach blink, etc.)
		// is sourced from row.Status, which only refreshes on the 1s tick's
		// loadTreeCmd. Without this, switching tmux pane via hook → cursor
		// watcher updates m.activePaneID instantly but the new pane's
		// running/idle state can be up to 1s stale. Fire loadTreeCmd here
		// so capture-pane re-samples on focus change. Guarded by `changed`
		// so we don't loop with publishRowsCmd's own shared-state writes
		// (publish only touches Rows, not Active/Cursor/Last → changed=false).
		// windowActive guard mirrors the tickMsg leader pattern: hidden
		// sidebars skip the fork burst.
		if changed && m.windowActive {
			return m, tea.Batch(loadTreeCmd(), waitCursorCmd(m.cursorNotify))
		}
		// Hidden-to-visible edge: the slow 3s tick means a newly-visible
		// sidebar could wait up to 3s before detecting the window switch.
		// Re-check visibility here (1 tmux fork) so the user sees fresh
		// data within ~100ms of switching windows, not 3s.
		if changed && !m.windowActive {
			if sidebarWindowIsActive() {
				m.windowActive = true
				windowActiveCache.Store(true)
				m.refreshContent()
				return m, tea.Batch(loadTreeCmd(), waitCursorCmd(m.cursorNotify))
			}
		}
		return m, waitCursorCmd(m.cursorNotify)
	}
	return m, nil
}

// applySnapshot merges a daemon-pushed StateSnapshot into a thin client and
// re-arms the snapshot wait. It folds the rowsLoadedMsg + cursorChangedMsg
// apply paths into one: the daemon is the single writer, so there's no
// cross-instance race to guard against. clientMode only.
func (m teaModel) applySnapshot(s StateSnapshot) (tea.Model, tea.Cmd) {
	m.gotSnapshot = true // first snapshot clears the connecting spinner + arms the fallback no-op
	// Hidden set is authoritative from the daemon snapshot. Filter is view-only:
	// canonical s.Rows stays full so the menu can list hidden sessions.
	m.hiddenSessions = hiddenSliceToSet(s.HiddenSessions)
	m.fullRows = s.Rows
	m.workspaces = summarizeWorkspaces(m.fullRows, m.hiddenSessions)
	m.rows = filterHiddenRows(m.fullRows, m.hiddenSessions)
	m.paneRows = paneRowsFor(m.rows)
	m.activePaneID = s.Active
	m.activeWindowID = s.ActiveWindow
	m.lastActivePaneID = s.LastActive
	if s.Scrolloff > 0 {
		m.scrolloff = s.Scrolloff
	}
	// A focused client owns its own cursor (the user is navigating here); an
	// unfocused one mirrors the daemon's shared cursor.
	if !m.focused && s.Cursor != "" {
		m.cursorPaneID = s.Cursor
		delete(m.unreadPanes, s.Cursor)
	}
	m.cursorPaneID = reconcileSelectedPane(m.cursorPaneID, m.paneRows)
	if m.cursorPaneID == "" {
		m.cursorPaneID = m.activePaneID
	}
	// View scroll mirrors the daemon when unfocused; the focused client keeps
	// the offset it just scrolled to (its own intent already moved it, and the
	// echoed snapshot would otherwise bounce the viewport).
	if !m.focused {
		m.viewportPinned = s.ViewPinned
		m.pinnedOffset = s.ViewYOffset
		if s.ViewPinned {
			m.viewport.SetYOffset(s.ViewYOffset)
		}
	}
	// Usage rides the snapshot — only the daemon scans ~/.claude/projects.
	setUsage(s.Usage)
	m.applyStatusTransitions()
	// windowActive without a tmux fork: our window is active iff it holds the
	// focused pane the daemon reported. Gates the spin/blink repaints so a
	// hidden client doesn't recompute styled lines 6.6×/sec.
	m.windowActive = m.windowID != "" && m.windowID == s.ActiveWindow
	windowActiveCache.Store(m.windowActive)
	// Usage footer presence can change the viewport height; resync before
	// painting so the footer line doesn't overflow the pane by one row.
	if h := m.viewportHeight(); h != m.viewport.Height {
		m.viewport.Height = h
	}
	// Repaint when this sidebar's window is active, OR when the pane set
	// changed structurally (new card appeared / card removed). The structural
	// check guards against the stale-ActiveWindow race: after-new-window's
	// ensure re-selects panes in the old window, reverting ActiveWindow via
	// focus.sock. Without this fallback the VISIBLE sidebar thinks it's hidden
	// and freezes until the next manual window switch fixes ActiveWindow.
	// Cost: one len() compare per hidden sidebar per 1/s heartbeat (negligible).
	paneCountChanged := len(m.paneRows) != m.lastPaneCount
	m.lastPaneCount = len(m.paneRows)
	if m.windowActive || paneCountChanged {
		m.refreshContent()
	}
	return m, waitSnapshotCmd(m.snapshotCh)
}

// handleKey routes keyboard input. Returns the next model + Cmd.
func (m teaModel) handleKey(msg tea.KeyMsg) (teaModel, tea.Cmd) {
	// Workspace menu is modal: Esc/q close it, everything else is swallowed so
	// stray keys don't navigate the (hidden) card list underneath.
	if m.menuOpen {
		switch msg.String() {
		case "esc", "q":
			m.menuOpen = false
		}
		return m, nil
	}

	switch {
	case key.Matches(msg, m.keys.Quit):
		return m, tea.Quit

	case key.Matches(msg, m.keys.FocusMain):
		// ESC / Ctrl+L return focus to the main editing pane and snap the
		// cursor back to the active card so the next reveal starts where
		// the user actually is, not where they last browsed to.
		focusMainPane()
		if m.activePaneID != "" {
			m.cursorPaneID = m.activePaneID
			m.publishCursor(m.cursorPaneID)
		}
		m.cursorVisible = false
		m.viewportPinned = false
		m.syncSharedView()
		m.refreshContent()
		return m, nil

	case key.Matches(msg, m.keys.Down):
		m.moveCursor(+1)
		return m, nil
	case key.Matches(msg, m.keys.Up):
		m.moveCursor(-1)
		return m, nil

	case key.Matches(msg, m.keys.Top):
		// gg sequence — single g sets pending; second g jumps to top.
		if m.pendingG {
			m.pendingG = false
			m.jumpToCursor(0)
		} else {
			m.pendingG = true
			return m, nil
		}
		return m, nil
	case key.Matches(msg, m.keys.Bottom):
		m.pendingG = false
		if n := len(m.paneRows); n > 0 {
			m.jumpToCursor(n - 1)
		}
		return m, nil

	case key.Matches(msg, m.keys.HalfDown):
		m.viewportPinned = true
		m.viewport.HalfPageDown()
		m.pinnedOffset = m.viewport.YOffset
		m.syncSharedView()
		return m, nil
	case key.Matches(msg, m.keys.HalfUp):
		m.viewportPinned = true
		m.viewport.HalfPageUp()
		m.pinnedOffset = m.viewport.YOffset
		m.syncSharedView()
		return m, nil

	case key.Matches(msg, m.keys.Enter):
		m.pendingG = false
		// Selection committed → cursor is no longer "in flight". Hiding it
		// here (instead of waiting for BlurMsg from tmux when focus moves
		// to the chosen pane) avoids a 1-frame lag where the picked card
		// still wears the mauve cursor border on its way out.
		m.cursorVisible = false
		m.viewportPinned = false
		m.syncSharedView()
		cmd := m.focusCursorPaneCmd()
		m.refreshContent()
		return m, cmd

	case key.Matches(msg, m.keys.Search):
		m.pendingG = false
		m.beginSearch()
		return m, textinput.Blink

	case key.Matches(msg, m.keys.NextMatch):
		m.pendingG = false
		m.walkMatch(+1)
		return m, nil
	case key.Matches(msg, m.keys.PrevMatch):
		m.pendingG = false
		m.walkMatch(-1)
		return m, nil

	case key.Matches(msg, m.keys.SwitchLast):
		m.pendingG = false
		return m.switchToLastActive(), nil
	}

	// Pane-by-number jump (1-9). bubbles/key doesn't handle ranges natively
	// so we match the raw string here. Anything else: clear the gg pending.
	if s := msg.String(); len(s) == 1 && s[0] >= '1' && s[0] <= '9' {
		idx := int(s[0] - '1')
		var cmd tea.Cmd
		if idx < len(m.paneRows) {
			m.cursorVisible = true
			m.viewportPinned = false
			m.syncSharedView()
			m.cursorPaneID = m.paneRows[idx].PaneID
			cmd = m.focusCursorPaneCmd()
			m.refreshContent()
		}
		m.pendingG = false
		return m, cmd
	}

	m.pendingG = false
	return m, nil
}

// moveCursor advances/rewinds the cursor by delta panes, wrapping at edges
// the same way the tcell path does. Clearing unread on cursor move matches
// "looking at it" behavior — same convention as the tcell path.
func (m *teaModel) moveCursor(delta int) {
	if len(m.paneRows) == 0 {
		return
	}
	m.cursorVisible = true
	m.viewportPinned = false
	m.syncSharedView()
	idx := findPaneIndex(m.paneRows, m.cursorPaneID)
	idx = (idx + delta + len(m.paneRows)) % len(m.paneRows)
	m.cursorPaneID = m.paneRows[idx].PaneID
	delete(m.unreadPanes, m.cursorPaneID)
	// Broadcast the new cursor: a shared-state write (peer fsnotify picks it
	// up) in leader mode, or a daemon intent in clientMode.
	m.publishCursor(m.cursorPaneID)
	m.refreshContent()
}

// jumpToCursor sets cursor to paneRows[idx] and re-renders. Used by gg/G.
func (m *teaModel) jumpToCursor(idx int) {
	if idx < 0 || idx >= len(m.paneRows) {
		return
	}
	m.cursorVisible = true
	m.viewportPinned = false
	m.syncSharedView()
	m.cursorPaneID = m.paneRows[idx].PaneID
	m.refreshContent()
}

// focusCursorPaneCmd returns a Cmd that switches tmux focus off the main
// goroutine. All 4 tmux ops (switch-client, select-window, select-pane,
// set-option) are pipelined into one tmux command via `\;` — the previous
// 4-fork sequence cost ~1s on macOS because each fork+exec has ~50-100ms
// of overhead. One fork keeps the click feeling instant.
func (m *teaModel) focusCursorPaneCmd() tea.Cmd {
	if m.cursorPaneID == "" {
		return nil
	}
	var session, window string
	for _, row := range m.rows {
		if row.PaneID == m.cursorPaneID && (row.Kind == kindIntent || row.Kind == kindLocation) {
			session, window = row.Session, row.Window
			break
		}
	}
	pid := m.cursorPaneID
	m.activePaneID = pid // optimistic — refreshContent paints the new active card immediately
	m.activeWindowID = window
	// Focus runs LOCALLY in every mode, including the thin client. The display
	// client is pane-resident (live tty + $TMUX_PANE), so its forked
	// switch-client resolves deterministically to the user's attached client.
	// Delegating to the daemon broke this: the daemon is a detached process
	// holding a `tmux -C attach` control client, so its no-`-c` switch-client
	// picked a target by client-activity recency among many control clients —
	// usually NOT the user's terminal. The shared cursor/active write below is
	// the daemon's reload trigger (its fileCh watcher rebroadcasts), so every
	// client still converges on the new active card.
	return func() tea.Msg {
		args := make([]string, 0, 16)
		// Update main_pane FIRST so the focus hooks fired by switch-client /
		// select-window / select-pane below already see the new value and
		// short-circuit cmdOnFocus's redundant work (saves ~40 tmux calls
		// per click on the 5-hook fan-out path).
		args = append(args, "set-option", "-g", "@tmux_sidebar_main_pane", pid, ";")
		if session != "" {
			args = append(args, "switch-client", "-t", session, ";")
		}
		if window != "" {
			args = append(args, "select-window", "-t", window, ";")
		}
		args = append(args, "select-pane", "-t", pid)
		exec.Command(cachedLookPath("tmux"), args...).Run()
		// Write cursor AND active so peer sidebars can mirror the new
		// active card from the fsnotify event alone — no extra tmux
		// roundtrip needed in their Update path.
		writeSharedCursorActive(pid, pid, window)
		return nil
	}
}

// switchToLastActive toggles to the last-active Claude pane. Driven by the
// sidebar-switch-last script (send-keys'ing the reserved SwitchLast key) so
// leader+leader / prefix+Tab never forks the EDR-scanned Go binary.
//
// The switch runs SYNCHRONOUSLY here, not as a tea.Cmd: bubbletea runs Cmds in
// concurrent goroutines, so two rapid presses would race on the actual tmux
// switch and land non-deterministically. Doing it inline keeps back-to-back
// presses serialized by the single Update loop, which is what makes the
// a↔b toggle stable under input bursts — no flock needed.
func (m teaModel) switchToLastActive() teaModel {
	last := m.lastActivePaneID
	if last == "" || last == m.activePaneID {
		return m // nothing to toggle back to → stay put
	}
	// Resolve target session/window from rows. Absent → the pane was killed
	// (rows are the live set) → stay put rather than jumping elsewhere.
	var session, window string
	found := false
	for _, row := range m.rows {
		if row.PaneID == last && (row.Kind == kindIntent || row.Kind == kindLocation) {
			session, window, found = row.Session, row.Window, true
			break
		}
	}
	if !found {
		return m
	}

	prior := m.activePaneID
	args := []string{"set-option", "-g", "@tmux_sidebar_main_pane", last, ";"}
	if session != "" {
		args = append(args, "switch-client", "-t", session, ";")
	}
	if window != "" {
		args = append(args, "select-window", "-t", window, ";")
	}
	args = append(args, "select-pane", "-t", last)
	exec.Command(cachedLookPath("tmux"), args...).Run()

	// Promotes the prior active → LastActive under the shared-state lock
	// (no clobbering of concurrent leader writes). Mirror the result into the
	// model synchronously so a queued second press reads fresh state.
	writeSharedCursorActive(last, last, window)
	m.activePaneID = last
	m.activeWindowID = window
	m.lastActivePaneID = prior
	m.cursorPaneID = last
	m.cursorVisible = false
	m.viewportPinned = false
	m.syncSharedView()
	m.refreshContent()
	return m
}

// applyStatusTransitions detects running→idle/done transitions and marks the
// affected panes as unread (mirrors the tcell path's prevStatus logic). The
// currently-cursor'd pane is always cleared since the user is "looking at" it.
func (m *teaModel) applyStatusTransitions() {
	current := make(map[string]string)
	for _, row := range m.rows {
		if row.PaneID != "" && (row.Kind == kindLocation || row.Kind == kindIntent) && row.Status != "" {
			current[row.PaneID] = row.Status
		}
	}
	for paneID, status := range current {
		prev := m.prevStatus[paneID]
		m.prevStatus[paneID] = status
		// running → anything-but-running-or-needs-input == fresh "done" event
		if prev == "running" && status != "running" && status != "needs-input" {
			if paneID != m.cursorPaneID {
				m.unreadPanes[paneID] = true
			}
		}
	}
	delete(m.unreadPanes, m.cursorPaneID)
	// Mirror the leader-side clear: when a pane is the active tmux pane the
	// user has eyes on it, so any sticky unread badge should drop. Without
	// this the yellow band lingers indefinitely after the leader stops
	// emitting Status="done" — the row's Status flips back to "" but
	// unreadPanes never gets cleaned up.
	for _, row := range m.rows {
		if (row.Kind == kindLocation || row.Kind == kindIntent) && row.Active {
			delete(m.unreadPanes, row.PaneID)
		}
	}
}

// handleSearchKey processes keys while the search input has focus. Esc
// cancels the whole search; Enter dismisses the input but keeps the query
// (so the cursor stays on a match and n/N still work). Every other key is
// forwarded to the textinput, then matches are recomputed.
func (m teaModel) handleSearchKey(msg tea.KeyMsg) (teaModel, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		m.searchActive = false
		m.searchInput.Blur()
		m.searchInput.SetValue("")
		m.searchMatches = nil
		m.viewport.Height = m.viewportHeight()
		m.refreshContent()
		return m, nil
	case tea.KeyEnter:
		m.searchActive = false
		m.searchInput.Blur()
		m.viewport.Height = m.viewportHeight()
		m.refreshContent()
		return m, nil
	}

	var cmd tea.Cmd
	m.searchInput, cmd = m.searchInput.Update(msg)
	m.recomputeMatches()
	m.refreshContent()
	return m, cmd
}

// beginSearch flips into search-input mode. Existing query (if any) is
// cleared so the user starts fresh — matching the tcell path.
func (m *teaModel) beginSearch() {
	m.searchActive = true
	m.searchInput.SetValue("")
	m.searchInput.Focus()
	m.searchMatches = nil
	m.viewport.Height = m.viewportHeight()
}

// recomputeMatches rebuilds searchMatches from the current query and snaps
// the cursor to the next match if it isn't already on one.
func (m *teaModel) recomputeMatches() {
	q := m.searchInput.Value()
	if q == "" {
		m.searchMatches = nil
		return
	}
	m.searchMatches = findSearchMatches(m.rows, q)
	if len(m.searchMatches) == 0 {
		return
	}
	idx := findSelectedRowIndex(m.rows, m.cursorPaneID)
	if idx < 0 || !m.searchMatches[idx] {
		m.cursorPaneID = nextSearchMatch(m.rows, m.cursorPaneID, m.searchMatches, +1)
		m.viewportPinned = false
		m.syncSharedView()
	}
}

// walkMatch moves the cursor to the next/previous match using the existing
// nextSearchMatch helper. No-op if there are no matches yet.
func (m *teaModel) walkMatch(direction int) {
	if len(m.searchMatches) == 0 {
		return
	}
	m.cursorVisible = true
	m.viewportPinned = false
	m.syncSharedView()
	m.cursorPaneID = nextSearchMatch(m.rows, m.cursorPaneID, m.searchMatches, direction)
	m.refreshContent()
}

// viewportHeight returns the number of rows the viewport is allowed to use,
// reserving the bottom row for the search prompt when it's visible and one
// more for the API-value footer when usage stats have been computed.
func (m teaModel) viewportHeight() int {
	if m.height <= 0 {
		return 0
	}
	h := m.height
	if m.searchActive || m.searchInput.Value() != "" {
		h--
	}
	if m.hasUsageFooter() {
		h--
	}
	return max(0, h)
}

// hasUsageFooter reports whether the API-value footer is currently renderable
// at the sidebar's width. False until startUsageRefresh has produced a snapshot
// — this keeps the viewport at full height during the brief boot window.
func (m teaModel) hasUsageFooter() bool {
	return m.width > 0 && formatUsageFooter(m.width) != ""
}

// handleMouse routes mouse input. Wheel events scroll the viewport; left
// clicks on a card focus the corresponding pane. Click-on-press is good
// enough — the tcell path tracked release-at-same-position to work around
// tmux focus consumption, but bubbletea sees both press and release cleanly.
func (m teaModel) handleMouse(msg tea.MouseMsg) (teaModel, tea.Cmd) {
	// Right-click toggles the workspace show/hide menu. Pure Go: bubbletea
	// delivers the button via WithMouseCellMotion — no tmux binding, no fork.
	if msg.Button == tea.MouseButtonRight {
		if msg.Action == tea.MouseActionPress {
			m.menuOpen = !m.menuOpen
		}
		return m, nil
	}

	// While the menu is open, a left click on a workspace row toggles it; all
	// other clicks (title/separator/footer) are inert. The card list beneath
	// is unreachable until the menu closes.
	if m.menuOpen {
		if msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft {
			if idx := menuRowToWorkspace(msg.Y); idx >= 0 && idx < len(m.workspaces) {
				m.toggleHiddenSession(m.workspaces[idx].Name)
			}
		}
		return m, nil
	}

	switch msg.Button {
	case tea.MouseButtonWheelUp:
		m.viewportPinned = true
		m.viewport.ScrollUp(cfgMouseScrollLines())
		m.pinnedOffset = m.viewport.YOffset
		m.syncSharedView()
		return m, nil
	case tea.MouseButtonWheelDown:
		m.viewportPinned = true
		m.viewport.ScrollDown(cfgMouseScrollLines())
		m.pinnedOffset = m.viewport.YOffset
		m.syncSharedView()
		return m, nil
	}

	if msg.Action != tea.MouseActionPress || msg.Button != tea.MouseButtonLeft {
		return m, nil
	}

	rowIdx := msg.Y + m.viewport.YOffset
	if rowIdx < 0 || rowIdx >= len(m.rows) {
		return m, nil
	}

	pid := m.rows[rowIdx].PaneID
	if pid == "" {
		// Click landed on a border or session header — resolve to neighbor.
		pid = borderRowPaneID(m.rows[rowIdx], rowIdx, m.rows)
	}
	if pid == "" {
		return m, nil
	}

	m.cursorVisible = true
	m.viewportPinned = false
	m.syncSharedView()
	m.cursorPaneID = pid
	delete(m.unreadPanes, pid)
	cmd := m.focusCursorPaneCmd()
	m.refreshContent()
	return m, cmd
}

// runBubble is the bubbletea entry point. Loops on auto-reload — if the
// binary watcher requests a re-exec, we try `syscall.Exec` after bubbletea
// unwinds; if exec returns (i.e. fails), we fall back to starting a fresh
// bubbletea program so the tmux pane stays alive instead of dying. Without
// this loop, peer sidebars in other sessions could fail to exec (e.g. fs
// cache lag, codesign hiccup) and silently kill their own pane.
//
// Logging contract: every iteration writes a line to os.Stderr (now fd 2,
// see initLogging) describing how tea.Run exited. The panic guard below
// only catches panics on this calling goroutine — Cmd worker panics go
// through bubbletea's own recover, which writes the stack to fd 2 and
// returns ErrProgramPanic from tea.Run; that error path is logged by
// the per-iteration line. Naked goroutines (usage, context watcher,
// loadTree prefetch fan-outs) are NOT under this defer's umbrella — they
// each take their own recover via safeGo (see logging.go).
func runBubble() (retErr error) {
	if p, err := os.Executable(); err == nil {
		binaryPath = p
	}

	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "[%s] runBubble PANIC: %v\n%s\n",
				time.Now().Format("15:04:05"), r, debug.Stack())
			_ = os.Stderr.Sync()
			retErr = fmt.Errorf("panic: %v", r)
		}
	}()

	for {
		// Re-baseline the binary mtime each iteration. After a syscall.Exec
		// failure we fall through to a fresh tea.NewProgram instead of
		// dying — the previous mtime captured by the dead bubbletea would
		// otherwise still match what's on disk, never triggering re-exec
		// again, OR re-trigger immediately in a rebuild loop if codesign
		// hit twice. Clearing forces a clean re-baseline.
		resetStartupBinaryMtime()

		_, err := tea.NewProgram(
			newTeaModel(),
			tea.WithAltScreen(),
			tea.WithMouseCellMotion(),
			// Forwards CSI focus-in/out (ESC[I, ESC[O) from tmux as
			// tea.FocusMsg/BlurMsg. Requires `set -g focus-events on`
			// in tmux (already set in this repo's dotfiles/dot_tmux.conf).
			tea.WithReportFocus(),
		).Run()

		fmt.Fprintf(os.Stderr, "[%s] runBubble: tea.Run returned err=%v reExec=%v\n",
			time.Now().Format("15:04:05"), err, reExecRequested)
		_ = os.Stderr.Sync()

		if !reExecRequested {
			return err
		}
		reExecRequested = false

		// Tear down the control conn so the `tmux -C` child exits instead
		// of leaking — exec replaces our image but inherited pipe fds would
		// otherwise keep the old child alive while the new image spawns its
		// own. Re-execed image calls startControlConn() fresh.
		stopControlConn()

		// Hand off to the new binary now that bubbletea has restored
		// terminal modes. syscall.Exec only returns on failure; in that
		// case log + loop, never exit, so the pane survives.
		execErr := syscall.Exec(binaryPath, os.Args, os.Environ())
		fmt.Fprintf(os.Stderr, "[%s] runBubble: re-exec failed (%v); restarting bubbletea\n",
			time.Now().Format("15:04:05"), execErr)
		// Exec failed — we're staying in this process. Re-boot the control
		// conn we just tore down so the restarted bubbletea isn't stuck
		// forking every query.
		startControlConn()
	}
}

// reExecRequested signals runBubble to syscall.Exec into the new binary
// after tea.Run returns. Set synchronously in Update on binaryChangedMsg
// (see commit eb99071 — tea.Sequence(tea.Quit, fn) silently dropped the
// fn, so we set the flag inline now). Read by runBubble's loop after
// tea.Run returns, before deciding whether to exit or exec.
var reExecRequested bool

// standaloneRequested signals runDisplay to hand off to the in-process engine
// after the program quits. Set in Update on standaloneFallbackMsg when the
// daemon never delivered a snapshot before the deadline. Read once after
// prog.Run returns. See runDisplay + standaloneFallbackMsg.
var standaloneRequested bool
