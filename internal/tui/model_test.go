package tui

import (
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"context"
	"errors"
	"github.com/charmbracelet/x/ansi"
	"strings"
	"testing"
	"time"
)

type fakeBackend struct {
	actions int
	last    string
}

func (f *fakeBackend) Snapshot(context.Context, bool) (Snapshot, error) { return DemoData(), nil }
func (f *fakeBackend) Action(_ context.Context, a Account, op string) error {
	f.actions++
	f.last = a.ID() + "/" + op
	return nil
}
func key(m *Model, k string) tea.Cmd {
	_, c := m.Update(tea.KeyPressMsg(tea.Key{Code: []rune(k)[0], Text: k}))
	return c
}
func special(m *Model, k rune) tea.Cmd { _, c := m.Update(tea.KeyPressMsg(tea.Key{Code: k})); return c }
func TestRenderingFits(t *testing.T) {
	for _, size := range [][2]int{{48, 16}, {60, 24}, {89, 30}, {90, 24}, {110, 32}, {160, 48}, {30, 10}} {
		m := Demo(context.Background())
		m.data.Accounts[0].Alias = "工作 🚀 very long Unicode account label"
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		for _, mode := range []string{"normal", "help", "confirm"} {
			m.help = mode == "help"
			if mode == "confirm" {
				a := m.data.Accounts[0]
				m.confirmation = &a
			} else {
				m.confirmation = nil
			}
			v := m.View().Content
			if lipgloss.Height(v) > size[1] {
				t.Fatalf("%v %s height %d", size, mode, lipgloss.Height(v))
			}
			for _, line := range strings.Split(v, "\n") {
				if lipgloss.Width(line) > size[0] {
					t.Fatalf("%v %s overflow %d", size, mode, lipgloss.Width(line))
				}
			}
		}
	}
}
func TestFilteringAndProviderTabs(t *testing.T) {
	m := Demo(context.Background())
	key(m, "2")
	if len(m.filtered()) != 2 {
		t.Fatal("Claude tab")
	}
	key(m, "/")
	key(m, "s")
	key(m, "t")
	if len(m.filtered()) != 1 || m.filtered()[0].Alias != "studio" {
		t.Fatal("filter")
	}
	special(m, tea.KeyEscape)
	if len(m.filtered()) != 2 {
		t.Fatal("escape did not clear")
	}
}
func TestRefreshPreservesSelectionAndFailureData(t *testing.T) {
	m := Demo(context.Background())
	key(m, "j")
	wanted, _ := m.selected()
	data := DemoData()
	data.Accounts[0], data.Accounts[1] = data.Accounts[1], data.Accounts[0]
	m.Update(snapshotMsg{value: data, live: true})
	got, _ := m.selected()
	if got.ID() != wanted.ID() {
		t.Fatal("selection moved")
	}
	m.Update(snapshotMsg{err: errors.New("offline"), live: true})
	if len(m.data.Accounts) != 4 || !strings.Contains(m.notice, "previous") {
		t.Fatal("failed refresh erased data")
	}
}
func TestBusyRefreshCannotOverlap(t *testing.T) {
	f := &fakeBackend{}
	m := New(context.Background(), f)
	if key(m, "r") != nil {
		t.Fatal("overlapping load")
	}
	m.busy = false
	if key(m, "r") == nil || !m.busy {
		t.Fatal("refresh missing")
	}
}
func TestSwitchRequiresExplicitConfirmation(t *testing.T) {
	f := &fakeBackend{}
	m := New(context.Background(), f)
	m.busy = false
	m.data = DemoData()
	key(m, "s")
	if m.confirmation == nil || f.actions != 0 {
		t.Fatal("switch not guarded")
	}
	special(m, tea.KeyEscape)
	if m.confirmation != nil {
		t.Fatal("cancel failed")
	}
	key(m, "s")
	cmd := key(m, "y")
	if cmd == nil {
		t.Fatal("confirm missing")
	}
	batch := cmd().(tea.BatchMsg)
	for _, c := range batch {
		c()
	}
	if f.actions != 1 || f.last != "codex:work/switch" {
		t.Fatal("wrong action", f.last)
	}
}
func TestDemoNeverMutates(t *testing.T) {
	m := Demo(context.Background())
	key(m, "s")
	key(m, "y")
	key(m, "l")
	if m.confirmation != nil || m.busy {
		t.Fatal("demo enabled action")
	}
}
func TestUnknownAndPartialLimits(t *testing.T) {
	a := Account{}
	if a.Status() != "Unknown" {
		t.Fatal(a.Status())
	}
	a.CheckedAt = 1
	a.Windows = []Window{{Label: "Codex", Used: 1}, {Label: "Spark", Used: .2}}
	if a.Status() != "Some limits" {
		t.Fatal(a.Status())
	}
	a.Error = "revoked"
	if a.Status() != "Check failed" {
		t.Fatal(a.Status())
	}
}
func TestEscapeSequencesNeverReachRenderedLabels(t *testing.T) {
	m := Demo(context.Background())
	m.data.Accounts[0].Email = "bad\x1b[2J\x1b]52;c;ZXZpbA==\a\nemail"
	v := m.View().Content
	if strings.Contains(v, "\x1b[2J") || strings.Contains(v, "\x1b]52") {
		t.Fatal("untrusted controls rendered")
	}
}
func TestEmptyAccounts(t *testing.T) {
	m := Demo(context.Background())
	m.data.Accounts = nil
	key(m, "j")
	key(m, "s")
	if !strings.Contains(m.View().Content, "No matching accounts") {
		t.Fatal("missing empty state")
	}
}

