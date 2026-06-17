package main

import "testing"

func TestParseAttachedTerminalWindow(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			// The live freeze case: one real terminal on @119, three
			// control-mode clients (two orphans + daemon conn) on other
			// windows. Must pick the real terminal's window, ignoring the
			// control clients whose windows are also session-active.
			name: "real terminal wins over control clients",
			raw: "1781547602|0|@119|attached,focused,UTF-8\n" +
				"1781417897|1|@119|attached,focused,control-mode,no-output,UTF-8\n" +
				"1781546190|1|@117|attached,focused,control-mode,no-output,UTF-8",
			want: "@119",
		},
		{
			name: "most-recently-active terminal wins (multi-monitor)",
			raw: "100|0|@10|attached,focused\n" +
				"200|0|@20|attached,focused",
			want: "@20",
		},
		{
			// All clients control-mode → no real terminal → caller falls
			// back to the per-session membership self-heal.
			name: "no real terminal returns empty",
			raw: "1781417897|1|@119|attached,control-mode,no-output\n" +
				"1781546190|1|@117|attached,control-mode,no-output",
			want: "",
		},
		{
			name: "detached terminal ignored",
			raw:  "100|0|@10|UTF-8",
			want: "",
		},
		{
			name: "empty input",
			raw:  "",
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseAttachedTerminalWindow(tt.raw); got != tt.want {
				t.Errorf("parseAttachedTerminalWindow() = %q, want %q", got, tt.want)
			}
		})
	}
}
