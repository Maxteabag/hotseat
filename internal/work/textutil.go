// Package work reads stopped native work and explicitly resumes the same
// conversation.
//
// It ports four Python modules that shared one job: turning stored transcripts
// into something readable (textutil), finding native sessions that a usage limit
// stopped and describing how to continue them (resume), recovering what a stopped
// piece of work was actually doing (inspect), and the /proc-backed process
// identity plus the resume/reboot safety chain (work).
//
// Everything here is read-only and local except Service.Act, which is explicit.
// Content is truncated on the way out rather than in the caller, so a long
// transcript cannot flood a terminal, and speech markup is stripped because it is
// delivery scaffolding, not what the agent said.
package work

import (
	"regexp"
	"strings"
)

// speechMarkup matches the TTS markup voice clients wrap spoken replies in; it is
// noise when read.
var speechMarkup = regexp.MustCompile(`</?(speak|break|voice|speed|emphasis|prosody|phoneme)[^>]*>`)

// Snippet is the default display length for trimmed text.
const Snippet = 400

// Clean strips speech markup and collapses whitespace.
func Clean(text string) string {
	if text == "" {
		return ""
	}
	return strings.Join(strings.Fields(speechMarkup.ReplaceAllString(text, " ")), " ")
}

// Trim cleans and shortens text for display, ending a cut with "…".
func Trim(text string, limit int) string {
	cleaned := Clean(text)
	runes := []rune(cleaned)
	if len(runes) <= limit {
		return cleaned
	}
	return strings.TrimRightFunc(string(runes[:limit]), isPySpace) + "…"
}

// Entry is one parsed native transcript line. A line that parsed to something
// other than an object is kept as a nil map so tail arithmetic stays the same.
type Entry map[string]any

// EntryText is the readable text of one native transcript entry: a string
// content, or the text blocks of a list content joined by spaces.
func EntryText(entry Entry) string {
	message, _ := entry["message"].(map[string]any)
	switch content := message["content"].(type) {
	case string:
		return content
	case []any:
		parts := make([]string, 0, len(content))
		for _, part := range content {
			block, ok := part.(map[string]any)
			if !ok || block["type"] != "text" {
				continue
			}
			text, _ := block["text"].(string)
			parts = append(parts, text)
		}
		return strings.Join(parts, " ")
	}
	return ""
}

// str returns the entry's field as a string, or "" when absent or not a string.
func (e Entry) str(key string) string {
	value, _ := e[key].(string)
	return value
}

// truthy mirrors Python truthiness for the JSON values a transcript holds.
func truthy(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case bool:
		return v
	case string:
		return v != ""
	case float64:
		return v != 0
	case []any:
		return len(v) > 0
	case map[string]any:
		return len(v) > 0
	}
	return true
}

// isPySpace mirrors str.rstrip()'s notion of whitespace closely enough for
// display text.
func isPySpace(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\r', '\v', '\f', 0x1c, 0x1d, 0x1e, 0x1f, 0x85, 0xa0:
		return true
	}
	return r > 0xff && (r == 0x1680 || (r >= 0x2000 && r <= 0x200a) || r == 0x2028 || r == 0x2029 || r == 0x202f || r == 0x205f || r == 0x3000)
}
