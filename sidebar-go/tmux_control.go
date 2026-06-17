package main

import (
	"bufio"
	"errors"
	"expvar"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// errControlDead marks a query that failed because the control connection
// is gone (closed/timeout/EOF), not because tmux rejected the command.
// tmuxQuery forks a one-shot retry only for this case — a genuine tmux
// error (e.g. unset option) would just recur on the fork.
var errControlDead = errors.New("tmux control conn dead")

// tmux control mode (`tmux -C attach-session`): one persistent child
// replaces a fork+exec per query. Commands go in on stdin one per line;
// replies come back wrapped in "%begin <ts> <num> <flags>" ...payload...
// "%end|%error <ts> <num> <flags>". Replies arrive strictly in send
// order. Async notifications (%output, %window-pane-changed, ...) only
// appear outside begin/end blocks.

// controlReply is one completed %begin..%end/%error block.
type controlReply struct {
	num   int
	lines []string
	isErr bool
}

// controlNotification is an async %-line outside any block.
type controlNotification struct {
	name string // "%output", "%window-pane-changed", ...
	rest string // remainder after the name, without leading space
}

// controlParser is a pure line-fed state machine over control-mode
// stdout. No I/O — unit-testable with canned transcripts.
type controlParser struct {
	inBlock bool
	num     int
	lines   []string

	onReply  func(controlReply)
	onNotify func(controlNotification)
}

// blockBoundary parses "%begin/%end/%error <ts> <num> <flags>" lines,
// returning the command number.
func blockBoundary(line, prefix string) (int, bool) {
	rest, ok := strings.CutPrefix(line, prefix)
	if !ok {
		return 0, false
	}
	fields := strings.Fields(rest)
	if len(fields) < 2 {
		return 0, false
	}
	num, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, false
	}
	return num, true
}

func (p *controlParser) feed(line string) {
	if p.inBlock {
		// Only a matching-num %end/%error closes the block; any other
		// line — including ones starting with % — is reply payload.
		if num, ok := blockBoundary(line, "%end "); ok && num == p.num {
			p.finish(false)
			return
		}
		if num, ok := blockBoundary(line, "%error "); ok && num == p.num {
			p.finish(true)
			return
		}
		p.lines = append(p.lines, line)
		return
	}

	if num, ok := blockBoundary(line, "%begin "); ok {
		p.inBlock = true
		p.num = num
		p.lines = nil
		return
	}
	if strings.HasPrefix(line, "%") {
		if p.onNotify != nil {
			name, rest, _ := strings.Cut(line, " ")
			p.onNotify(controlNotification{name: name, rest: rest})
		}
		return
	}
	// Stray non-% line outside a block: protocol noise, drop.
}

func (p *controlParser) finish(isErr bool) {
	reply := controlReply{num: p.num, lines: p.lines, isErr: isErr}
	p.inBlock = false
	p.lines = nil
	if p.onReply != nil {
		p.onReply(reply)
	}
}

// tmuxControl owns the control-mode child process and its reader, and
// multiplexes synchronous request/reply over the single pipe.
//
// Routing is pure FIFO: tmux command-numbers in %begin are a server-wide
// counter we can't predict (observed jumping 1055→1062→1069), so we don't
// match on them. Replies arrive strictly in send order, so the oldest
// pending request always owns the next reply block. The one wrinkle is the
// unsolicited greeting block tmux emits on attach (before any command) —
// `ready` gates sends until that block is seen, so no request is ever in
// flight when the greeting lands.
type tmuxControl struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser

	ready chan struct{} // closed once the attach greeting block is consumed

	mu      sync.Mutex
	closed  bool
	greeted bool
	pending []chan controlReply // FIFO: replies arrive in send order

	// done is closed when the reader goroutine exits (EOF = tmux gone).
	done chan struct{}
}

// controlTimeout bounds a single request. A blown timeout means the FIFO
// is desynced (a reply went missing); we kill the conn rather than risk
// routing this request's late reply to the next caller.
const controlTimeout = 2 * time.Second

// tmuxServerArgs derives the `-S <socket>` selector for the server this
// process is attached to, from $TMUX (format: "<socket>,<pid>,<session>").
// We must target this socket explicitly because the child has TMUX
// stripped from its env (nesting guard) and would otherwise fall back to
// the default socket — wrong whenever the user runs tmux with -L/-S.
func tmuxServerArgs() []string {
	t := os.Getenv("TMUX")
	if t == "" {
		return nil
	}
	sock, _, _ := strings.Cut(t, ",")
	if sock == "" {
		return nil
	}
	return []string{"-S", sock}
}

