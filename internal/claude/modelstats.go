package claude

// Per-model token usage, read from Claude Code's own local statistics.
//
// This answers a different question from the quota bars and must not be
// confused with them:
//
//   - The quota bars come from the API and describe one account's remaining
//     budget.
//   - These numbers come from `stats-cache.json`, count tokens rather than
//     quota, and cover every account used on this machine.
//
// They are reported side by side because "how much of the week is gone" and
// "what was it spent on" are both worth knowing, but they are never added
// together.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DefaultDays is the window RecentByModel aggregates over.
const DefaultDays = 7

// dateSuffix: trailing release dates are noise in a label: claude-haiku-4-5-20251001.
var dateSuffix = regexp.MustCompile(`-\d{8}$`)

// FamilyOrder lists the known model families, most capable first.
var FamilyOrder = []string{"fable", "opus", "sonnet", "haiku"}

// StatsFile is the path of Claude Code's statistics cache under root (ConfigDir
// when empty).
func StatsFile(root string) string {
	if root == "" {
		root = ConfigDir()
	}
	return filepath.Join(root, "stats-cache.json")
}

// Label turns a model id into something readable: claude-fable-5-1 -> Fable 5.1.
func Label(model string) string {
	name := dateSuffix.ReplaceAllString(model, "")
	name = strings.TrimPrefix(name, "claude-")
	parts := strings.Split(name, "-")
	family := capitalize(parts[0])
	version := strings.Join(parts[1:], ".")
	return strings.TrimSpace(family + " " + version)
}

// capitalize mirrors str.capitalize: first rune upper, the rest lower.
func capitalize(s string) string {
	if s == "" {
		return s
	}
	lower := strings.ToLower(s)
	return strings.ToUpper(lower[:1]) + lower[1:]
}

// Family classifies a model id into one of FamilyOrder or "other".
func Family(model string) string {
	lowered := strings.ToLower(model)
	for _, known := range FamilyOrder {
		if strings.Contains(lowered, known) {
			return known
		}
	}
	return "other"
}

// ModelUsage is one model's share of the window.
type ModelUsage struct {
	Model  string  `json:"model"`
	Label  string  `json:"label"`
	Family string  `json:"family"`
	Tokens int64   `json:"tokens"`
	Share  float64 `json:"share"`
}

// ModelSummary is the aggregated per-model usage for the last Days days.
type ModelSummary struct {
	Days int     `json:"days"`
	AsOf *string `json:"as_of"`
	// Stale: the cache is recomputed periodically, so today's activity may be missing.
	Stale       bool         `json:"stale"`
	TotalTokens int64        `json:"total_tokens"`
	Models      []ModelUsage `json:"models"`
}

// RecentByModel aggregates the last `days` of per-model token counts from the
// statistics file under root (ConfigDir when empty), as of `today` (now when
// zero).
//
// Returns nil when the statistics file is absent or unreadable, which is a
// normal state on a fresh install rather than an error.
func RecentByModel(days int, root string, today time.Time) *ModelSummary {
	raw, err := os.ReadFile(StatsFile(root))
	if err != nil {
		return nil
	}
	var data struct {
		DailyModelTokens json.RawMessage `json:"dailyModelTokens"`
		LastComputedDate any             `json:"lastComputedDate"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil
	}
	var daily []json.RawMessage
	if data.DailyModelTokens == nil || json.Unmarshal(data.DailyModelTokens, &daily) != nil {
		return nil
	}

	if today.IsZero() {
		today = time.Now()
	}
	cutoff := today.AddDate(0, 0, -days).Format("2006-01-02")

	totals := map[string]int64{}
	var order []string
	for _, rowRaw := range daily {
		var row struct {
			Date          any             `json:"date"`
			TokensByModel json.RawMessage `json:"tokensByModel"`
		}
		if json.Unmarshal(rowRaw, &row) != nil || stringify(row.Date) < cutoff {
			continue
		}
		for _, entry := range orderedNumbers(row.TokensByModel) {
			if entry.value > 0 {
				if _, seen := totals[entry.key]; !seen {
					order = append(order, entry.key)
				}
				totals[entry.key] += int64(entry.value)
			}
		}
	}

	var total int64
	for _, count := range totals {
		total += count
	}
	// Counter.most_common: by count descending, insertion order for ties.
	sort.SliceStable(order, func(i, j int) bool { return totals[order[i]] > totals[order[j]] })
	models := make([]ModelUsage, 0, len(order))
	for _, model := range order {
		count := totals[model]
		share := 0.0
		if total != 0 {
			share = float64(count) / float64(total)
		}
		models = append(models, ModelUsage{
			Model: model, Label: Label(model), Family: Family(model), Tokens: count, Share: share,
		})
	}

	var asOf *string
	stale := false
	if s, ok := data.LastComputedDate.(string); ok {
		asOf = &s
		stale = s != "" && s < today.AddDate(0, 0, -1).Format("2006-01-02")
	}
	return &ModelSummary{Days: days, AsOf: asOf, Stale: stale, TotalTokens: total, Models: models}
}

// stringify is str() for the date field: strings as-is, numbers by their
// literal, anything else empty.
func stringify(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		return t.String()
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	}
	return ""
}

type numberEntry struct {
	key   string
	value float64
}

// orderedNumbers reads an object's numeric members in document order, which a
// map would lose. Non-object input or non-numeric members yield nothing.
func orderedNumbers(raw json.RawMessage) []numberEntry {
	if len(raw) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return nil
	}
	var out []numberEntry
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return out
		}
		key, _ := keyTok.(string)
		var value any
		if err := dec.Decode(&value); err != nil {
			return out
		}
		if n, ok := value.(json.Number); ok {
			if f, err := n.Float64(); err == nil {
				out = append(out, numberEntry{key: key, value: f})
			}
		}
	}
	return out
}
