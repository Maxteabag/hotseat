package collect

import (
	"context"
	"errors"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/Maxteabag/hotseat/internal/claude"
	"github.com/Maxteabag/hotseat/internal/codex"
	"github.com/Maxteabag/hotseat/internal/quota"
	"github.com/Maxteabag/hotseat/internal/tui"
	"github.com/Maxteabag/hotseat/internal/work"
)

// SnapshotWorkers is how many account rows are built at once (the Python used a
// five-worker pool shared by the Codex job and the Claude rows).
const SnapshotWorkers = 5

// Timeouts the TUI applied to the Python subprocess; kept so a hung probe or a
// stuck launch cannot wedge the interface.
const (
	SnapshotTimeout = 6 * time.Minute
	ActionTimeout   = 45 * time.Second
	WorkTimeout     = 60 * time.Second
)

// Switcher activates a saved Codex profile; *codex.Profiles satisfies it.
type Switcher interface {
	Switch(name string) error
}

// Bridge is the in-process replacement for `python -m hotseat.tui_bridge`. It
// satisfies tui.Backend and tui.WorkBackend. The zero value reads the real
// machine; the exported fields are seams for tests.
type Bridge struct {
	// Backend supplies Claude accounts; nil means claude.ForPlatform("").
	Backend claude.Backend
	// Quota is the cooldown cache quota reads go through; nil means the
	// zero-value cache (default root, wall clock, quota.ForToken).
	Quota *quota.Cache
	// Codex reads Codex accounts and balances; nil means codex.New(codex.DefaultHome()).
	Codex CodexSource
	// Profiles switches the default Codex profile; nil means
	// codex.NewProfiles(codex.DefaultHome(), nil).
	Profiles Switcher
	// WorkService reads and acts on stopped work; nil means work.New with the
	// real Codex history and a terminal launcher.
	WorkService *work.Service
	// Spawn starts detached processes; nil means claude.DetachedSpawn.
	Spawn claude.Spawn
	// Terminal overrides terminal detection when non-empty.
	Terminal string
	// AutoRefresh brings an expired account's token back through the CLI; nil
	// means claude.Auto with default options.
	AutoRefresh func(backend claude.Backend, account *claude.Account) error
	// RunningSessions counts live claude processes; nil means claude.RunningSessions.
	RunningSessions func() int
	// Now is the clock in epoch seconds; nil means the wall clock.
	Now func() float64
}

var (
	_ tui.Backend     = (*Bridge)(nil)
	_ tui.WorkBackend = (*Bridge)(nil)
)

// New is a Bridge against the real machine.
func New() *Bridge {
	b := &Bridge{}
	b.WorkService = work.New(&codexSessions{codex.NewSessions(codex.DefaultHome())}, b.launcher())
	return b
}

func (b *Bridge) backend() claude.Backend {
	if b.Backend == nil {
		b.Backend = claude.ForPlatform("")
	}
	return b.Backend
}

func (b *Bridge) quota() *quota.Cache {
	if b.Quota == nil {
		b.Quota = &quota.Cache{}
	}
	return b.Quota
}

func (b *Bridge) codex() CodexSource {
	if b.Codex == nil {
		b.Codex = codex.New(codex.DefaultHome())
	}
	return b.Codex
}

func (b *Bridge) profiles() Switcher {
	if b.Profiles == nil {
		b.Profiles = codex.NewProfiles(codex.DefaultHome(), nil)
	}
	return b.Profiles
}

func (b *Bridge) work() *work.Service {
	if b.WorkService == nil {
		b.WorkService = work.New(&codexSessions{codex.NewSessions(codex.DefaultHome())}, b.launcher())
	}
	return b.WorkService
}

func (b *Bridge) launcher() *terminalLauncher {
	return &terminalLauncher{spawn: b.Spawn, terminal: b.Terminal}
}

func (b *Bridge) now() float64 {
	if b.Now != nil {
		return b.Now()
	}
	return epochNow()
}