// startTmuxControl spawns a control-mode client attached to this process's
// tmux server and starts the reader. onNotify fires on the reader
// goroutine for async %-lines — keep it fast.
func startTmuxControl(onNotify func(controlNotification)) (*tmuxControl, error) {
	return startTmuxControlOn(tmuxServerArgs(), onNotify)
}

// startTmuxControlOn is startTmuxControl with an explicit server selector
// (e.g. ["-S", path] or ["-L", name]); the integration test uses it to
// target a throwaway server.
func startTmuxControlOn(serverArgs []string, onNotify func(controlNotification)) (*tmuxControl, error) {
	// no-output: suppress %output stream until we opt back in (phase 4.7);
	// a chatty pane would otherwise flood the pipe at attach.
	args := append([]string{}, serverArgs...)
	args = append(args, "-C", "attach-session", "-f", "no-output")
	cmd := exec.Command(cachedLookPath("tmux"), args...)
	// Drop TMUX from env or attach refuses (nesting guard).
	env := os.Environ()
	filtered := env[:0]
	for _, kv := range env {
		if !strings.HasPrefix(kv, "TMUX=") {
			filtered = append(filtered, kv)
		}
	}
	cmd.Env = filtered
	cmd.Stderr = nil

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		stdin.Close()
		return nil, err
	}

	tc := &tmuxControl{
		cmd:   cmd,
		stdin: stdin,
		ready: make(chan struct{}),
		done:  make(chan struct{}),
	}
	safeGo("tmuxControl.reader", func() {
		defer close(tc.done)
		defer cmd.Wait() // reap; runs after scanner sees EOF
		parser := &controlParser{onReply: tc.routeReply, onNotify: onNotify}
		sc := bufio.NewScanner(stdout)
		// capture-pane payload lines can be long; default 64KB token cap
		// kills the scanner mid-stream and looks like a dead conn.
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			parser.feed(sc.Text())
		}
		if err := sc.Err(); err != nil {
			debugLog("tmux-control: reader: %v", err)
		}
	})
	return tc, nil
}

// routeReply delivers a completed reply block to the oldest pending
// request (pure FIFO). The first block ever seen is the unsolicited attach
// greeting — it has no waiter, so it just flips ready to release run().
func (tc *tmuxControl) routeReply(r controlReply) {
	tc.mu.Lock()
	if !tc.greeted {
		tc.greeted = true
		tc.mu.Unlock()
		close(tc.ready)
		return
	}
	if len(tc.pending) == 0 {
		tc.mu.Unlock()
		return // no waiter (shouldn't happen post-greeting) — drop
	}
	ch := tc.pending[0]
	tc.pending = tc.pending[1:]
	tc.mu.Unlock()
	ch <- r // buffered(1) — never blocks the reader
}

// run sends a tmux command and waits for its reply. Returns trimmed
// stdout, or an error on tmux error / timeout / dead conn. On timeout it
// kills the conn (FIFO is unrecoverable once a reply is lost).
func (tc *tmuxControl) run(args ...string) (string, error) {
	// Block until the attach greeting is consumed, so this request is never
	// queued while the greeting block is still in flight (which would pop
	// it off the FIFO as a phantom reply).
	select {
	case <-tc.ready:
	case <-tc.done:
		return "", errControlDead
	case <-time.After(controlTimeout):
		tc.fail()
		return "", fmt.Errorf("%w: attach timeout", errControlDead)
	}

	line := quoteTmuxArgs(args)
	ch := make(chan controlReply, 1)

	tc.mu.Lock()
	if tc.closed {
		tc.mu.Unlock()
		return "", errControlDead
	}
	tc.pending = append(tc.pending, ch)
	_, err := io.WriteString(tc.stdin, line+"\n")
	tc.mu.Unlock()
	if err != nil {
		tc.fail()
		return "", fmt.Errorf("%w: %v", errControlDead, err)
	}

	select {
	case r := <-ch:
		out := strings.Join(r.lines, "\n")
		if r.isErr {
			// tmux ran the command and rejected it — a real error, not a
			// dead conn. Return as-is; a fork retry would just recur.
			return "", fmt.Errorf("tmux: %s", out)
		}
		return strings.TrimRight(out, "\n"), nil
	case <-tc.done:
		return "", errControlDead
	case <-time.After(controlTimeout):
		tc.fail()
		return "", fmt.Errorf("%w: reply timeout", errControlDead)
	}
}

