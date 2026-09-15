package tui

import (
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"context"
	"fmt"
	"github.com/charmbracelet/x/ansi"
	"image/color"
	"strings"
	"time"
	"unicode"
)

var (
	bg     = lipgloss.Color("#171B24")
	fg     = lipgloss.Color("#D6DCE8")
	muted  = lipgloss.Color("#8490A5")
	accent = lipgloss.Color("#88C0D0")
	border = lipgloss.Color("#394456")
	green  = lipgloss.Color("#A3BE8C")
	amber  = lipgloss.Color("#EBCB8B")
	red    = lipgloss.Color("#BF616A")
)

func ink(s string, c color.Color) string { return lipgloss.NewStyle().Foreground(c).Render(s) }

// Strip terminal controls in every backend-provided label before rendering.
func safe(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, ansi.Strip(s))
}
func fit(s string, w int) string {
	w = max(0, w)
	s = ansi.Truncate(s, w, "…")
	return s + strings.Repeat(" ", max(0, w-lipgloss.Width(s)))
}
func frame(title, body string, w, h int, focused bool) string {
	if w < 2 || h < 2 {
		return ""
	}
	col := border
	if focused {
		col = accent
	}
	inner := w - 2
	titleColor := muted
	if focused {
		titleColor = accent
	}
	lines := []string{}
	if title != "" {
		lines = append(lines, ink(" "+title, titleColor))
	}
	lines = append(lines, strings.Split(body, "\n")...)
	for len(lines) < h-2 {
		lines = append(lines, "")
	}
	lines = lines[:min(len(lines), h-2)]
	for i := range lines {
		lines[i] = fit(lines[i], inner)
	}
	return lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(col).Render(strings.Join(lines, "\n"))
}

type clockMsg struct{}
type autoRefreshMsg struct{}

const refreshInterval = 15 * time.Second

func autoRefreshTick() tea.Cmd {
	return tea.Tick(refreshInterval, func(time.Time) tea.Msg { return autoRefreshMsg{} })
}

func clockTick() tea.Cmd { return tea.Tick(time.Minute, func(time.Time) tea.Msg { return clockMsg{} }) }

type snapshotMsg struct {
	value Snapshot
	err   error
	live  bool
}
type actionMsg struct {
	err              error
	alias, operation string
}
type Model struct {
	workMode, workBusy, workAll bool
	workCursor, workScroll      int
	workData                    WorkSnapshot
	workDetail                  *WorkItem
	workConfirmation            *workConfirm
	workNotice                  string

	ctx                                            context.Context
	backend                                        Backend
	data                                           Snapshot
	width, height, cursor, focus, scroll, provider int
	tableOffset                                    int
	busy                                           bool
	filtering                                      bool
	help                                           bool
	demo                                           bool
	confirmation                                   *Account
	notice                                         string
	filter                                         textinput.Model
	spinner                                        spinner.Model
}

func New(ctx context.Context, b Backend) *Model {
	input := textinput.New()
	input.Prompt = "/ "
	input.Placeholder = "Filter accounts…"
	input.CharLimit = 100
	input.SetWidth(102)
	input.SetVirtualCursor(true)
	return &Model{ctx: ctx, backend: b, width: 110, height: 32, provider: 1, busy: true, filter: input, spinner: spinner.New(spinner.WithSpinner(spinner.MiniDot), spinner.WithStyle(lipgloss.NewStyle().Foreground(accent)))}
}
func Demo(ctx context.Context) *Model {
	m := New(ctx, nil)
	m.demo = true
	m.busy = false
	m.data = DemoData()
	m.notice = "Demo data · account actions disabled"
	return m
}

