package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLooksLikeQuestion(t *testing.T) {
	tests := []struct {
		name string
		text string
		want bool
	}{
		{"ends with ?", "Which approach should we use?", true},
		{"starts with should", "Should I proceed with option A", true},
		{"starts with do you", "Do you want me to fix this", true},
		{"starts with want me to", "Want me to add tests", true},
		{"starts with which", "Which file should I modify?", true},
		{"plain statement", "I've fixed the bug in handler.go.", false},
		{"let me know follow-up", "Let me know if you have questions.", false},
		{"feel free follow-up", "Feel free to ask if anything is unclear?", false},
		{"happy to help", "Happy to help with anything else!", false},
		{"code block ending", "```\nreturn nil\n```", false},
		{"empty", "", false},
		{"just a period", "Done.", false},
		{"shall I", "Shall I also update the tests", true},
		{"would you", "Would you like me to continue?", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := looksLikeQuestion(tt.text)
			if got != tt.want {
				t.Errorf("looksLikeQuestion(%q) = %v, want %v", tt.text, got, tt.want)
			}
		})
	}
}

func TestLastSentence(t *testing.T) {
	tests := []struct {
		text string
		want string
	}{
		{"Hello. How are you?", "How are you?"},
		{"Done.", "Done."},
		{"No terminator", "No terminator"},
		{"First. Second. Third?", "Third?"},
		{"Line one\nLine two", "Line two"},
		{"", ""},
		{"  ", ""},
		{"End with exclamation!", "End with exclamation!"},
	}
	for _, tt := range tests {
		t.Run(tt.text, func(t *testing.T) {
			got := lastSentence(tt.text)
			if got != tt.want {
				t.Errorf("lastSentence(%q) = %q, want %q", tt.text, got, tt.want)
			}
		})
	}
}

func TestExtractAssistantText(t *testing.T) {
	tests := []struct {
		name string
		line string
		want string
	}{
		{
			"assistant with text",
			`{"type":"assistant","message":{"content":[{"type":"text","text":"Hello world"}]}}`,
			"Hello world",
		},
		{
			"user message",
			`{"type":"user","message":{"content":[{"type":"text","text":"Hi"}]}}`,
			"",
		},
		{
			"assistant with tool_use only",
			`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"123"}]}}`,
			"",
		},
		{
			"invalid json",
			`not json`,
			"",
		},
		{
			"system type",
			`{"type":"system"}`,
			"",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractAssistantText(tt.line)
			if got != tt.want {
				t.Errorf("extractAssistantText() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLastAssistantText(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.jsonl")

	content := `{"type":"user","message":{"content":[{"type":"text","text":"fix the bug"}]}}
{"type":"assistant","message":{"content":[{"type":"text","text":"I fixed the bug. Should I also update the tests?"}]}}
{"type":"system"}
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	got := lastAssistantText(path)
	want := "I fixed the bug. Should I also update the tests?"
	if got != want {
		t.Errorf("lastAssistantText() = %q, want %q", got, want)
	}
}

func TestLastAssistantText_NoAssistant(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.jsonl")

	content := `{"type":"user","message":{"content":[{"type":"text","text":"hi"}]}}
{"type":"system"}
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	got := lastAssistantText(path)
	if got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestTranscriptPath(t *testing.T) {
	home, _ := os.UserHomeDir()
	cwd := filepath.Join(home, "work", "example-org", "repo")
	got := transcriptPath(cwd, "abc-123")
	slug := strings.ReplaceAll(cwd, "/", "-")
	want := filepath.Join(home, ".claude", "projects", slug, "abc-123.jsonl")
	if got != want {
		t.Errorf("transcriptPath() = %q, want %q", got, want)
	}
}

func TestTranscriptPath_Empty(t *testing.T) {
	if got := transcriptPath("", "abc"); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
	if got := transcriptPath("/foo", ""); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}