// fail tears the conn down and unblocks every pending request. Used on
// timeout or write error — the reconnect path (4.8) rebuilds from scratch.
// Called from run() on a caller goroutine, so it must not block: it closes
// stdin (best-effort graceful detach) and hands off to a watchdog that
// force-kills the child if EOF never comes. A wedged single-threaded tmux
// server won't process our detach, so without the kill the `tmux -C` child
// orphans and keeps a server pipe open => whole-server hang.
func (tc *tmuxControl) fail() {
	tc.mu.Lock()
	if tc.closed {
		tc.mu.Unlock()
		return
	}
	tc.closed = true
	tc.stdin.Close()
	tc.pending = nil
	tc.mu.Unlock()
	safeGo("tmuxControl.failReap", func() { tc.reap(controlTimeout) })
}

// reap blocks until the reader goroutine exits (child fully gone),
// SIGKILLing the child if it hasn't exited within grace. Safe to call
// concurrently from fail()'s watchdog and Close(): Kill on an already-reaped
// pid and a second receive on the closed done channel are both harmless.
func (tc *tmuxControl) reap(grace time.Duration) {
	select {
	case <-tc.done:
	case <-time.After(grace):
		tc.cmd.Process.Kill()
		<-tc.done
	}
}

// tmuxSafeArg matches arguments that need no quoting in tmux's command
// lexer: flags, pane/window ids (%3, @1, $0), paths, numbers, csv lists.
var tmuxSafeArg = regexp.MustCompile(`^[A-Za-z0-9_%@$:=,./~+-]+$`)

