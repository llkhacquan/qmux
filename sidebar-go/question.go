package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const maxTranscriptTail = 128 * 1024 // 128KB, same as agentpet

// transcriptPath resolves the JSONL transcript for a Claude session.
// Pattern: ~/.claude/projects/{cwd-slug}/{sessionID}.jsonl
// where cwd-slug = cwd with "/" replaced by "-".
func transcriptPath(cwd, sessionID string) string {
	if cwd == "" || sessionID == "" {
		return ""
	}
	slug := strings.ReplaceAll(cwd, "/", "-")
	return filepath.Join(Cfg().AgentDataDir(), slug, sessionID+".jsonl")
}

// lastAssistantText reads the tail of a JSONL transcript and returns the
// text content of the last assistant message. Reads at most 128KB from the
// end to keep the cost bounded on large transcripts.
func lastAssistantText(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return ""
	}
	size := info.Size()
	offset := int64(0)
	if size > maxTranscriptTail {
		offset = size - maxTranscriptTail
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return ""
	}

	buf := make([]byte, size-offset)
	n, _ := io.ReadFull(f, buf)
	if n == 0 {
		return ""
	}
	buf = buf[:n]

	lines := strings.Split(string(buf), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		text := extractAssistantText(line)
		if text != "" {
			if len(text) > 400 {
				text = text[len(text)-400:]
			}
			return text
		}
	}
	return ""
}

// transcriptRecord is the minimal shape needed to identify assistant messages.
type transcriptRecord struct {
	Type    string `json:"type"`
	Message struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// extractAssistantText parses a JSONL line and returns the concatenated
// text blocks if type=assistant, empty string otherwise.
func extractAssistantText(line string) string {
	var rec transcriptRecord
	if json.Unmarshal([]byte(line), &rec) != nil || rec.Type != "assistant" {
		return ""
	}
	var blocks []contentBlock
	if json.Unmarshal(rec.Message.Content, &blocks) != nil {
		return ""
	}
	var sb strings.Builder
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}

// looksLikeQuestion returns true if the text ends with a blocking question.
// Filters out polite follow-ups ("let me know if...") that aren't blocking.
func looksLikeQuestion(text string) bool {
	last := lastSentence(text)
	if last == "" {
		return false
	}
	lower := strings.ToLower(last)
	if isOptionalFollowUp(lower) {
		return false
	}
	return isPrimaryQuestion(lower)
}

func isPrimaryQuestion(sentenceLower string) bool {
	if strings.HasSuffix(sentenceLower, "?") {
		return true
	}
	starters := []string{
		"which ", "what ", "how ", "should i ", "do you ",
		"want me to ", "shall i ", "would you ", "can you ",
		"could you ", "are you ",
	}
	for _, s := range starters {
		if strings.HasPrefix(sentenceLower, s) {
			return true
		}
	}
	return false
}

var optionalFollowUps = []string{
	"let me know if", "let me know when", "feel free to",
	"if you'd like any", "if you want any", "happy to help",
	"don't hesitate", "just let me know",
}

func isOptionalFollowUp(sentenceLower string) bool {
	for _, pat := range optionalFollowUps {
		if strings.Contains(sentenceLower, pat) {
			return true
		}
	}
	return false
}

// lastSentence extracts the last sentence from text. Splits on common
// sentence terminators (. ! ? newline) and returns the final non-empty one.
func lastSentence(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	// Split on sentence boundaries, walk backward
	var last string
	start := 0
	for i, r := range text {
		if r == '.' || r == '!' || r == '?' || r == '\n' {
			candidate := strings.TrimSpace(text[start : i+1])
			if candidate != "" {
				last = candidate
			}
			start = i + 1
		}
	}
	// Trailing fragment (no terminator)
	if start < len(text) {
		candidate := strings.TrimSpace(text[start:])
		if candidate != "" {
			last = candidate
		}
	}
	return last
}
