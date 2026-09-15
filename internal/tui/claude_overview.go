package tui

import (
	"strings"
	"time"
)

func claudeWindows(a Account) (five, weekly, fable *Window) {
	for _, p := range pairs(a.Windows) {
		if strings.EqualFold(p.name, "All models") {
			five = p.five
			weekly = p.weekly
		}
		if strings.Contains(strings.ToLower(p.name), "fable") {
			fable = p.weekly
		}
	}
	return
}
func (m *Model) claudeOverview(w, h int) string {
	rows := m.filtered()
	inner := w - 4
	wide := w >= 96
	accountW := min(24, inner/4)
	cellW := (inner - accountW) / 3
	type line struct {
		text    string
		account int
	}
	all := []line{}
	selectedStart := 0
	now := time.Now()
	header := fit("Account", accountW) + fit("5h left / reset", cellW) + fit("Weekly left / reset", cellW) + "Fable weekly left"
	if !wide {
		header = "Account / window · remaining quota"
	}
	for i, a := range rows {
		if i > 0 {
			all = append(all, line{ink(strings.Repeat("─", inner), border), -1})
		}
		if i == m.cursor {
			selectedStart = len(all)
		}
		name := accountLabel(a, accountW)
		if a.Error != "" || len(a.Windows) == 0 {
			status := "Not checked"
			if a.Error != "" {
				status = "Read failed · Enter for error"
				if strings.Contains(a.Error, "Access token expired") {
					status = "Token expired · refresh needed"
				}
				if strings.Contains(a.Error, "429") {
					status = safe(a.Error)
				}
				if strings.Contains(strings.ToLower(a.Error), "rejected") {
					status = "Sign in required"
				}
			}
			all = append(all, line{fit(name, accountW) + status, i})
			all = append(all, line{accountMeta(a, 0), i})
			continue
		}
		five, weekly, fable := claudeWindows(a)
		if wide {
			text := fit(name, accountW)
			reset := fit(accountMeta(a, 0), accountW)
			for _, q := range []*Window{five, weekly, fable} {
				text += fit(quotaCell(q, min(36, cellW-2)), cellW)
				reset += fit(resetCell(q, now), cellW)
			}
			all = append(all, line{text, i}, line{reset, i})
		} else {
			all = append(all, line{name, i})
			for _, period := range []struct {
				name string
				q    *Window
			}{{"5h", five}, {"Weekly", weekly}, {"Fable weekly", fable}} {
				all = append(all, line{fit(" "+period.name, 15) + quotaCell(period.q, min(24, inner-16)), i})
				if period.q != nil {
					all = append(all, line{" " + resetCell(period.q, now), i})
				}
			}
		}
	}
	visible := max(1, h-4)
	start := max(0, selectedStart-visible+min(2, visible))
	lines := []string{" " + ink(header, muted), " " + ink(strings.Repeat("─", inner), border)}
	for _, row := range all[start:min(len(all), start+visible)] {
		prefix := " "
		if row.account == m.cursor {
			prefix = ink("›", accent)
		}
		lines = append(lines, prefix+fit(row.text, inner))
	}
	if len(rows) == 0 {
		lines = append(lines, " No matching accounts")
	}
	return frame("", strings.Join(lines, "\n"), w, h, true)
}
