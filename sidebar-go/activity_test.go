package main

import (
	"testing"

	"github.com/llkhacquan/qmux/sidebar-go/internal/ckcontext"
)

func TestToolActivityLabel_FileTypeHints(t *testing.T) {
	tests := []struct {
		name string
		in   ckcontext.Intent
		want string
	}{
		{
			name: "read go test file",
			in:   ckcontext.Intent{Intent: "reading handler_test.go", Tool: "Read", FilePath: "/proj/handler_test.go"},
			want: "reviewing tests handler_test.go",
		},
		{
			name: "edit go test file",
			in:   ckcontext.Intent{Intent: "editing handler_test.go", Tool: "Edit", FilePath: "/proj/handler_test.go"},
			want: "writing tests handler_test.go",
		},
		{
			name: "read ts test file",
			in:   ckcontext.Intent{Intent: "reading auth.test.ts", Tool: "Read", FilePath: "/proj/src/auth.test.ts"},
			want: "reviewing tests auth.test.ts",
		},
		{
			name: "read spec file",
			in:   ckcontext.Intent{Intent: "reading auth.spec.js", Tool: "Read", FilePath: "/proj/src/auth.spec.js"},
			want: "reviewing tests auth.spec.js",
		},
		{
			name: "file in tests/ dir",
			in:   ckcontext.Intent{Intent: "reading helper.go", Tool: "Read", FilePath: "/proj/tests/helper.go"},
			want: "reviewing tests helper.go",
		},
		{
			name: "read markdown",
			in:   ckcontext.Intent{Intent: "reading README.md", Tool: "Read", FilePath: "/proj/README.md"},
			want: "reading docs README.md",
		},
		{
			name: "write markdown",
			in:   ckcontext.Intent{Intent: "creating CHANGELOG.md", Tool: "Write", FilePath: "/proj/CHANGELOG.md"},
			want: "updating docs CHANGELOG.md",
		},
		{
			name: "read json config",
			in:   ckcontext.Intent{Intent: "reading settings.json", Tool: "Read", FilePath: "/proj/settings.json"},
			want: "parsing config settings.json",
		},
		{
			name: "edit yaml",
			in:   ckcontext.Intent{Intent: "editing values.yaml", Tool: "Edit", FilePath: "/proj/values.yaml"},
			want: "adjusting config values.yaml",
		},
		{
			name: "read sql",
			in:   ckcontext.Intent{Intent: "reading schema.sql", Tool: "Read", FilePath: "/proj/schema.sql"},
			want: "reviewing schema schema.sql",
		},
		{
			name: "write migration file",
			in:   ckcontext.Intent{Intent: "creating 001_init.sql", Tool: "Write", FilePath: "/proj/migrations/001_init.sql"},
			want: "writing migration 001_init.sql",
		},
		{
			name: "read proto",
			in:   ckcontext.Intent{Intent: "reading api.proto", Tool: "Read", FilePath: "/proj/api.proto"},
			want: "reading proto api.proto",
		},
		{
			name: "edit go source",
			in:   ckcontext.Intent{Intent: "editing handler.go", Tool: "Edit", FilePath: "/proj/handler.go"},
			want: "editing source handler.go",
		},
		{
			name: "read ts source",
			in:   ckcontext.Intent{Intent: "reading index.ts", Tool: "Read", FilePath: "/proj/src/index.ts"},
			want: "reading source index.ts",
		},
		{
			name: "edit python source",
			in:   ckcontext.Intent{Intent: "editing main.py", Tool: "Edit", FilePath: "/proj/main.py"},
			want: "editing source main.py",
		},
		{
			name: "MultiEdit counts as write",
			in:   ckcontext.Intent{Intent: "editing context.go", Tool: "MultiEdit", FilePath: "/proj/context.go"},
			want: "editing source context.go",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := toolActivityLabel(&tt.in)
			if got != tt.want {
				t.Errorf("toolActivityLabel() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestToolActivityLabel_SubagentEnrichment(t *testing.T) {
	in := &ckcontext.Intent{
		Intent:       "delegating Research auth patterns",
		Tool:         "Agent",
		SubagentType: "researcher",
	}
	got := toolActivityLabel(in)
	want := "spawning researcher Research auth patterns"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestToolActivityLabel_SubagentNoDescription(t *testing.T) {
	in := &ckcontext.Intent{
		Intent:       "delegating",
		Tool:         "Agent",
		SubagentType: "Explore",
	}
	got := toolActivityLabel(in)
	want := "spawning explore"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestToolActivityLabel_Fallback(t *testing.T) {
	tests := []struct {
		name string
		in   ckcontext.Intent
		want string
	}{
		{
			name: "bash command - no file_path, falls back to raw intent",
			in:   ckcontext.Intent{Intent: "running make install", Tool: "Bash"},
			want: "running make install",
		},
		{
			name: "grep - falls back to raw intent",
			in:   ckcontext.Intent{Intent: "searching \"functionName\"", Tool: "Grep"},
			want: "searching \"functionName\"",
		},
		{
			name: "unknown tool",
			in:   ckcontext.Intent{Intent: "WebFetch https://example.com", Tool: "WebFetch"},
			want: "WebFetch https://example.com",
		},
		{
			name: "unknown file extension",
			in:   ckcontext.Intent{Intent: "reading data.bin", Tool: "Read", FilePath: "/proj/data.bin"},
			want: "reading data.bin",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := toolActivityLabel(&tt.in)
			if got != tt.want {
				t.Errorf("toolActivityLabel() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFileTypeVerb(t *testing.T) {
	tests := []struct {
		tool string
		path string
		want string
	}{
		{"Read", "/a/b_test.go", "reviewing tests"},
		{"Edit", "/a/b_test.go", "writing tests"},
		{"Read", "/a/b.go", "reading source"},
		{"Edit", "/a/b.go", "editing source"},
		{"Read", "/a/b.md", "reading docs"},
		{"Write", "/a/b.md", "updating docs"},
		{"Read", "/a/b.json", "parsing config"},
		{"Edit", "/a/b.yaml", "adjusting config"},
		{"Read", "/a/b.sql", "reviewing schema"},
		{"Write", "/a/migrations/001.sql", "writing migration"},
		{"Read", "/a/b.proto", "reading proto"},
		{"Edit", "/a/b.proto", "updating proto"},
		{"Bash", "/a/b.go", ""},
		{"Read", "/a/b.unknown", ""},
	}
	for _, tt := range tests {
		t.Run(tt.tool+"_"+tt.path, func(t *testing.T) {
			got := fileTypeVerb(tt.tool, tt.path)
			if got != tt.want {
				t.Errorf("fileTypeVerb(%q, %q) = %q, want %q", tt.tool, tt.path, got, tt.want)
			}
		})
	}
}
