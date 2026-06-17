package main

import (
	"io"
	"reflect"
	"strings"
	"testing"
	"time"
)

// feedTranscript runs a canned control-mode transcript through the parser
// and collects replies + notifications.
func feedTranscript(t *testing.T, transcript string) ([]controlReply, []controlNotification) {
	t.Helper()
	var replies []controlReply
	var notes []controlNotification
	p := &controlParser{
		onReply:  func(r controlReply) { replies = append(replies, r) },
		onNotify: func(n controlNotification) { notes = append(notes, n) },
	}
	for line := range strings.SplitSeq(transcript, "\n") {
		p.feed(line)
	}
	return replies, notes
}

func TestControlParserGreeting(t *testing.T) {
	// On attach, tmux emits an unsolicited empty %begin/%end pair plus
	// client/session notifications before any command is sent.
	transcript := strings.Join([]string{
		"%begin 1622298891 0 0",
		"%end 1622298891 0 0",
		"%client-session-changed /dev/ttys000 $0 main",
		"%session-changed $0 main",
	}, "\n")
	replies, notes := feedTranscript(t, transcript)

	if len(replies) != 1 {
		t.Fatalf("replies = %d, want 1", len(replies))
	}
	if replies[0].num != 0 || replies[0].isErr || len(replies[0].lines) != 0 {
		t.Errorf("greeting reply = %+v, want empty num=0 ok", replies[0])
	}
	wantNotes := []controlNotification{
		{name: "%client-session-changed", rest: "/dev/ttys000 $0 main"},
		{name: "%session-changed", rest: "$0 main"},
	}
	if !reflect.DeepEqual(notes, wantNotes) {
		t.Errorf("notes = %+v, want %+v", notes, wantNotes)
	}
}

func TestControlParserSimpleReply(t *testing.T) {
	transcript := strings.Join([]string{
		"%begin 1622298891 3 1",
		"0: main* (1 panes)",
		"1: work (2 panes)",
		"%end 1622298891 3 1",
	}, "\n")
	replies, _ := feedTranscript(t, transcript)

	if len(replies) != 1 {
		t.Fatalf("replies = %d, want 1", len(replies))
	}
	want := controlReply{num: 3, lines: []string{"0: main* (1 panes)", "1: work (2 panes)"}}
	if !reflect.DeepEqual(replies[0], want) {
		t.Errorf("reply = %+v, want %+v", replies[0], want)
	}
}

func TestControlParserErrorReply(t *testing.T) {
	transcript := strings.Join([]string{
		"%begin 1622298891 4 1",
		"unknown command: nope",
		"%error 1622298891 4 1",
	}, "\n")
	replies, _ := feedTranscript(t, transcript)

	if len(replies) != 1 {
		t.Fatalf("replies = %d, want 1", len(replies))
	}
	if !replies[0].isErr || replies[0].num != 4 {
		t.Errorf("reply = %+v, want isErr num=4", replies[0])
	}
	if len(replies[0].lines) != 1 || replies[0].lines[0] != "unknown command: nope" {
		t.Errorf("lines = %v", replies[0].lines)
	}
}

func TestControlParserPercentPayloadInsideBlock(t *testing.T) {
	// Payload lines starting with % (e.g. captured pane content) must NOT
	// be treated as notifications or block boundaries — only a matching-num
	// %end/%error closes the block.
	transcript := strings.Join([]string{
		"%begin 1622298891 7 1",
		"%output %1 sneaky pane content",
		"%end 1622298891 99 1",
		"100% done",
		"%end 1622298891 7 1",
	}, "\n")
	replies, notes := feedTranscript(t, transcript)

	if len(notes) != 0 {
		t.Errorf("notes = %+v, want none", notes)
	}
	if len(replies) != 1 {
		t.Fatalf("replies = %d, want 1", len(replies))
	}
	wantLines := []string{
		"%output %1 sneaky pane content",
		"%end 1622298891 99 1",
		"100% done",
	}
	if !reflect.DeepEqual(replies[0].lines, wantLines) {
		t.Errorf("lines = %v, want %v", replies[0].lines, wantLines)
	}
}

func TestControlParserNotificationsBetweenBlocks(t *testing.T) {
	transcript := strings.Join([]string{
		"%begin 1622298891 1 1",
		"out-a",
		"%end 1622298891 1 1",
		"%window-pane-changed @1 %42",
		"%output %3 some\\040escaped\\040bytes",
		"%begin 1622298892 2 1",
		"out-b",
		"%end 1622298892 2 1",
	}, "\n")
	replies, notes := feedTranscript(t, transcript)

	if len(replies) != 2 {
		t.Fatalf("replies = %d, want 2", len(replies))
	}
	if replies[0].num != 1 || replies[1].num != 2 {
		t.Errorf("reply nums = %d,%d want 1,2", replies[0].num, replies[1].num)
	}
	wantNotes := []controlNotification{
		{name: "%window-pane-changed", rest: "@1 %42"},
		{name: "%output", rest: "%3 some\\040escaped\\040bytes"},
	}
	if !reflect.DeepEqual(notes, wantNotes) {
		t.Errorf("notes = %+v, want %+v", notes, wantNotes)
	}
}