// DemoWithData renders supplied fixtures without backend calls or account actions.
func DemoWithData(ctx context.Context, data Snapshot, work WorkSnapshot) *Model {
	m := Demo(ctx)
	m.data = data
	m.workData = work
	return m
}
func (m *Model) Init() tea.Cmd {
	if m.demo {
		return nil
	}
	return tea.Batch(m.load(false), m.spinner.Tick, clockTick(), autoRefreshTick())
}
func (m *Model) load(live bool) tea.Cmd {
	return func() tea.Msg { s, e := m.backend.Snapshot(m.ctx, live); return snapshotMsg{s, e, live} }
}
func (m *Model) filtered() []Account {
	out := []Account{}
	query := strings.ToLower(m.filter.Value())
	for _, a := range m.data.Accounts {
		if m.provider == 1 && a.Provider != "codex" || m.provider == 2 && a.Provider != "claude" {
			continue
		}
		if !strings.Contains(strings.ToLower(a.Name()+" "+strings.Join(a.Aliases, " ")+" "+a.Email+" "+a.Provider+" "+a.Workspace), query) {
			continue
		}
		out = append(out, a)
	}
	return out
}
func (m *Model) selected() (Account, bool) {
	rows := m.filtered()
	if len(rows) == 0 {
		return Account{}, false
	}
	m.cursor = min(max(m.cursor, 0), len(rows)-1)
	return rows[m.cursor], true
}
func (m *Model) refresh() tea.Cmd {
	if m.busy || m.demo {
		return nil
	}
	m.busy = true
	m.notice = "Reading account quotas…"
	return tea.Batch(m.load(true), m.spinner.Tick)
}
func (m *Model) runAction(a Account, operation string) tea.Cmd {
	m.busy = true
	m.notice = operation + " · " + a.Alias
	return tea.Batch(m.spinner.Tick, func() tea.Msg { return actionMsg{m.backend.Action(m.ctx, a, operation), a.Alias, operation} })
}
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case workMsg:
		previous, _ := m.selectedWork()
		m.workBusy = false
		if msg.err != nil {
			m.workNotice = "Read failed: " + msg.err.Error()
			return m, nil
		}
		m.workData = msg.snapshot
		m.workCursor = 0
		for i, w := range m.workRows() {
			if w.key() == previous.key() {
				m.workCursor = i
				break
			}
		}
		m.workNotice = strings.Join(msg.snapshot.Errors, " · ")
		return m, nil
	case inspectWorkMsg:
		m.workBusy = false
		if msg.err != nil {
			m.workNotice = msg.err.Error()
		} else {
			selected, ok := m.selectedWork()
			if m.workMode && ok && selected.key() == msg.item.key() {
				m.workDetail = &msg.item
				m.workScroll = 0
			}
		}
		return m, nil
	case workActionMsg:
		m.workBusy = false
		if msg.err != nil {
			m.workNotice = msg.err.Error()
			return m, nil
		}
		m.workNotice = "Session opened in a new terminal"
		m.workDetail = nil
		return m, m.loadWork()

	case clockMsg:
		return m, clockTick()
	case autoRefreshMsg:
		if m.workMode {
			if m.workConfirmation != nil || m.workDetail != nil {
				return m, autoRefreshTick()
			}
			return m, tea.Batch(autoRefreshTick(), m.loadWork())
		}
		if m.demo {
			return m, nil
		}
		if m.confirmation != nil {
			return m, autoRefreshTick()
		}
		return m, tea.Batch(autoRefreshTick(), m.refresh())
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.filter.SetWidth(max(8, msg.Width-8))
		return m, nil
	case snapshotMsg:
		previous, _ := m.selected()
		m.busy = false
		if msg.err != nil {
			m.notice = "Refresh failed · " + safe(msg.err.Error()) + " · showing previous readings"
			return m, nil
		}
		m.data = msg.value
		m.cursor = 0
		for i, a := range m.filtered() {
			if a.ID() == previous.ID() {
				m.cursor = i
				break
			}
		}
		m.notice = "Updated " + time.Now().Format("15:04:05")
		if len(m.data.Errors) > 0 {
			m.notice = strings.Join(m.data.Errors, " · ")
		}
		if !msg.live {
			return m, m.refresh()
		}
		return m, nil
	case actionMsg:
		m.busy = false
		if msg.err != nil {
			m.notice = "Action failed · " + safe(msg.err.Error())
			return m, nil
		}
		m.notice = msg.operation + " completed · " + msg.alias
		return m, m.refresh()
	case spinner.TickMsg:
		if m.busy {
			var cmd tea.Cmd
			m.spinner, cmd = m.spinner.Update(msg)
			return m, cmd
		}
		return m, nil
	case tea.KeyPressMsg:
		key := msg.String()
		if key == "ctrl+c" {
			return m, tea.Quit
		}
		if m.workMode {
			return m.workKey(key)
		}
		if m.confirmation != nil {
			if key == "esc" || key == "n" {
				m.confirmation = nil
				return m, nil
			}
			if key == "y" && !m.busy {
				a := *m.confirmation
				m.confirmation = nil
				return m, m.runAction(a, "switch")
			}
			return m, nil
		}
		if m.help {
			if key == "?" || key == "esc" || key == "q" {
				m.help = false
			}
			return m, nil
		}
		if m.filtering {
			if key == "esc" {
				m.filtering = false
				m.filter.Blur()
				m.filter.SetValue("")
				m.cursor = 0
				return m, nil
			}
			if key == "enter" {
				m.filtering = false
				m.filter.Blur()
				return m, nil
			}
			var cmd tea.Cmd
			m.filter, cmd = m.filter.Update(msg)
			m.cursor = 0
			m.scroll = 0
			return m, cmd
		}
		switch key {
		case "w":
			m.workMode = true
			return m, m.loadWork()
		case "q":
			return m, tea.Quit
		case "?":
			m.help = true
		case "/":
			m.filtering = true
			return m, m.filter.Focus()
		case "esc":
			m.filter.SetValue("")
			m.focus = 0
			m.scroll = 0
		case "1", "2":
			m.provider = int(key[0] - '0')
			m.tableOffset = 0
			m.cursor = 0
			m.scroll = 0
		case "tab", "shift+tab", "enter":
			m.focus = 1 - m.focus
		case "pgdown":
			m.pageAccounts(1)
		case "pgup":
			m.pageAccounts(-1)
		case "j", "down":
			m.tableOffset = 0
			if m.focus == 0 {
				m.cursor = min(m.cursor+1, max(0, len(m.filtered())-1))
				m.scroll = 0
			} else {
				a, _ := m.selected()
				m.scroll = min(m.scroll+1, max(0, len(a.Windows)-1))
			}
		case "k", "up":
			m.tableOffset = 0
			if m.focus == 0 {
				m.cursor = max(0, m.cursor-1)
				m.scroll = 0
			} else {
				m.scroll = max(0, m.scroll-1)
			}
		case "g", "home":
			m.cursor = 0
			m.scroll = 0
		case "G", "end":
			m.cursor = max(0, len(m.filtered())-1)
			m.scroll = 0
		case "r":
			return m, m.refresh()
		case "s":
			a, ok := m.selected()
			if ok && !m.busy && !m.demo && a.CanSwitch {
				m.confirmation = &a
			} else {
				m.notice = "Switch unavailable for this selection"
			}
		case "n", "N", "l":
			a, ok := m.selected()
			if ok && !m.busy && !m.demo && a.CanLaunch {
				return m, m.runAction(a, "launch")
			}
			m.notice = "A new session requires a saved account"
		}
	}
	return m, nil
}
func (m *Model) identity(a Account, w, h int) string {
	marker := "Saved profile"
	if a.Active {
		marker = "Active identity"
	}
	if !a.Saved {
		marker = "Live identity · not saved"
	}
	content := []string{"", ink(" "+safe(a.Name()), accent) + "  " + ink(marker, muted), " " + safe(a.Email), " " + strings.ToUpper(a.Provider) + " · " + safe(a.Plan), " " + safe(a.Workspace)}
	if a.Provider == "codex" {
		value := "Not checked"
		if a.ResetCredits != nil {
			value = fmt.Sprintf("%d", *a.ResetCredits)
		}
		if a.ResetCreditsError != "" {
			value = safe(a.ResetCreditsError)
		}
		content = append(content, " Usage resets: "+value)
	}
	return frame("Identity", strings.Join(content, "\n"), w, h, false)
}
func (m *Model) quotas(a Account, w, h int) string {
	a.Windows = displayedWindows(a)
	if a.Warning != "" {
		a.ProfileNotes = append(a.ProfileNotes, a.Warning)
	}
	lines := []string{""}
	for _, note := range a.ProfileNotes {
		if a.Provider == "codex" && strings.Contains(strings.ToLower(note), "spark") {
			continue
		}
		lines = append(lines, ink(" "+safe(note), muted))
	}
	if a.CheckedAt > 0 {
		lines = append(lines, ink(" Read "+time.Unix(int64(a.CheckedAt), 0).Format("Jan 02 15:04")+" · usage per window", muted), "")
	}
	if a.Error != "" {
		lines = append(lines, ink(" Could not read this account", red))
		for _, line := range strings.Split(lipgloss.NewStyle().Width(max(1, w-4)).Render(safe(a.Error)), "\n") {
			lines = append(lines, " "+line)
		}
	} else if len(a.Windows) == 0 {
		lines = append(lines, " No quota reading yet.", ink(" Press r to check availability.", muted))
	}
	visible := max(1, (h-7)/3)
	start := min(m.scroll, max(0, len(a.Windows)-1))
	for _, window := range a.Windows[start:min(len(a.Windows), start+visible)] {
		lines = append(lines, " "+safe(window.Label))
		reset := "reset unknown"
		if window.Reset > 0 {
			reset = "resets " + time.Unix(int64(window.Reset), 0).Format("Mon 15:04")
		}
		barWidth := max(3, min(30, w-36))
		date, countdown := resetText(window.Reset, time.Now())
		reset = date + " " + time.Now().Format("MST") + " · in " + countdown
		lines = append(lines, " "+remainingBar(window.Used, barWidth)+fmt.Sprintf(" %5.1f%% remaining (%g%% used)", remaining(window.Used), window.Used*100), ink(" "+reset, muted))
	}
	if len(a.Windows) > visible {
		lines = append(lines, ink(fmt.Sprintf(" %d–%d of %d · Tab then j/k to scroll", start+1, min(len(a.Windows), start+visible), len(a.Windows)), muted))
	}
	return frame("Quota windows", strings.Join(lines, "\n"), w, h, m.focus == 1)
}
func (m *Model) View() tea.View {
	if m.workMode {
		return m.workView()
	}
	terminalW := m.width
	w, h := min(m.width, 140), m.height
	if w < 48 || h < 16 {
		v := tea.NewView(fit("Hotseat · enlarge terminal to 48 × 16", w))
		v.AltScreen = true
		return v
	}
	tabs := []string{"[1] Codex", "[2] Claude"}
	for i := range tabs {
		if i+1 == m.provider {
			tabs[i] = ink(tabs[i], accent)
		} else {
			tabs[i] = ink(tabs[i], muted)
		}
	}
	header := fit(" "+strings.Join(tabs, "    "), w)
	filterLine := ""
	if m.filtering || m.filter.Value() != "" {
		filterLine = fit(" "+m.filter.View(), w) + "\n"
	}
	contentHeight := h - 3
	if filterLine != "" {
		contentHeight--
	}
	var body string
	if m.help {
		body = frame("Keyboard", "\n j / k       Move through accounts or quota windows\n Enter / Esc Show / hide account details\n /           Filter by alias, email, provider or workspace\n 1 / 2       Codex / Claude\n r           Read fresh quota (no inference request)\n s           Change default account, with confirmation\n n           New session on the selected account\n Esc         Clear filter / cancel\n q           Quit\n\n Quotas are model-specific. A full window does not mean\n every model is exhausted. Errors are never shown as 0%.\n\n ? / Esc     Close help", w, contentHeight, true)
	} else if m.confirmation != nil {
		a := *m.confirmation
		detail := fmt.Sprintf("Changes the shared default used by %d running Claude sessions. Their account may change mid-session.", m.data.Sessions)
		if a.Provider == "codex" {
			detail = "New Codex processes use this switch. Existing sessions may retain their credentials."
		}
		text := "\n Change default to " + safe(a.Provider+" / "+a.Alias) + "?\n\n " + safe(a.Email) + "\n\n " + lipgloss.NewStyle().Width(w-6).Render(detail) + "\n\n y  Confirm     n / Esc  Cancel"
		body = frame("Change default account", text, w, contentHeight, true)
	} else {
		a, ok := m.selected()
		if !ok {
			a = Account{Alias: "Select an account", Email: "Use / to search, or r to refresh."}
		}
		if m.focus == 0 {
			body = m.overview(min(w, 140), contentHeight)
		} else {
			top := min(10, contentHeight/2)
			body = lipgloss.JoinVertical(lipgloss.Left, m.identity(a, w, top), m.quotas(a, w, contentHeight-top))
		}
	}
	status := " " + safe(m.notice)
	if m.busy {
		status = " " + m.spinner.View() + " Refreshing…"
	}
	mode := ""
	if m.filtering {
		mode = "FILTER"
	}
	hints := "j/k select  Enter details  / filter  r refresh  n new session  w work  s switch  ? help  q quit"
	if w < 90 {
		hints = "j/k  Enter  / filter  r  n new  w work  s  ?  q quit"
	}
	footer := " " + ink(hints, muted)
	if mode != "" {
		footer = lipgloss.NewStyle().Background(accent).Foreground(bg).Render(" "+mode+" ") + footer
	}
	screen := header + "\n" + filterLine + body + "\n" + fit(status, w) + "\n" + fit(footer, w)
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
func DemoData() Snapshot {
	now := float64(time.Now().Unix())
	two, zero := 2, 0
	return Snapshot{GeneratedAt: now, Accounts: []Account{
		{Provider: "codex", ResetCredits: &two, Alias: "work", CanLaunch: true, Email: "developer@example.com", Plan: "Business", Workspace: "Engineering", Active: true, Saved: true, CanSwitch: true, CheckedAt: now, Windows: []Window{{"Codex · weekly", 0.27, now + 86400*3}, {"Spark · 5-hour", 0.12, now + 7200}, {"Spark · weekly", 0.58, now + 86400*3}}},
		{Provider: "codex", ResetCredits: &zero, Alias: "personal", CanLaunch: true, Email: "personal@example.com", Plan: "Pro", Saved: true, CanSwitch: true, CheckedAt: now, Windows: []Window{{"Codex · weekly", 1, now + 86400}, {"Spark · weekly", 0.45, now + 86400}}},
		{Provider: "claude", Alias: "studio", Email: "studio@example.com", Plan: "Max", Workspace: "Personal", Saved: true, CanSwitch: true, CanLaunch: true, CheckedAt: now, Windows: []Window{{"All models · 5-hour", 0.18, now + 3600}, {"All models · weekly", 0.62, now + 86400*2}, {"Fable · weekly", 0.84, now + 86400*2}}},
		{Provider: "claude", Alias: "team", Email: "team@example.com", Plan: "Team", Saved: true, Active: true, CanSwitch: true, CanLaunch: true, Error: "The account rejected this token. Sign in again to read its quota."},
	}}
}
