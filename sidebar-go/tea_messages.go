package main

// Bubbletea Msg types for the sidebar runtime. Kept in their own file so the
// dispatch surface stays scannable as Update() grows.

// rowsLoadedMsg carries the result of loadTree (or its faster variant). The
// Cmd that produced it ran on a worker goroutine; Update() owns the merge.
type rowsLoadedMsg struct {
	rows             []Row
	activePaneID     string
	activeWindowID   string
	lastActivePaneID string
}

// tickMsg fires from tea.Tick on the refresh cadence (~1s). Triggers a tree
// reload to catch panes/state changes that hooks may have missed.
type tickMsg struct{}

// ctxWatcherReadyMsg delivers the singleton fsnotify watcher and its notify
// channel from the bootstrap Cmd back to Update(), where the watch loop is
// converted into a long-lived Cmd. We pass the channel by value so Update()
// can re-Cmd it without grabbing the watcher object every time.
type ctxWatcherReadyMsg struct {
	watcher *ContextWatcher
	notify  <-chan struct{}
}

// ctxChangedMsg fires when the context fsnotify channel signals an update.
// Triggers a fresh loadTree so intent/usage rows reflect the new file.
type ctxChangedMsg struct{}

// controlNotifyMsg fires when the tmux control conn pushes a structural
// change (pane/window/session topology). Lets the leader reload faster
// than the 1s poll, and lets a hidden sidebar notice a window switch
// without waiting for its slow 3s tick.
type controlNotifyMsg struct{}

// spinTickMsg drives the working-pane spinner frame counter. Emitted on a
// ~150ms cadence — fast enough to feel animated, slow enough that we don't
// burn CPU when nothing is happening.
type spinTickMsg struct{}

// blinkTickMsg toggles needs-input border peach. ~700ms cadence matches the
// classic renderer so users don't notice the rewrite.
type blinkTickMsg struct{}

// cursorWatcherReadyMsg hands the directory watcher's notify channel back to
// Update() so the watch loop can be re-armed without re-creating the watcher.
type cursorWatcherReadyMsg struct {
	notify <-chan sharedStateSnapshot
}

// sharedStateSnapshot is the subset of shared-state fields peer sidebars need
// to mirror cross-instance focus changes without waiting for the 1s tick.
// viewYOffset/viewPinned propagate scroll position so switching tmux
// sessions doesn't leave the user at a different viewable region.
type sharedStateSnapshot struct {
	cursor       string
	active       string
	activeWindow string
	last         string
	viewYOffset  int
	viewPinned   bool
}

// cursorChangedMsg carries the freshly-read cursor + active + last-active
// pane IDs from the shared state file. Update() decides whether to apply
// them locally. viewYOffset/viewPinned mirror sharedStateSnapshot.
type cursorChangedMsg struct {
	cursor       string
	active       string
	activeWindow string
	last         string
	viewYOffset  int
	viewPinned   bool
}

// binaryChangedMsg fires when the rebuild watcher's fsnotify Cmd sees an
// atomic rename of the live binary (the install pattern is build-to-tmp +
// mv-into-place). Update() responds by syscall.Exec'ing the new binary so
// the sidebar doesn't need a manual kill+respawn after a rebuild.
type binaryChangedMsg struct{}

// initialFocusMsg seeds m.focused on startup. tmux only emits CSI focus
// sequences on transitions, so a sidebar that boots already-focused would
// otherwise sit at focused=false until the user clicks away and back.
type initialFocusMsg struct {
	focused bool
}

// snapshotMsg carries one daemon-pushed StateSnapshot to a display thin
// client (clientMode). It replaces the rowsLoadedMsg + cursorChangedMsg
// pair that the in-process engine produces: the daemon is the single writer,
// so the client just merges and renders. See display.go.
type snapshotMsg struct {
	snap StateSnapshot
}

// standaloneFallbackMsg fires once, displayStandaloneDeadline after a thin
// client boots. If no daemon snapshot has arrived by then the client gives up
// on the daemon and re-runs as a full in-process engine, so a pane never sticks
// on the connecting spinner when the daemon truly can't start. A no-op once a
// snapshot has been seen. See runDisplay.
type standaloneFallbackMsg struct{}
