package tui

import (
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"context"
	"strings"
	"testing"
)

type fakeWorkBackend struct {
	fakeBackend
	calls int
	op    string
	id    string
}

func (f *fakeWorkBackend) Work(context.Context) (WorkSnapshot, error)                  { return demoWork(), nil }
func (f *fakeWorkBackend) InspectWork(_ context.Context, w WorkItem) (WorkItem, error) { return w, nil }
func (f *fakeWorkBackend) WorkAction(_ context.Context, w WorkItem, op string) error {
	f.calls++
	f.op = op
	f.id = w.ID
	return nil
}
func TestWorkScreenIsSeparateAndEscapeRestoresAccounts(t *testing.T) {
	m := Demo(context.Background())
	if strings.Contains(m.View().Content, "Finish the database") {
		t.Fatal("work cluttered accounts")
	}
	key(m, "w")
	if !strings.Contains(m.View().Content, "Finish the database") {
		t.Fatal("work missing")
	}
	special(m, tea.KeyEnter)
	if m.workDetail == nil {
		t.Fatal("inspection missing")
	}
	special(m, tea.KeyEscape)
	if m.workDetail != nil || !m.workMode {
		t.Fatal("escape should return to work list")
	}
	special(m, tea.KeyEscape)
	if m.workMode {
		t.Fatal("escape should return to accounts")
	}
}
func TestWorkActionRequiresConfirmation(t *testing.T) {
	b := &fakeWorkBackend{}
	m := New(context.Background(), b)
	m.workMode = true
	m.workData = demoWork()
	key(m, "c")
	if m.workConfirmation == nil || b.calls != 0 {
		t.Fatal("missing confirmation")
	}
	special(m, tea.KeyEscape)
	if b.calls != 0 {
		t.Fatal("cancel launched")
	}
	key(m, "c")
	cmd := key(m, "y")
	cmd()
	if b.calls != 1 || b.id != "demo-codex" || b.op != "resume" {
		t.Fatal("wrong session action")
	}
}
func TestWorkViewsFitTerminal(t *testing.T) {
	for _, s := range [][2]int{{48, 16}, {60, 24}, {110, 36}, {160, 45}} {
		m := Demo(context.Background())
		m.Update(tea.WindowSizeMsg{Width: s[0], Height: s[1]})
		key(m, "w")
		for i := 0; i < 3; i++ {
			if i == 1 {
				item, _ := m.selectedWork()
				m.workDetail = &item
			}
			if i == 2 {
				item, _ := m.selectedWork()
				m.workConfirmation = &workConfirm{item, "reboot"}
			}
			v := m.View().Content
			if lipgloss.Width(v) > s[0] || lipgloss.Height(v) > s[1] {
				t.Fatalf("work overflow %v", s)
			}
		}
	}
}
func TestStaleInspectReplyDoesNotReplaceNewSelection(t *testing.T) {
	m := Demo(context.Background())
	key(m, "w")
	old, _ := m.selectedWork()
	key(m, "2")
	m.Update(inspectWorkMsg{item: old})
	if m.workDetail != nil {
		t.Fatal("stale inspect result shown")
	}
}
