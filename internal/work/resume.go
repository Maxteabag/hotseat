package work

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// limitText is the text Claude Code writes into a transcript when a session limit
// is hit.
var limitText = regexp.MustCompile(`(?i)hit your (session|usage) limit`)

// DefaultWindowDays is how far back a stop is still worth continuing.
const DefaultWindowDays = 7

// ResumePrompt is the continuation prompt handed to resumed work.
const ResumePrompt = "Continue the work that was interrupted by the usage limit. The account has " +
	"quota again. Re-check the results of the interrupted step before retrying it, " +
	"and do not repeat work that already completed."

// tailKeep is how many trailing transcript entries the stopped-work scan keeps.
const tailKeep = 40

// ResumeError means stopped work could not be listed, or could not be continued.
type ResumeError struct{ Msg string }

func (e *ResumeError) Error() string { return e.Msg }

// StoppedItem is one piece of work that stopped and may be continued. Native
// items fill the first nine fields; Model, Account and Readiness exist for the
// Clarp source and for callers that annotate items, and are omitted when empty.
type StoppedItem struct {
	Kind        string     `json:"kind"`
	Cause       string     `json:"cause"`
	Pending     int        `json:"pending"`
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Backend     string     `json:"backend"`
	CWD         string     `json:"cwd"`
	LastMessage string     `json:"last_message"`
	StoppedAt   float64    `json:"stopped_at"`
	Model       string     `json:"model,omitempty"`
	Account     string     `json:"account,omitempty"`
	Readiness   *Readiness `json:"readiness,omitempty"`
}

// Readiness says whether an item can be continued, and what stands in the way.
type Readiness struct {
	Ready   bool    `json:"ready"`
	Hard    *string `json:"hard"`
	Warning *string `json:"warning"`
}

// Quota is the part of an account's usage summary readiness looks at.
type Quota struct {
	Limited       bool
	BlockedModels []string
}

// AccountQuota is one saved account as readiness sees it: its alias and its
// quota, nil when the quota is unknown.
type AccountQuota struct {
	Alias string
	Usage *Quota
}

// Continuation is the command that continues a native item.
type Continuation struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Continued bool   `json:"continued"`
	Command   string `json:"command"`
	CWD       string `json:"cwd"`
}

// DefaultProjectsDir is where Claude Code keeps native transcripts. It honours
// CLAUDE_CONFIG_DIR (design decision 1).
func DefaultProjectsDir() string {
	if override := os.Getenv("CLAUDE_CONFIG_DIR"); override != "" {
		return filepath.Join(override, "projects")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".claude", "projects")
}

// LastEntries is the tail of a transcript, parsed. Transcripts are large, so the
// file is read once; the buffer is trimmed whenever it grows past keep*8.
func LastEntries(path string, keep int) []Entry {
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()
	var entries []Entry
	reader := bufio.NewReader(file)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			if entry, ok := parseLine(line); ok {
				entries = append(entries, entry)
			}
			if len(entries) > keep*8 {
				entries = entries[keep*4:]
			}
		}
		if err != nil {
			if err != io.EOF {
				return nil
			}
			break
		}
	}
	if len(entries) > keep {
		entries = entries[len(entries)-keep:]
	}
	return entries
}

// allEntries parses a whole transcript; damaged lines are skipped.
func allEntries(path string) ([]Entry, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var entries []Entry
	reader := bufio.NewReader(file)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			if entry, ok := parseLine(line); ok {
				entries = append(entries, entry)
			}
		}
		if err != nil {
			if err != io.EOF {
				return nil, err
			}
			break
		}
	}
	return entries, nil
}

// parseLine parses one JSONL line. Non-object values count as entries (nil maps)
// so the tail arithmetic matches; unparsable lines are dropped.
func parseLine(line []byte) (Entry, bool) {
	var value any
	if err := json.Unmarshal(line, &value); err != nil {
		return nil, false
	}
	object, _ := value.(map[string]any)
	return Entry(object), true
}

// transcripts lists <projectsDir>/*/*.jsonl, sorted by path.
func transcripts(projectsDir string) []string {
	projects, err := os.ReadDir(projectsDir)
	if err != nil {
		return nil
	}
	var found []string
	for _, project := range projects {
		dir := filepath.Join(projectsDir, project.Name())
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			continue
		}
		files, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, file := range files {
			if strings.HasSuffix(file.Name(), ".jsonl") {
				found = append(found, filepath.Join(dir, file.Name()))
			}
		}
	}
	return found
}