func TestControlParserEmptyAndStrayLines(t *testing.T) {
	transcript := strings.Join([]string{
		"",                  // blank outside block: dropped
		"stray noise",       // non-% outside block: dropped
		"%begin 1 5 1",
		"",                  // blank inside block: payload
		"%end 1 5 1",
	}, "\n")
	replies, notes := feedTranscript(t, transcript)

	if len(notes) != 0 {
		t.Errorf("notes = %+v, want none", notes)
	}
	if len(replies) != 1 {
		t.Fatalf("replies = %d, want 1", len(replies))
	}
	if !reflect.DeepEqual(replies[0].lines, []string{""}) {
		t.Errorf("lines = %q, want one empty line", replies[0].lines)
	}
}

func TestQuoteTmuxArgs(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"display-message", "-p", "-t", "%3", "#{window_active}"},
			`display-message -p -t %3 '#{window_active}'`},
		{[]string{"list-panes", "-a", "-F", "#{pane_id}|#{pane_title}"},
			`list-panes -a -F '#{pane_id}|#{pane_title}'`},
		{[]string{"select-pane", "-t", "@1"}, "select-pane -t @1"},
		{[]string{"set-option", "-g", "@opt", "a b c"},
			`set-option -g @opt 'a b c'`},
		{[]string{"rename", "it's mine"}, `rename 'it'\''s mine'`},
		{[]string{"x", ""}, "x ''"},
	}
	for _, c := range cases {
		if got := quoteTmuxArgs(c.args); got != c.want {
			t.Errorf("quoteTmuxArgs(%q) = %q, want %q", c.args, got, c.want)
		}
	}
}

func TestRouteReplyFIFO(t *testing.T) {
	tc := &tmuxControl{ready: make(chan struct{}), done: make(chan struct{})}

	// First block seen is the greeting — releases ready, pops nothing.
	tc.routeReply(controlReply{lines: []string{"greeting"}})
	select {
	case <-tc.ready:
	default:
		t.Fatal("greeting did not close ready")
	}

	ch1 := make(chan controlReply, 1)
	ch2 := make(chan controlReply, 1)
	tc.pending = []chan controlReply{ch1, ch2}

	// Replies arrive in send order; numbers are ignored (server-wide,
	// unpredictable) — pure FIFO routing.
	tc.routeReply(controlReply{num: 9999, lines: []string{"first"}})
	tc.routeReply(controlReply{num: 7, lines: []string{"second"}})
	if got := (<-ch1).lines[0]; got != "first" {
		t.Errorf("ch1 = %q", got)
	}
	if got := (<-ch2).lines[0]; got != "second" {
		t.Errorf("ch2 = %q", got)
	}
}

func TestRouteReplyEmptyQueue(t *testing.T) {
	tc := &tmuxControl{ready: make(chan struct{}), done: make(chan struct{})}
	// Greeting then a stray block with no waiter — must not panic.
	tc.routeReply(controlReply{lines: []string{"greeting"}})
	tc.routeReply(controlReply{lines: []string{"orphan"}})
}

// newPipedControl builds a tmuxControl whose stdin drains to a captured
// buffer, with no real tmux child — exercises run() routing in isolation.
// ready is pre-closed since these tests skip the attach handshake.
func newPipedControl(t *testing.T) (*tmuxControl, *io.PipeReader) {
	t.Helper()
	pr, pw := io.Pipe()
	ready := make(chan struct{})
	close(ready)
	tc := &tmuxControl{stdin: pw, ready: ready, greeted: true, done: make(chan struct{})}
	return tc, pr
}

func TestRunDeliversReply(t *testing.T) {
	tc, pr := newPipedControl(t)
	go io.Copy(io.Discard, pr) // drain writes

	go func() {
		// Wait for run() to register the pending request, then reply.
		for {
			tc.mu.Lock()
			n := len(tc.pending)
			tc.mu.Unlock()
			if n > 0 {
				break
			}
			time.Sleep(time.Millisecond)
		}
		tc.routeReply(controlReply{num: 1, lines: []string{"0: main"}})
	}()

	out, err := tc.run("list-sessions")
	if err != nil {
		t.Fatalf("run err: %v", err)
	}
	if out != "0: main" {
		t.Errorf("out = %q", out)
	}
}

func TestRunErrorReply(t *testing.T) {
	tc, pr := newPipedControl(t)
	go io.Copy(io.Discard, pr)
	go func() {
		for {
			tc.mu.Lock()
			n := len(tc.pending)
			tc.mu.Unlock()
			if n > 0 {
				break
			}
			time.Sleep(time.Millisecond)
		}
		tc.routeReply(controlReply{num: 1, isErr: true, lines: []string{"bad"}})
	}()

	if _, err := tc.run("nope"); err == nil {
		t.Fatal("want error, got nil")
	}
}

func TestRunClosedConn(t *testing.T) {
	tc, pr := newPipedControl(t)
	go io.Copy(io.Discard, pr)
	tc.fail()
	if _, err := tc.run("list-sessions"); err == nil {
		t.Fatal("want error on closed conn")
	}
}

func TestBlockBoundaryMalformed(t *testing.T) {
	cases := []string{
		"%begin",
		"%begin 123",
		"%begin abc def",
		"%beginning 1 2 3",
	}
	for _, line := range cases {
		if _, ok := blockBoundary(line, "%begin "); ok {
			t.Errorf("blockBoundary(%q) = ok, want reject", line)
		}
	}
	if num, ok := blockBoundary("%begin 1622298891 3 1", "%begin "); !ok || num != 3 {
		t.Errorf("blockBoundary valid = (%d,%v), want (3,true)", num, ok)
	}
}
