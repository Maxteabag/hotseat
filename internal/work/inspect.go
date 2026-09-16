package work

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// ContextPreamble opens a continuation prompt. It is not something the user
// typed, but its summary is often the best description of what the work was, so
// it is mined rather than discarded.
const ContextPreamble = "This session is being continued from a previous conversation"

var summaryMarker = regexp.MustCompile(`(?s)(?:Summary:|Primary Request and Intent:?)\s*(.+)`)

// InspectError means the stopped work could not be inspected.
type InspectError struct{ Msg string }

func (e *InspectError) Error() string { return e.Msg }

// Detail is everything known about one stopped native item.
type Detail struct {
	Kind          string       `json:"kind"`
	ID            string       `json:"id"`
	Name          string       `json:"name"`
	Backend       string       `json:"backend"`
	Model         *string      `json:"model"`
	CWD           *string      `json:"cwd"`
	Branch        *string      `json:"branch"`
	LastUser      string       `json:"last_user"`
	Summary       string       `json:"summary"`
	LastAssistant string       `json:"last_assistant"`
	LastActivity  *string      `json:"last_activity"`
	Queued        []any        `json:"queued"`
	Recent        []RecentTurn `json:"recent"`
	Turns         int          `json:"turns"`
}

// RecentTurn is one of the last exchanges, shortened for a list.
type RecentTurn struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

// summaryOf pulls the work description out of a context-continuation preamble.
func summaryOf(text string) string {
	if !strings.HasPrefix(text, ContextPreamble) {
		return ""
	}
	if match := summaryMarker.FindStringSubmatch(text); match != nil {
		return Trim(match[1], 500)
	}
	return Trim(text[len(ContextPreamble):], 500)
}

// usable says whether an entry is a real turn of the given role. Tool results
// and system scaffolding arrive as user entries too.
func usable(entry Entry, role string) bool {
	if entry.str("type") != role || truthy(entry["isApiErrorMessage"]) {
		return false
	}
	text := strings.TrimSpace(EntryText(entry))
	return text != "" && !strings.HasPrefix(text, "<") && !strings.HasPrefix(text, ContextPreamble)
}

// findTranscript locates <projectsDir>/*/<sessionID>.jsonl, accepting the short
// form shown in listings as a prefix.
func findTranscript(projectsDir, sessionID string) string {
	var prefixed string
	for _, path := range transcripts(projectsDir) {
		name := filepath.Base(path)
		if name == sessionID+".jsonl" {
			return path
		}
		if prefixed == "" && strings.HasPrefix(name, sessionID) {
			prefixed = path
		}
	}
	return prefixed
}

// NativeDetail reads a whole native transcript and describes what the session
// was doing. It returns nil when no transcript matches.
func NativeDetail(projectsDir, sessionID string) (*Detail, error) {
	transcript := findTranscript(projectsDir, sessionID)
	if transcript == "" {
		return nil, nil
	}
	parsed, err := allEntries(transcript)
	if err != nil {
		return nil, &InspectError{Msg: fmt.Sprintf("could not read the transcript: %v", err)}
	}
	entries := make([]Entry, 0, len(parsed))
	for _, e := range parsed {
		if e != nil {
			entries = append(entries, e)
		}
	}
	if len(entries) == 0 {
		return nil, nil
	}

	var users, assistants []Entry
	recent := []RecentTurn{}
	for _, e := range entries {
		user, assistant := usable(e, "user"), usable(e, "assistant")
		if user {
			users = append(users, e)
		}
		if assistant {
			assistants = append(assistants, e)
		}
		if user || assistant {
			recent = append(recent, RecentTurn{Role: e.str("type"), Text: Trim(EntryText(e), 160)})
		}
	}
	if len(recent) > 6 {
		recent = recent[len(recent)-6:]
	}
	summary := ""
	for i := len(entries) - 1; i >= 0; i-- {
		if found := summaryOf(EntryText(entries[i])); found != "" {
			summary = found
			break
		}
	}
	last := entries[len(entries)-1]

	id := stem(transcript)
	detail := &Detail{
		Kind:         "native",
		ID:           id,
		Name:         shortName(id),
		Backend:      "claude",
		Summary:      summary,
		LastActivity: optional(last["timestamp"]),
		Queued:       []any{},
		Recent:       recent,
		Turns:        len(users),
	}
	for i := len(entries) - 1; i >= 0; i-- {
		message, _ := entries[i]["message"].(map[string]any)
		if truthy(message["model"]) {
			detail.Model = optional(message["model"])
			break
		}
	}
	detail.CWD = optional(last["cwd"])
	if detail.CWD == nil {
		detail.CWD = lastField(entries, "cwd")
	}
	detail.Branch = lastField(entries, "gitBranch")
	if len(users) > 0 {
		detail.LastUser = Trim(EntryText(users[len(users)-1]), Snippet)
	}
	if len(assistants) > 0 {
		detail.LastAssistant = Trim(EntryText(assistants[len(assistants)-1]), Snippet)
	}
	return detail, nil
}

// lastField is the most recent truthy string value of a field, or nil.
func lastField(entries []Entry, key string) *string {
	for i := len(entries) - 1; i >= 0; i-- {
		if truthy(entries[i][key]) {
			return optional(entries[i][key])
		}
	}
	return nil
}

// optional is a truthy transcript value as a string pointer, nil otherwise.
func optional(value any) *string {
	if !truthy(value) {
		return nil
	}
	if s, ok := value.(string); ok {
		return &s
	}
	s := fmt.Sprint(value)
	return &s
}

// Inspect is everything known about one stopped item, by transcript id or its
// short form.
func Inspect(projectsDir, identifier string) (*Detail, error) {
	found, err := NativeDetail(projectsDir, identifier)
	if err != nil {
		return nil, err
	}
	if found == nil {
		return nil, &InspectError{Msg: fmt.Sprintf("nothing found for %s", pyRepr(identifier))}
	}
	return found, nil
}

// pyRepr quotes a string the way Python's repr does for messages copied from it.
func pyRepr(s string) string {
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		return `"` + s + `"`
	}
	return "'" + strings.ReplaceAll(s, "'", `\'`) + "'"
}

// Inspect is Inspect against the service's projects directory.
func (s *Service) Inspect(identifier string) (*Detail, error) {
	return Inspect(s.ProjectsDir, identifier)
}
