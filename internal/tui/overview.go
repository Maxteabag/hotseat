package tui

import (
	"charm.land/lipgloss/v2"
	"fmt"
	"math"
	"strings"
	"time"
)

func remaining(used float64) float64 { return math.Max(0, math.Min(100, (1-used)*100)) }
func remainingBar(used float64, width int) string {
	value := remaining(used)
	filled := int(math.Round(value / 100 * float64(width)))
	c := green
	if value <= 20 {
		c = amber
	}
	if value == 0 {
		c = red
	}
	return ink(strings.Repeat("█", filled), c) + ink(strings.Repeat("░", width-filled), border)
}
func resetText(reset float64, now time.Time) (string, string) {
	if reset <= 0 {
		return "—", "—"
	}
	at := time.Unix(int64(reset), 0)
	date := at.Format("02 Jan 15:04")
	seconds := int(at.Sub(now).Seconds())
	if seconds <= 0 {
		return date, "due; refresh"
	}
	minutes := (seconds + 59) / 60
	days := minutes / 1440
	hours := (minutes % 1440) / 60
	if days > 0 {
		return date, fmt.Sprintf("%dd %dh", days, hours)
	}
	if hours > 0 {
		return date, fmt.Sprintf("%dh %dm", hours, minutes%60)
	}
	return date, fmt.Sprintf("%dm", minutes)
}
func windowLabel(s string) string {
	s = strings.ReplaceAll(s, "codex_bengalfox", "Spark")
	s = strings.ReplaceAll(s, "codex ", "Codex ")
	s = strings.ReplaceAll(s, " primary ", " ")
	s = strings.ReplaceAll(s, " secondary ", " ")
	return safe(s)
}

type quotaPair struct {
	name         string
	five, weekly *Window
	other        *Window
}

func pairs(windows []Window) []quotaPair {
	out := []quotaPair{}
	for _, q := range windows {
		label := windowLabel(q.Label)
		period := ""
		name := label
		for _, suffix := range []string{"5-hour", "weekly"} {
			if strings.HasSuffix(label, suffix) {
				period = suffix
				name = strings.TrimRight(strings.TrimSuffix(label, suffix), " ·")
				break
			}
		}
		if period == "" {
			out = append(out, quotaPair{name: label, other: &q})
			continue
		}
		i := 0
		for i < len(out) && out[i].name != name {
			i++
		}
		if i == len(out) {
			out = append(out, quotaPair{name: name})
		}
		if period == "5-hour" {
			out[i].five = &q
		} else {
			out[i].weekly = &q
		}
	}
	return out
}
func creditText(a Account) string {
	if a.Provider != "codex" {
		return "—"
	}
	if a.ResetCredits == nil {
		return "?"
	}
	return fmt.Sprint(*a.ResetCredits)
}
func quotaCell(q *Window, width int) string {
	if q == nil {
		return ink("Not reported", muted)
	}
	return remainingBar(q.Used, max(3, width-9)) + fmt.Sprintf(" %5.1f%%", remaining(q.Used))
}
func resetCell(q *Window, now time.Time) string {
	if q == nil {
		return ""
	}
	date, left := resetText(q.Reset, now)
	return ink(date+" · "+left, muted)
}
func (m *Model) overview(w, h int) string {
	if m.provider == 2 {
		return m.claudeOverview(w, h)
	}
	rows := m.filtered()
	inner := w - 4
	wide := w >= 96
	type line struct {
		text    string
		account int
	}
	all := []line{}
	selectedStart := 0
	now := time.Now()
	accountW, resetsW, resetW := min(25, inner/4), 7, 28
	quotaW := inner - accountW - resetsW - resetW
	header := fit("Account", accountW) + fit("Weekly left", quotaW) + fit("Reset", resetW) + "Resets"
	if !wide {
		accountW = inner - 12
		header = "Account / weekly remaining"
	}
	for i, a := range rows {
		if i > 0 {
			all = append(all, line{ink(strings.Repeat("─", inner), border), -1})
		}
		if i == m.cursor {
			selectedStart = len(all)
		}
		label := accountLabel(a, accountW)
		if a.Error != "" {
			status := "Read failed · Enter for error"
			if strings.Contains(a.Error, "429") {
				status = "API throttled · retry later"
			}
			lower := strings.ToLower(a.Error)
			if strings.Contains(lower, "revoked") || strings.Contains(lower, "rejected") {
				status = "Sign in required"
			}
			all = append(all, line{fit(label, accountW) + fit(status, max(1, inner-accountW-resetsW)) + creditText(a), i})
			continue
		}
		q := codexWeekly(a)
		if wide {
			all = append(all, line{fit(label, accountW) + fit(quotaCell(q, min(36, quotaW-2)), quotaW) + fit(resetCell(q, now), resetW) + creditText(a), i})
			all = append(all, line{accountMeta(a, 0), i})
		} else {
			all = append(all, line{fit(label, accountW) + "Resets " + creditText(a), i}, line{" " + quotaCell(q, min(36, inner-2)), i}, line{" " + resetCell(q, now), i})
		}
	}
	visible := max(1, h-4)
	start := max(0, selectedStart-visible+min(4, visible))
	lines := []string{" " + ink(header, muted), " " + ink(strings.Repeat("─", max(0, inner)), border)}
	for _, row := range all[start:min(len(all), start+visible)] {
		text := " " + fit(row.text, inner)
		if row.account == m.cursor {
			text = ink("›", accent) + fit(row.text, inner)
		}
		lines = append(lines, text)
	}
	if len(all) == 0 {
		lines = append(lines, " No matching accounts")
	}
	return frame("", strings.Join(lines, "\n"), w, h, true)
}