// stem is the file name without its extension, like pathlib's Path.stem.
func stem(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// epochSeconds is a modification time as Python's st_mtime float.
func epochSeconds(t time.Time) float64 {
	return float64(t.Unix()) + 1e-9*float64(t.Nanosecond())
}

// StoppedNativeSessions lists native sessions whose transcript ends on a
// usage-limit error, most recent first.
func StoppedNativeSessions(projectsDir string, windowDays int, now time.Time) []StoppedItem {
	if info, err := os.Stat(projectsDir); err != nil || !info.IsDir() {
		return nil
	}
	var cutoff float64
	if windowDays != 0 {
		cutoff = epochSeconds(now) - float64(windowDays)*86400
	}
	var found []StoppedItem
	for _, transcript := range transcripts(projectsDir) {
		info, err := os.Stat(transcript)
		if err != nil {
			continue
		}
		modified := epochSeconds(info.ModTime())
		if modified < cutoff {
			continue
		}
		entries := LastEntries(transcript, tailKeep)
		if len(entries) == 0 {
			continue
		}
		// Taken from the tail already in hand: re-reading a transcript that can run
		// to tens of megabytes just for one line would not be worth it.
		said := ""
		for i := len(entries) - 1; i >= 0; i-- {
			e := entries[i]
			if e.str("type") == "assistant" && !truthy(e["isApiErrorMessage"]) && strings.TrimSpace(EntryText(e)) != "" {
				said = EntryText(e)
				break
			}
		}
		// Only the final assistant turn counts. An earlier limit that was worked
		// through is history, not something waiting to be continued.
		last := lastTurn(entries)
		if last == nil || !truthy(last["isApiErrorMessage"]) {
			continue
		}
		message, _ := last["message"].(map[string]any)
		text, ok := message["content"].(string)
		if !ok {
			if encoded, err := json.Marshal(message["content"]); err == nil {
				text = string(encoded)
			}
		}
		if !limitText.MatchString(text) {
			continue
		}
		cwd := last.str("cwd")
		if cwd == "" {
			cwd = DecodeProjectDir(filepath.Base(filepath.Dir(transcript)))
		}
		id := stem(transcript)
		found = append(found, StoppedItem{
			Kind:        "native",
			Cause:       "usage_limit",
			Pending:     0,
			ID:          id,
			Name:        shortName(id),
			Backend:     "claude",
			CWD:         cwd,
			LastMessage: Trim(said, 150),
			StoppedAt:   modified,
		})
	}
	slices.SortStableFunc(found, func(a, b StoppedItem) int {
		return compareFloatDesc(a.StoppedAt, b.StoppedAt)
	})
	return found
}

// lastTurn is the final assistant or user entry of a tail, or nil.
func lastTurn(entries []Entry) Entry {
	for i := len(entries) - 1; i >= 0; i-- {
		if kind := entries[i].str("type"); kind == "assistant" || kind == "user" {
			return entries[i]
		}
	}
	return nil
}

func shortName(id string) string {
	if runes := []rune(id); len(runes) > 8 {
		return string(runes[:8])
	}
	return id
}

func compareFloatDesc(a, b float64) int {
	switch {
	case a > b:
		return -1
	case a < b:
		return 1
	}
	return 0
}

// DecodeProjectDir recovers a usable path guess from a project directory name,
// which encodes the path with dashes: -a-b-c becomes /a/b/c.
func DecodeProjectDir(name string) string {
	return "/" + strings.ReplaceAll(strings.TrimLeft(name, "-"), "-", "/")
}

// SortByPriority orders items by how much attention they deserve, not merely by
// recency.
//
// Work already queued and unable to run outranks everything: it is blocked and
// someone is waiting on it. A paused queue holding nothing is last, because it is
// leftover state rather than stalled work. Recency only breaks ties.
func SortByPriority(items []StoppedItem) {
	key := func(item StoppedItem) (bool, bool) {
		hasPending := item.Pending != 0
		idlePause := item.Cause == "queue_paused" && !hasPending
		return !hasPending, idlePause
	}
	slices.SortStableFunc(items, func(a, b StoppedItem) int {
		aNoPending, aIdle := key(a)
		bNoPending, bIdle := key(b)
		if aNoPending != bNoPending {
			if !aNoPending {
				return -1
			}
			return 1
		}
		if aIdle != bIdle {
			if !aIdle {
				return -1
			}
			return 1
		}
		return compareFloatDesc(a.StoppedAt, b.StoppedAt)
	})
}

// Family is the model family an id belongs to: claude-fable-5-1 -> fable.
func Family(model string) string {
	lowered := strings.ToLower(model)
	for _, known := range []string{"fable", "opus", "sonnet", "haiku"} {
		if strings.Contains(lowered, known) {
			return known
		}
	}
	return ""
}

// ServableBy lists which accounts could serve this model right now.
//
// An account qualifies when it is not rate limited and has not spent the weekly
// limit for that model's family. Per-model limits are the point: an account can
// be healthy overall and still unable to serve one family.
func ServableBy(model string, accounts []AccountQuota) []string {
	family := Family(model)
	var able []string
	for _, account := range accounts {
		usage := account.Usage
		if usage == nil || usage.Limited {
			continue
		}
		blocked := false
		for _, m := range usage.BlockedModels {
			name := Family(m)
			if name == "" {
				name = strings.ToLower(m)
			}
			if family != "" && name == family {
				blocked = true
				break
			}
		}
		if blocked {
			continue
		}
		if account.Alias != "" {
			able = append(able, account.Alias)
		}
	}
	return able
}

// StarvingModels maps models no account can serve to the agents waiting on them.
//
// Supervisor's account check requires one account to serve every parked agent's
// model at once, so a single unservable model keeps the whole parked set waiting,
// including agents whose own model is available. Naming it is the difference
// between "nothing runs" and "this is why".
func StarvingModels(items []StoppedItem, accounts []AccountQuota) map[string][]string {
	starving := map[string][]string{}
	for _, item := range items {
		if item.Cause != "waiting_for_account" {
			continue
		}
		if len(ServableBy(item.Model, accounts)) > 0 {
			continue
		}
		model := item.Model
		if model == "" {
			model = "unknown"
		}
		starving[model] = append(starving[model], item.Name)
	}
	return starving
}

// ReadinessOf says whether this item can be continued, and what stands in the
// way.
//
// Two different things get confused here, so they are kept apart:
//
//   - a hard block means continuing now would reproduce the original failure, so
//     it is refused;
//   - a warning means it may fail for a reason that depends on which model the
//     work uses, which cannot be known in advance.
//
// An exhausted account is hard. A spent limit for one model family is a warning,
// because the resumed work may not use that family at all.
func ReadinessOf(item StoppedItem, accounts []AccountQuota, defaultAlias string) Readiness {
	if item.Cause == "waiting_for_account" {
		able := ServableBy(item.Model, accounts)
		if len(able) > 0 {
			return Readiness{Ready: true, Warning: ptr(fmt.Sprintf("parked; %s could serve it", strings.Join(able[:min(2, len(able))], ", ")))}
		}
		model := item.Model
		if model == "" {
			model = "its model"
		}
		return Readiness{Hard: ptr("no account can serve " + model)}
	}
	if item.Cause == "queue_paused" {
		// A prompt to a paused queue is accepted and then queued, so it reports
		// success while changing nothing. Refusing is the honest answer.
		detail := ""
		if item.Pending != 0 {
			detail = fmt.Sprintf(", %d turn(s) already waiting", item.Pending)
		}
		return Readiness{Hard: ptr(fmt.Sprintf("queue is paused%s; a prompt would only queue", detail))}
	}
	if item.Backend != "claude" {
		// Codex limits are OpenAI's. Nothing here can see that quota, and
		// pretending otherwise would be worse than saying so.
		return Readiness{Ready: true, Warning: ptr("OpenAI quota is not visible from here")}
	}
	alias := item.Account
	if alias == "" {
		alias = defaultAlias
	}
	var account *AccountQuota
	for i := range accounts {
		if accounts[i].Alias == alias {
			account = &accounts[i]
			break
		}
	}
	if account == nil {
		name := alias
		if name == "" {
			name = "unknown"
		}
		return Readiness{Hard: ptr(fmt.Sprintf("account %s is not saved here", name))}
	}
	if account.Usage == nil {
		return Readiness{Hard: ptr(alias + " quota is unknown")}
	}
	if account.Usage.Limited {
		return Readiness{Hard: ptr(alias + " is still rate limited")}
	}
	if blocked := account.Usage.BlockedModels; len(blocked) > 0 {
		return Readiness{Ready: true, Warning: ptr(fmt.Sprintf("%s has no %s quota left", alias, blocked[0]))}
	}
	return Readiness{Ready: true}
}

func ptr(s string) *string { return &s }

// Stopped lists native work a usage limit stopped within the window, ordered by
// priority. includePaused is accepted for parity with the Python signature; the
// native source has no paused queues, so it changes nothing.
func (s *Service) Stopped(windowDays int, includePaused bool) []StoppedItem {
	items := StoppedNativeSessions(s.ProjectsDir, windowDays, s.now())
	SortByPriority(items)
	return items
}

// ContinueItem describes how to continue a native item: the resume command and
// the directory it belongs in. Nothing is launched (native sessions need a
// terminal). Other kinds are refused; the plugin route is gone (design decision 7).
func ContinueItem(item StoppedItem) (Continuation, error) {
	if item.Kind != "native" {
		return Continuation{}, &ResumeError{Msg: "This source requires its plugin"}
	}
	return Continuation{
		ID:        item.ID,
		Kind:      "native",
		Continued: false,
		Command:   "claude --resume " + item.ID,
		CWD:       item.CWD,
	}, nil
}
