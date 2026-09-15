package tui

import (
	tea "charm.land/bubbletea/v2"
	"context"
	"github.com/charmbracelet/x/ansi"
	"strings"
	"testing"
	"time"
)

func TestOverviewShowsActualRemainingAndEveryReset(t *testing.T) {
	m := Demo(context.Background())
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	v := ansi.Strip(m.View().Content)
	for _, want := range []string{"73.0%", "0.0%", "Weekly left", "3d 0h"} {
		if !strings.Contains(v, want) {
			t.Errorf("missing %s", want)
		}
	}
	for _, bad := range []string{"Some limits", "account desk", "NORMAL", "developer@example.com"} {
		if strings.Contains(v, bad) {
			t.Errorf("unexpected overview text %s", bad)
		}
	}
}
func TestResetCountdownAndExpiredReading(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.Local)
	date, left := resetText(float64(now.Add(49*time.Hour).Unix()), now)
	if date != "17 Sep 13:00" || left != "2d 1h" {
		t.Fatal(date, left)
	}
	_, left = resetText(float64(now.Add(-time.Minute).Unix()), now)
	if left != "due; refresh" {
		t.Fatal(left)
	}
	date, left = resetText(0, now)
	if date != "—" || left != "—" {
		t.Fatal("invented reset")
	}
}
func TestRemainingNeverNegative(t *testing.T) {
	if remaining(1.1) != 0 || remaining(.27) != 73 || remaining(0) != 100 {
		t.Fatal("remaining calculation")
	}
}
func TestNarrowViewKeepsExactReset(t *testing.T) {
	m := Demo(context.Background())
	m.Update(tea.WindowSizeMsg{Width: 60, Height: 36})
	v := ansi.Strip(m.View().Content)
	date, _ := resetText(m.data.Accounts[0].Windows[0].Reset, time.Now())
	if !strings.Contains(v, date) {
		t.Fatal("missing reset timestamp")
	}
}
func TestDetailsStillShowsIdentity(t *testing.T) {
	m := Demo(context.Background())
	special(m, tea.KeyEnter)
	if !strings.Contains(m.View().Content, "developer@example.com") {
		t.Fatal("missing identity")
	}
	special(m, tea.KeyEscape)
	if strings.Contains(m.View().Content, "developer@example.com") {
		t.Fatal("did not return to overview")
	}
}

func TestPairedWindowsKeepTheirOwnUsageAndResets(t *testing.T) {
	q := pairs([]Window{{Label: "Codex primary 5-hour", Used: .2, Reset: 100}, {Label: "Codex secondary weekly", Used: .8, Reset: 200}, {Label: "Spark primary weekly", Used: .4, Reset: 300}})
	if len(q) != 2 || q[0].five == nil || q[0].weekly == nil || q[0].five.Used != .2 || q[0].weekly.Reset != 200 {
		t.Fatal(q)
	}
	if q[1].five != nil || q[1].weekly == nil {
		t.Fatal("invented 5h window")
	}
}
func TestResetCreditZeroVersusUnknown(t *testing.T) {
	zero := 0
	two := 2
	for _, c := range []struct {
		a    Account
		want string
	}{{Account{Provider: "codex"}, "?"}, {Account{Provider: "codex", ResetCredits: &zero}, "0"}, {Account{Provider: "codex", ResetCredits: &two}, "2"}, {Account{Provider: "claude"}, "—"}} {
		if creditText(c.a) != c.want {
			t.Fatal(c)
		}
	}
}

func TestSeparateProviderTabsAndClaudeFable(t *testing.T) {
	m := Demo(context.Background())
	if m.provider != 1 || len(m.filtered()) != 2 {
		t.Fatal("must start on Codex")
	}
	v := ansi.Strip(m.View().Content)
	if strings.Contains(v, "1 All") || strings.Contains(v, "CL/") {
		t.Fatal("mixed providers")
	}
	key(m, "2")
	v = ansi.Strip(m.View().Content)
	for _, want := range []string{"Fable weekly", "82.0%", "38.0%", "16.0%"} {
		if !strings.Contains(v, want) {
			t.Errorf("missing Claude field %s", want)
		}
	}
	if strings.Contains(v, "73.0%") {
		t.Fatal("Codex usage leaked into Claude")
	}
	a := Account{Windows: []Window{{Label: "All models · weekly", Used: .4}, {Label: "Fable · weekly", Used: .9}}}
	five, weekly, fable := claudeWindows(a)
	if five != nil || weekly == nil || weekly.Used != .4 || fable == nil || fable.Used != .9 {
		t.Fatal("incorrect Claude quota grouping")
	}
}

func TestClaudeLayoutFitsSmallTerminals(t *testing.T) {
	for _, size := range [][2]int{{48, 16}, {60, 24}, {96, 24}, {110, 36}} {
		m := Demo(context.Background())
		key(m, "2")
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		v := ansi.Strip(m.View().Content)
		for _, line := range strings.Split(v, "\n") {
			if len([]rune(line)) > size[0] {
				t.Fatalf("Claude width overflow %v", size)
			}
		}
		if len(strings.Split(v, "\n")) > size[1] {
			t.Fatal("Claude height overflow")
		}
	}
}

func TestClaudeMultiplierVisibleEvenWhenQuotaReadFails(t *testing.T) {
	m := Demo(context.Background())
	key(m, "2")
	m.data.Accounts[2].Plan = "Max 20×"
	m.data.Accounts[3].Plan = "Team 5×"
	v := ansi.Strip(m.View().Content)
	if !strings.Contains(v, "Max 20×") || !strings.Contains(v, "Team 5×") {
		t.Fatal("missing subscription multipliers")
	}
}

func TestCodexOnlyShowsRegularWeeklyQuota(t *testing.T) {
	m := Demo(context.Background())
	v := ansi.Strip(m.View().Content)
	for _, bad := range []string{"Spark", "5h", "88.0%", "42.0%", "55.0%"} {
		if strings.Contains(v, bad) {
			t.Errorf("unwanted Codex data %s", bad)
		}
	}
	a := Account{Provider: "codex", Windows: []Window{{Label: "Spark · weekly", Used: .4}}}
	if codexWeekly(a) != nil || len(displayedWindows(a)) != 0 {
		t.Fatal("Spark substituted for regular weekly quota")
	}
	key(m, "2")
	v = ansi.Strip(m.View().Content)
	if !strings.Contains(v, "5h left") || !strings.Contains(v, "Fable weekly") {
		t.Fatal("Claude columns changed")
	}
}