// quoteTmuxArgs joins args into one control-mode command line, single-
// quoting anything with lexer-special characters (spaces, ';', format
// braces in "#{...}"). Single-quoted content is literal to tmux, which is
// exactly what format strings and titles need.
func quoteTmuxArgs(args []string) string {
	parts := make([]string, len(args))
	for i, a := range args {
		if a != "" && tmuxSafeArg.MatchString(a) {
			parts[i] = a
		} else {
			parts[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
	}
	return strings.Join(parts, " ")
}

// Close shuts the connection down and blocks until the child is reaped.
// stdin EOF makes tmux detach the control client; if the reader hasn't exited
// shortly after, SIGKILL. Used on exit and before re-exec: we must not
// syscall.Exec while a `tmux -C` child still holds a server pipe. Idempotent —
// if fail() already closed stdin (marking closed=true), we still wait/kill
// here rather than short-circuiting, so a timed-out conn can't orphan its
// child across a hot-swap.
func (tc *tmuxControl) Close() {
	tc.mu.Lock()
	if !tc.closed {
		tc.closed = true
		tc.stdin.Close()
		tc.pending = nil
	}
	tc.mu.Unlock()
	tc.reap(2 * time.Second)
}

// Done reports reader exit (EOF on the control conn — tmux restarted or
// the child died).
func (tc *tmuxControl) Done() <-chan struct{} {
	return tc.done
}

// globalTmuxControl holds the interactive process's persistent control
// connection, when one is up. nil in subcommand invocations and before
// the conn boots.
var globalTmuxControl atomic.Pointer[tmuxControl]

// Fallback diagnostics (published on /debug/vars). tmuxFallbacks counts
// control-conn queries that errored and forked instead; tmuxLastFallback
// records the most recent failing command + error for debugging.
var (
	tmuxFallbacks    = expvar.NewInt("tmux_control_fallbacks")
	tmuxLastFallback = expvar.NewString("tmux_control_last_fallback")
)

// tmuxQuery runs a read-only tmux query over the persistent control conn
// when available, falling back to a one-shot fork (runTmux) when the conn
// is absent or errored. Callers must pass explicit -t targets: the control
// client is not the "current" client, so target-less commands resolve
// against the wrong context.
func tmuxQuery(args ...string) (string, error) {
	if tc := globalTmuxControl.Load(); tc != nil {
		out, err := tc.run(args...)
		if err == nil {
			return out, nil
		}
		if !errors.Is(err, errControlDead) {
			// tmux rejected the command (e.g. unset option) — forking would
			// just reproduce the same error. Return it; callers that treat
			// errors as "" (tmuxOption) behave exactly as the old fork path.
			return "", err
		}
		// Conn is genuinely down: fork a one-shot so a dead/reconnecting
		// control conn never breaks a query the old path could answer.
		tmuxFallbacks.Add(1)
		tmuxLastFallback.Set(strings.Join(args, " ") + " :: " + err.Error())
	}
	return runTmux(args...)
}

// controlNotify is a coalescing doorbell: the reader goroutine rings it
// when tmux pushes a structural-change notification, and waitControlNotify
// Cmd drains it to trigger a fresh loadTree faster than the 1s poll.
// Buffered(1) + non-blocking send = "something changed since last drain".
var controlNotify = make(chan struct{}, 1)

// controlDirtyNotifications are the async %-lines that mean the pane/window
// tree (or which window is active) may have changed — worth a reload. We
// ignore the rest (%output handled separately in 4.7, %client-detached, …).
var controlDirtyNotifications = map[string]bool{
	"%window-pane-changed":    true, // active pane moved within a window
	"%session-window-changed": true, // session's current window changed
	"%client-session-changed": true, // a client switched session
	"%session-changed":        true,
	"%sessions-changed":       true,
	"%window-add":             true,
	"%window-close":           true,
	"%unlinked-window-add":    true,
	"%unlinked-window-close":  true,
	"%window-renamed":         true,
	"%layout-change":          true, // pane resize/split → width changed
}

// dispatchControlNotify runs on the reader goroutine — must not block.
func dispatchControlNotify(n controlNotification) {
	if !controlDirtyNotifications[n.name] {
		return
	}
	select {
	case controlNotify <- struct{}{}:
	default: // already pending — coalesce
	}
}

// controlStopCh stops the current supervisor goroutine. Each
// startControlConn installs a fresh channel; stopControlConn closes it.
// Per-supervisor (not a shared flag) so a restart after a failed re-exec
// can't accidentally keep an old supervisor alive.
var (
	controlStopMu sync.Mutex
	controlStopCh chan struct{}
)

// controlBackoffMax caps the reconnect backoff after a tmux server restart.
const (
	controlBackoffInit = 250 * time.Millisecond
	controlBackoffMax  = 5 * time.Second
)

// startControlConn launches the control-connection supervisor for
// interactive mode. The supervisor dials, publishes the conn to
// globalTmuxControl, and redials with backoff whenever it dies (tmux
// restart). tmuxQuery forks in the gaps, so queries never block on it.
func startControlConn() {
	controlStopMu.Lock()
	stop := make(chan struct{})
	controlStopCh = stop
	controlStopMu.Unlock()
	safeGo("tmuxControl.supervisor", func() { superviseControlConn(stop) })
}

// stopControlConn tears down the control connection + its supervisor
// (before re-exec or on exit) so neither the `tmux -C` child nor the
// reconnect loop leaks across hot-swaps.
func stopControlConn() {
	controlStopMu.Lock()
	if controlStopCh != nil {
		close(controlStopCh)
		controlStopCh = nil
	}
	controlStopMu.Unlock()
	if tc := globalTmuxControl.Swap(nil); tc != nil {
		tc.Close()
	}
}

// superviseControlConn keeps a live control connection until stopped.
// Dies-fast connections (tmux still down) grow the backoff; a connection
// that lived a while resets it.
func superviseControlConn(stop chan struct{}) {
	backoff := controlBackoffInit
	for {
		select {
		case <-stop:
			return
		default:
		}
		tc, err := startTmuxControl(dispatchControlNotify)
		if err != nil {
			debugLog("tmux-control: dial: %v", err)
			select {
			case <-stop:
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, controlBackoffMax)
			continue
		}
		globalTmuxControl.Store(tc)
		debugLog("tmux-control: connected")
		backoff = controlBackoffInit
		connectedAt := time.Now()

		select {
		case <-tc.Done():
		case <-stop:
			tc.Close()
			return
		}
		globalTmuxControl.CompareAndSwap(tc, nil)
		debugLog("tmux-control: conn died, reconnecting")
		// A conn that barely lived means tmux is still flapping — back off.
		if time.Since(connectedAt) < 2*time.Second {
			backoff = min(backoff*2, controlBackoffMax)
		}
		select {
		case <-stop:
			return
		case <-time.After(backoff):
		}
	}
}