func (b *Bridge) runningSessions() int {
	if b.RunningSessions != nil {
		return b.RunningSessions()
	}
	return claude.RunningSessions()
}

func (b *Bridge) refresh(backend claude.Backend, account *claude.Account) error {
	if b.AutoRefresh != nil {
		return b.AutoRefresh(backend, account)
	}
	_, err := claude.Auto(backend, account, claude.RefreshOptions{})
	return err
}

// --- snapshot ----------------------------------------------------------------

// Snapshot is the account overview the TUI renders. With refresh, every account's
// quota is read live; without it, rows carry identity only and checked_at is 0.
func (b *Bridge) Snapshot(ctx context.Context, refresh bool) (tui.Snapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, SnapshotTimeout)
	defer cancel()
	rows, err := b.Rows(ctx, refresh)
	if err != nil {
		return tui.Snapshot{}, err
	}
	return rows.Snapshot(), nil
}

// Rows is Snapshot before conversion: the exact rows the Python bridge printed.
func (b *Bridge) Rows(ctx context.Context, refresh bool) (RowSnapshot, error) {
	backend := b.backend()
	result := RowSnapshot{Accounts: []Row{}, GeneratedAt: b.now(), Errors: []string{}, Sessions: b.runningSessions()}
	accounts, err := backend.Accounts()
	if err != nil {
		accounts = nil
		result.Errors = append(result.Errors, "Claude discovery: "+err.Error())
	}

	claudeRows := make([]Row, len(accounts))
	var codexRows []Row
	var codexErr string
	group, gctx := errgroup.WithContext(ctx)
	group.SetLimit(SnapshotWorkers)
	group.Go(func() error {
		codexRows, codexErr = b.codexRows(gctx, refresh)
		return nil
	})
	for i := range accounts {
		group.Go(func() error {
			if err := gctx.Err(); err != nil {
				return err
			}
			claudeRows[i] = b.claudeRow(gctx, backend, accounts[i], refresh)
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return RowSnapshot{}, err
	}
	if err := ctx.Err(); err != nil {
		return RowSnapshot{}, err
	}
	rows := append(claudeRows, codexRows...)
	if codexErr != "" {
		result.Errors = append(result.Errors, codexErr)
	}
	result.Accounts = GroupAccounts(rows)
	return result, nil
}

func (b *Bridge) claudeRow(ctx context.Context, backend claude.Backend, account claude.Account, refresh bool) Row {
	email := account.Email
	if email == "" {
		email = "Unknown email"
	}
	var tier *string
	if account.RateLimitTier != "" {
		t := account.RateLimitTier
		tier = &t
	}
	unsaved := strings.HasPrefix(account.Alias, "(")
	row := Row{
		Provider: "claude", Alias: account.Alias, Email: email, Plan: account.PlanLabel(),
		RateLimitTier: tier, Workspace: account.Org, Active: account.IsActive, Saved: !unsaved,
		Windows: []tui.Window{}, CanSwitch: backend.CanSwitch() && !unsaved, CanLaunch: true,
	}
	if !refresh {
		return row
	}
	if account.Token == "" {
		row.Error = "No stored token"
		return row
	}
	if claude.Expired(account, b.now(), 0) {
		if err := b.refresh(backend, &account); err != nil {
			row.Error = "Access token expired; refresh failed: " + err.Error()
			return row
		}
	}
	limits, err := b.quota().Read(ctx, account.Token)
	if err != nil {
		row.Error = err.Error()
		return row
	}
	row.Read = true
	row.CheckedAt = limits.CheckedAt
	if row.CheckedAt == 0 {
		row.CheckedAt = b.now()
	}
	row.Cached = limits.Cached
	row.Warning = limits.Warning
	row.Limited = limits.Limited
	for _, w := range []struct {
		label string
		used  *float64
		reset *float64
	}{{"All models · 5-hour", limits.Used5h, limits.Reset5h}, {"All models · weekly", limits.Used7d, limits.Reset7d}} {
		if w.used != nil {
			row.Windows = append(row.Windows, tui.Window{Label: w.label, Used: *w.used, Reset: deref(w.reset)})
		}
	}
	for _, item := range limits.Scoped {
		row.Windows = append(row.Windows, tui.Window{Label: item.Model + " · weekly", Used: item.Used, Reset: deref(item.Reset)})
	}
	return row
}

func deref(v *float64) float64 {
	if v == nil {
		return 0
	}
	return *v
}

func (b *Bridge) codexRows(ctx context.Context, refresh bool) ([]Row, string) {
	source := b.codex()
	overview, err := source.Overview(ctx, refresh, 0)
	if err != nil {
		return nil, "Codex discovery: " + err.Error()
	}
	credits := map[string]codex.Balance{}
	if refresh {
		balances, err := source.Balances(ctx, nil)
		if err != nil {
			return nil, "Codex discovery: " + err.Error()
		}
		for _, balance := range balances {
			credits[balance.Alias] = balance
		}
	}
	rows := make([]Row, 0, len(overview.Accounts))
	for _, item := range overview.Accounts {
		row := Row{
			Provider: "codex", Alias: item.Alias, Email: "Unknown email", Plan: "Unknown plan",
			Active: item.IsActive, Saved: item.Saved, CanSwitch: item.Saved, CanLaunch: item.Saved,
			Windows: []tui.Window{},
		}
		if item.Email != nil && *item.Email != "" {
			row.Email = *item.Email
		}
		if item.Plan != nil && *item.Plan != "" {
			row.Plan = *item.Plan
		}
		if item.AccountID != nil {
			row.Workspace = *item.AccountID
		}
		if balance, ok := credits[item.Alias]; ok {
			row.ResetCredits = balance.AvailableCount
			if balance.Error != nil {
				row.ResetCreditsError = *balance.Error
			}
		}
		limits := item.Usage
		if limits != nil {
			for _, w := range limits.Windows {
				row.Windows = append(row.Windows, tui.Window{Label: w.Label, Used: w.Used, Reset: deref(w.Reset)})
			}
			row.NoResetReason = limits.NoResetReason
			if limits.Error != nil && *limits.Error != "" {
				row.Error = *limits.Error
			} else {
				row.CheckedAt = b.now()
			}
		} else if refresh {
			row.Error = "No quota reading returned"
		}
		rows = append(rows, row)
	}
	return rows, ""
}

// --- account actions ---------------------------------------------------------

// Action is the TUI's entry point: "switch" or "launch" on an account, with the
// switch acknowledged (the TUI confirms before calling).
func (b *Bridge) Action(ctx context.Context, account tui.Account, operation string) error {
	ctx, cancel := context.WithTimeout(ctx, ActionTimeout)
	defer cancel()
	switch operation {
	case "switch":
		return b.Switch(ctx, account, true)
	case "launch":
		return b.Launch(ctx, account)
	}
	return errors.New("Unsupported action")
}

// Switch changes the default account of a provider. The account is resolved
// again at action time; nothing arbitrary is ever executed.
func (b *Bridge) Switch(ctx context.Context, account tui.Account, acknowledged bool) error {
	switch account.Provider {
	case "claude":
		backend := b.backend()
		if _, err := claude.FindAccount(backend, account.Alias); err != nil {
			return err
		}
		_, err := claude.Switch(backend, account.Alias, b.runningSessions(), acknowledged)
		return err
	case "codex":
		if !acknowledged {
			return errors.New("Changing the default requires confirmation")
		}
		if !b.savedCodexProfile(account.Alias) {
			return errors.New("Saved Codex profile not found")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		return b.profiles().Switch(account.Alias)
	}
	return errors.New("This action is not available for that provider")
}

func (b *Bridge) savedCodexProfile(alias string) bool {
	for _, a := range b.codex().Accounts() {
		if a.Alias == alias && a.Saved {
			return true
		}
	}
	return false
}

// Launch opens a new terminal window pinned to the account.
func (b *Bridge) Launch(ctx context.Context, account tui.Account) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	switch account.Provider {
	case "claude":
		backend := b.backend()
		if _, err := claude.FindAccount(backend, account.Alias); err != nil {
			return err
		}
		_, err := claude.Launch(backend, account.Alias, b.Spawn, b.Terminal)
		return err
	case "codex":
		source, ok := b.codex().(*codex.Codex)
		if !ok {
			return errors.New("This action is not available for that provider")
		}
		_, err := source.Launch(account.Alias, b.launcher(), b.Terminal)
		return err
	}
	return errors.New("This action is not available for that provider")
}

// --- work ---------------------------------------------------------------------

// Listing is the raw work listing, as `tui_bridge work` printed it.
func (b *Bridge) Listing(ctx context.Context) work.Listing {
	return b.work().Listing(ctx, 100)
}

// Work is the work listing the TUI renders.
func (b *Bridge) Work(ctx context.Context) (tui.WorkSnapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, WorkTimeout)
	defer cancel()
	listing := b.Listing(ctx)
	if err := ctx.Err(); err != nil {
		return tui.WorkSnapshot{}, err
	}
	out := tui.WorkSnapshot{Items: make([]tui.WorkItem, 0, len(listing.Items)), Errors: listing.Errors}
	if out.Errors == nil {
		out.Errors = []string{}
	}
	for _, item := range listing.Items {
		out.Items = append(out.Items, workItem(item))
	}
	return out, nil
}

func workItem(item work.Item) tui.WorkItem {
	return tui.WorkItem{
		ID: item.ID, Provider: item.Provider, Title: item.Title, State: item.State, CWD: item.CWD,
		Updated: item.Updated, Live: item.Live, CanResume: item.CanResume, CanReboot: item.CanReboot,
		Reason: item.Reason, Revision: item.Revision,
	}
}

// InspectWork adds what the conversation was about to a listed item.
func (b *Bridge) InspectWork(ctx context.Context, item tui.WorkItem) (tui.WorkItem, error) {
	ctx, cancel := context.WithTimeout(ctx, WorkTimeout)
	defer cancel()
	detail, err := b.work().Detail(ctx, item.Provider, item.ID)
	if err != nil {
		return item, err
	}
	out := workItem(detail.Item)
	out.LastUser, out.LastAssistant, out.Model = detail.LastUser, detail.LastAssistant, detail.Model
	return out, nil
}

// WorkAction is the TUI's entry point: "resume" or "reboot", acknowledged.
func (b *Bridge) WorkAction(ctx context.Context, item tui.WorkItem, operation string) error {
	ctx, cancel := context.WithTimeout(ctx, ActionTimeout)
	defer cancel()
	switch operation {
	case "resume":
		_, err := b.ResumeWork(ctx, item, true)
		return err
	case "reboot":
		_, err := b.RebootWork(ctx, item, true)
		return err
	}
	return &work.WorkError{Msg: "Unsupported work action"}
}

// ResumeWork reopens a stopped conversation in a new terminal.
func (b *Bridge) ResumeWork(ctx context.Context, item tui.WorkItem, acknowledged bool) (work.ActResult, error) {
	return b.work().Act(ctx, item.Provider, item.ID, "resume", item.Revision, acknowledged)
}

// RebootWork stops the exact owning process and reopens the conversation.
func (b *Bridge) RebootWork(ctx context.Context, item tui.WorkItem, acknowledged bool) (work.ActResult, error) {
	return b.work().Act(ctx, item.Provider, item.ID, "reboot", item.Revision, acknowledged)
}

// Detail is InspectWork in the shape `tui_bridge inspect-work` printed.
func (b *Bridge) Detail(ctx context.Context, provider, identifier string) (work.ItemDetail, error) {
	return b.work().Detail(ctx, provider, identifier)
}