func TestNewSessionShortcutUsesSelectedAccount(t *testing.T) {
	f := &fakeBackend{}
	m := New(context.Background(), f)
	m.busy = false
	m.data = DemoData()
	key(m, "j")
	selected, _ := m.selected()
	cmd := key(m, "n")
	if cmd == nil {
		t.Fatal("N did not launch selected account")
	}
	for _, c := range cmd().(tea.BatchMsg) {
		c()
	}
	if f.last != selected.ID()+"/launch" {
		t.Fatal(f.last)
	}
}
func TestDefaultBadgeIsIndependentOfSelection(t *testing.T) {
	m := Demo(context.Background())
	key(m, "j")
	v := m.View().Content
	if strings.Contains(v, "Default account:") || strings.Contains(v, "DEFAULT") {
		t.Fatal("obsolete default label")
	}
	if !strings.Contains(v, accountLabel(m.data.Accounts[0], 25)) {
		t.Fatal("missing yellow active name")
	}
	if !m.data.Accounts[0].Active || m.data.Accounts[1].Active {
		t.Fatal("selection changed active identity")
	}
}

func TestMinimalChromeAndAutomaticRefresh(t *testing.T) {
	m := Demo(context.Background())
	v := m.View().Content
	for _, bad := range []string{"HOTSEAT", "Default account:", "/ Filter", " accounts ·", "CEST", "Missing windows", "refresh with r"} {
		if strings.Contains(v, bad) {
			t.Errorf("unwanted label %q", bad)
		}
	}
	if !strings.Contains(v, "[1] Codex") || !strings.Contains(v, "[2] Claude") {
		t.Fatal("missing bracketed tabs")
	}
	key(m, "/")
	if !strings.Contains(ansi.Strip(m.View().Content), "Filter accounts") {
		t.Fatal("filter input should appear on demand")
	}
	f := &fakeBackend{}
	live := New(context.Background(), f)
	live.busy = false
	live.Update(autoRefreshMsg{})
	if !live.busy {
		t.Fatal("timer did not request refresh")
	}
	if refreshInterval != 15*time.Second {
		t.Fatal("wrong refresh interval")
	}
	live.busy = false
	a := DemoData().Accounts[0]
	live.confirmation = &a
	live.Update(autoRefreshMsg{})
	if live.busy {
		t.Fatal("timer interrupted confirmation")
	}
}
