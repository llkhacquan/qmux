package main

import (
	"path/filepath"
	"strings"

	"github.com/llkhacquan/qmux/sidebar-go/internal/ckcontext"
)

// toolActivityLabel returns a context-aware label for a tool intent.
// Priority: file-type hint > subagent enrichment > raw intent fallback.
func toolActivityLabel(in *ckcontext.Intent) string {
	if in.FilePath != "" {
		base := filepath.Base(in.FilePath)
		if verb := fileTypeVerb(in.Tool, in.FilePath); verb != "" {
			return verb + " " + base
		}
	}

	if in.Tool == "Agent" && in.SubagentType != "" {
		rest := strings.TrimSpace(strings.TrimPrefix(in.Intent, "delegating"))
		if rest != "" {
			return "spawning " + strings.ToLower(in.SubagentType) + " " + rest
		}
		return "spawning " + strings.ToLower(in.SubagentType)
	}

	return in.Intent
}

// fileTypeVerb returns a descriptive verb for a tool + file path combo.
// Order matters: test patterns must precede generic source extensions.
func fileTypeVerb(tool, path string) string {
	lower := strings.ToLower(path)
	base := strings.ToLower(filepath.Base(path))
	isRead := tool == "Read"
	isWrite := tool == "Edit" || tool == "Write" || tool == "MultiEdit"

	// Test files (must check before generic source)
	if isTestFile(base, lower) {
		if isRead {
			return "reviewing tests"
		}
		if isWrite {
			return "writing tests"
		}
	}

	// Documentation
	if hasAnySuffix(base, ".md", ".txt", ".rst", ".adoc") {
		if isRead {
			return "reading docs"
		}
		if isWrite {
			return "updating docs"
		}
	}

	// Config files
	if isConfigFile(base) {
		if isRead {
			return "parsing config"
		}
		if isWrite {
			return "adjusting config"
		}
	}

	// SQL / migrations
	if strings.HasSuffix(base, ".sql") || containsAny(lower, "/migrations/", "/migrate/") {
		if isRead {
			return "reviewing schema"
		}
		if isWrite {
			return "writing migration"
		}
	}

	// Protobuf
	if strings.HasSuffix(base, ".proto") {
		if isRead {
			return "reading proto"
		}
		if isWrite {
			return "updating proto"
		}
	}

	// Source code (Go, TS, JS, Python, Rust, etc.)
	if isSourceFile(base) {
		if isRead {
			return "reading source"
		}
		if isWrite {
			return "editing source"
		}
	}

	return ""
}

func isTestFile(base, fullLower string) bool {
	return strings.HasSuffix(base, "_test.go") ||
		strings.HasSuffix(base, ".test.ts") ||
		strings.HasSuffix(base, ".test.tsx") ||
		strings.HasSuffix(base, ".test.js") ||
		strings.HasSuffix(base, ".test.jsx") ||
		strings.HasSuffix(base, "_test.py") ||
		strings.HasSuffix(base, ".spec.ts") ||
		strings.HasSuffix(base, ".spec.js") ||
		containsAny(fullLower, "/tests/", "/test/", "/__tests__/")
}

func isConfigFile(base string) bool {
	return hasAnySuffix(base, ".json", ".yaml", ".yml", ".toml", ".ini", ".env", ".envrc")
}

func isSourceFile(base string) bool {
	return hasAnySuffix(base, ".go", ".ts", ".tsx", ".js", ".jsx",
		".py", ".rs", ".rb", ".java", ".kt", ".swift", ".c", ".cpp", ".h")
}

func hasAnySuffix(s string, suffixes ...string) bool {
	for _, suf := range suffixes {
		if strings.HasSuffix(s, suf) {
			return true
		}
	}
	return false
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