func (m *Model) pageAccounts(direction int) {
	rows := m.filtered()
	step := max(1, m.height-12)
	position := 0
	for i, a := range rows {
		if i == m.cursor {
			break
		}
		n := accountRowCount(a, m.width)
		position += n
	}
	target := max(0, position+direction*step)
	position = 0
	for i, a := range rows {
		n := accountRowCount(a, m.width)
		if position+n > target {
			m.cursor = i
			m.tableOffset = 0
			return
		}
		position += n
	}
	m.cursor = max(0, len(rows)-1)
	m.tableOffset = 0
}

func accountRowCount(a Account, width int) int {
	if a.Provider == "codex" {
		if a.Error != "" {
			return 2
		}
		if width >= 96 {
			return 3
		}
		return 4
	}
	if a.Provider == "claude" && a.Error == "" && len(a.Windows) > 0 {
		if width >= 96 {
			return 2
		}
		n := 4
		five, weekly, fable := claudeWindows(a)
		for _, q := range []*Window{five, weekly, fable} {
			if q != nil {
				n++
			}
		}
		return n
	}
	if a.Error != "" || len(a.Windows) == 0 {
		return 2
	}
	n := 1
	for _, p := range pairs(a.Windows) {
		if width >= 96 && p.other == nil {
			n += 2
		} else {
			n++
			for _, q := range []*Window{p.five, p.weekly, p.other} {
				if q != nil {
					n += 2
				}
			}
		}
	}
	return n
}

func planName(plan string) string {
	lower := strings.ToLower(plan)
	if strings.Contains(lower, "business") {
		return "Business"
	}
	switch lower {
	case "pro":
		return "Pro"
	case "team":
		return "Team"
	case "max":
		return "Max"
	}
	return safe(plan)
}
func accountMeta(a Account, group int) string {
	if group != 0 {
		return ""
	}
	s := planName(a.Plan)
	if a.Cached {
		s += " · cached " + time.Unix(int64(a.CheckedAt), 0).Format("15:04")
	}
	if len(a.Aliases) > 1 {
		s += fmt.Sprintf(" · %d profiles", len(a.Aliases))
	}
	return ink(s, muted)
}

func accountLabel(a Account, width int) string {
	if !a.Active {
		return safe(a.Name())
	}
	return lipgloss.NewStyle().Bold(true).Foreground(amber).Render(safe(a.Name()))
}

func codexWeekly(a Account) *Window {
	for _, p := range pairs(a.Windows) {
		if strings.EqualFold(p.name, "Codex") {
			return p.weekly
		}
	}
	return nil
}
func displayedWindows(a Account) []Window {
	if a.Provider != "codex" {
		return a.Windows
	}
	if w := codexWeekly(a); w != nil {
		return []Window{*w}
	}
	return nil
}
