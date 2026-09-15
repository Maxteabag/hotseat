package tui

import (
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type WorkItem struct {
	ID            string  `json:"id"`
	Provider      string  `json:"provider"`
	Title         string  `json:"title"`
	State         string  `json:"state"`
	CWD           string  `json:"cwd"`
	Updated       float64 `json:"updated"`
	Live          bool    `json:"live"`
	CanResume     bool    `json:"can_resume"`
	CanReboot     bool    `json:"can_reboot"`
	Reason        string  `json:"reason"`
	Revision      string  `json:"revision"`
	LastUser      string  `json:"last_user"`
	LastAssistant string  `json:"last_assistant"`
	Model         string  `json:"model"`
}

func (w WorkItem) key() string { return w.Provider + ":" + w.ID }

type WorkSnapshot struct {
	Items  []WorkItem `json:"items"`
	Errors []string   `json:"errors"`
}
type WorkBackend interface {
	Work(context.Context) (WorkSnapshot, error)
	InspectWork(context.Context, WorkItem) (WorkItem, error)
	WorkAction(context.Context, WorkItem, string) error
}

func (b *PythonBackend) Work(ctx context.Context) (WorkSnapshot, error) {
	var s WorkSnapshot
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	out, err := b.execute(ctx, "work")
	if err == nil {
		err = json.Unmarshal(out, &s)
	}
	return s, err
}
func (b *PythonBackend) InspectWork(ctx context.Context, w WorkItem) (WorkItem, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	out, err := b.execute(ctx, "inspect-work", "--provider", w.Provider, "--id", w.ID)
	if err != nil {
		return w, responseError(out, err)
	}
	err = json.Unmarshal(out, &w)
	return w, err
}
func (b *PythonBackend) WorkAction(ctx context.Context, w WorkItem, operation string) error {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	out, err := b.execute(ctx, operation+"-work", "--provider", w.Provider, "--id", w.ID, "--revision", w.Revision, "--acknowledged")
	return responseError(out, err)
}
func responseError(out []byte, err error) error {
	var r struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(out, &r) == nil && r.Error != "" {
		return fmt.Errorf("%s", r.Error)
	}
	return err
}

type workMsg struct {
	snapshot WorkSnapshot
	err      error
}
type inspectWorkMsg struct {
	item WorkItem
	err  error
}
type workActionMsg struct{ err error }
type workConfirm struct {
	item      WorkItem
	operation string
}

func (m *Model) workRows() []WorkItem {
	rows := []WorkItem{}
	provider := "codex"
	if m.provider == 2 {
		provider = "claude"
	}
	for _, w := range m.workData.Items {
		if w.Provider != provider {
			continue
		}
		if !m.workAll && w.State != "failed" && w.State != "stuck" {
			continue
		}
		rows = append(rows, w)
	}
	return rows
}
func (m *Model) selectedWork() (WorkItem, bool) {
	rows := m.workRows()
	if len(rows) == 0 {
		return WorkItem{}, false
	}
	m.workCursor = min(max(0, m.workCursor), len(rows)-1)
	return rows[m.workCursor], true
}
func (m *Model) loadWork() tea.Cmd {
	if m.workBusy {
		return nil
	}
	if m.demo {
		if len(m.workData.Items) == 0 {
			m.workData = demoWork()
		}
		m.workNotice = "Demo sessions · actions disabled"
		return nil
	}
	backend, ok := m.backend.(WorkBackend)
	if !ok {
		m.workNotice = "Work inspection is unavailable"
		return nil
	}
	m.workBusy = true
	m.workNotice = "Reading sessions…"
	return func() tea.Msg { s, e := backend.Work(m.ctx); return workMsg{s, e} }
}
func (m *Model) workKey(key string) (tea.Model, tea.Cmd) {
	if m.workConfirmation != nil {
		if key == "esc" || key == "n" {
			m.workConfirmation = nil
			return m, nil
		}
		if key == "y" && !m.workBusy {
			pending := *m.workConfirmation
			m.workConfirmation = nil
			m.workBusy = true
			m.workNotice = "Opening the same conversation…"
			return m, func() tea.Msg {
				return workActionMsg{m.backend.(WorkBackend).WorkAction(m.ctx, pending.item, pending.operation)}
			}
		}
		return m, nil
	}
	switch key {
	case "q":
		return m, tea.Quit
	case "esc":
		if m.workDetail != nil {
			m.workDetail = nil
			m.workScroll = 0
		} else {
			m.workMode = false
		}
		return m, nil
	case "w":
		m.workMode = false
		return m, nil
	case "1", "2":
		m.provider = int(key[0] - '0')
		m.workCursor = 0
		m.workDetail = nil
		m.workScroll = 0
	case "a":
		m.workAll = !m.workAll
		m.workCursor = 0
		m.workDetail = nil
	case "j", "down":
		if m.workDetail != nil {
			m.workScroll++
		} else {
			m.workCursor = min(m.workCursor+1, max(0, len(m.workRows())-1))
		}
	case "k", "up":
		if m.workDetail != nil {
			m.workScroll = max(0, m.workScroll-1)
		} else {
			m.workCursor = max(0, m.workCursor-1)
		}
	case "r":
		return m, m.loadWork()
	case "enter":
		item, ok := m.selectedWork()
		if !ok || m.workBusy {
			return m, nil
		}
		if m.demo {
			m.workDetail = &item
			return m, nil
		}
		m.workBusy = true
		return m, func() tea.Msg { w, e := m.backend.(WorkBackend).InspectWork(m.ctx, item); return inspectWorkMsg{w, e} }
	case "c", "b":
		item, ok := m.selectedWork()
		if !ok || m.workBusy {
			return m, nil
		}
		if m.workDetail != nil {
			item = *m.workDetail
		}
		operation := "resume"
		allowed := item.CanResume
		if key == "b" {
			operation = "reboot"
			allowed = item.CanReboot
		}
		if m.demo {
			m.workNotice = "Demo: session actions disabled"
			return m, nil
		}
		if !allowed {
			m.workNotice = item.Reason
			if m.workNotice == "" {
				m.workNotice = "Action unavailable for this session state"
			}
			return m, nil
		}
		m.workConfirmation = &workConfirm{item, operation}
	}
	return m, nil
}
func (m *Model) workView() tea.View {
	terminalW := m.width
	w, h := min(m.width, 140), m.height
	if w < 48 || h < 16 {
		v := tea.NewView(fit("Enlarge terminal to 48 × 16", w))
		v.AltScreen = true
		return v
	}
	tabs := []string{"[1] Codex", "[2] Claude"}
	for i := range tabs {
		c := muted
		if i+1 == m.provider {
			c = accent
		}
		tabs[i] = ink(tabs[i], c)
	}
	filter := "Stopped"
	if m.workAll {
		filter = "Recent"
	}
	header := fit(" "+strings.Join(tabs, "    ")+"    "+filter, w)
	contentHeight := h - 3
	var body string
	if m.workConfirmation != nil {
		p := m.workConfirmation
		verb := "Resume"
		explanation := "Opens this conversation in a new terminal using the current default account."
		if p.operation == "reboot" {
			verb = "Reboot"
			explanation = "Stops the exact owning process, waits for it to exit, then reopens the same conversation using the current default account."
		}
		text := "\n " + verb + " " + safe(p.item.Provider) + " session?\n\n " + safe(p.item.Title) + "\n " + safe(p.item.ID) + "\n " + safe(p.item.CWD) + "\n\n " + lipgloss.NewStyle().Width(w-6).Render(explanation) + "\n\n y Confirm    n / Esc Cancel"
		body = frame("", text, w, contentHeight, true)
	} else if m.workDetail != nil {
		item := m.workDetail
		sections := []string{safe(item.Title), "", item.Provider + " · " + item.State, "Session: " + safe(item.ID), "Directory: " + safe(item.CWD), "Model: " + safe(item.Model), "", "Last request", safe(item.LastUser), "", "Recent output", safe(item.LastAssistant)}
		if item.Reason != "" {
			sections = append(sections, "", safe(item.Reason))
		}
		lines := strings.Split(lipgloss.NewStyle().Width(w-6).Render(strings.Join(sections, "\n")), "\n")
		m.workScroll = min(m.workScroll, max(0, len(lines)-(contentHeight-2)))
		body = frame("", strings.Join(lines[m.workScroll:], "\n"), w, contentHeight, true)
	} else {
		rows := m.workRows()
		inner := w - 4
		stateW, idW := 10, 10
		timeW := 13
		titleW := max(10, inner-stateW-idW-timeW)
		lines := []string{" " + ink(fit("State", stateW)+fit("Session", idW)+fit("Updated", timeW)+"Work", muted), " " + ink(strings.Repeat("─", inner), border)}
		visible := max(1, contentHeight-4)
		start := max(0, m.workCursor-visible+1)
		for i := start; i < min(len(rows), start+visible); i++ {
			item := rows[i]
			prefix := " "
			if i == m.workCursor {
				prefix = ink("›", accent)
			}
			col := fg
			if item.State == "failed" || item.State == "stuck" {
				col = amber
			}
			id := item.ID
			if len(id) > 8 {
				id = id[:8]
			}
			lines = append(lines, prefix+fit(ink(item.State, col), stateW)+fit(id, idW)+fit(time.Unix(int64(item.Updated), 0).Format("02 Jan 15:04"), timeW)+fit(safe(item.Title), titleW))
		}
		if len(rows) == 0 {
			lines = append(lines, " No stopped sessions. Press a for recent sessions.")
		}
		body = frame("", strings.Join(lines, "\n"), w, contentHeight, true)
	}
	footer := " j/k select  Enter inspect  c resume  b reboot  a recent/stopped  r refresh  Esc accounts"
	if w < 95 {
		footer = " j/k  Enter inspect  c resume  b reboot  a all  Esc"
	}
	screen := header + "\n" + body + "\n" + fit(" "+safe(m.workNotice), w) + "\n" + fit(footer, w)
	if terminalW > w {
		screen = lipgloss.NewStyle().Width(terminalW).Align(lipgloss.Center).Render(screen)
	}
	v := tea.NewView(screen)
	v.AltScreen = true
	v.WindowTitle = "Hotseat"
	v.BackgroundColor = bg
	v.ForegroundColor = fg
	return v
}
func demoWork() WorkSnapshot {
	return WorkSnapshot{Items: []WorkItem{
		{ID: "demo-codex", Provider: "codex", Title: "Finish the database migration", State: "failed", CWD: "/work/project", Updated: float64(time.Now().Unix()), CanResume: true, LastUser: "Finish the migration and run the tests.", LastAssistant: "Stopped after reaching the usage limit."},
		{ID: "demo-claude", Provider: "claude", Title: "Review the release changes", State: "stuck", CWD: "/work/project", Updated: float64(time.Now().Unix()), CanReboot: true, Live: true, LastUser: "Review changes for the release.", LastAssistant: "Waiting for quota."},
	}}
}
