package collect

import (
	"encoding/json"
	"slices"
	"strings"

	"github.com/Maxteabag/hotseat/internal/tui"
)

// Row is one account row of the bridge snapshot, in the exact shape the Python
// bridge emitted: Claude rows carry rate_limit_tier and, after a successful
// quota read, cached/warning/limited; Codex rows carry reset_credits and
// reset_credits_error instead. GroupAccounts adds display_alias, aliases and
// profile_notes. MarshalJSON emits exactly the keys the Python dict had.
type Row struct {
	Provider  string
	Alias     string
	Email     string
	Plan      string
	Workspace string
	Active    bool
	Saved     bool
	CanSwitch bool
	CanLaunch bool
	Windows   []tui.Window
	Error     string
	CheckedAt float64

	// Claude only.
	RateLimitTier *string
	// Read is set when a live quota read succeeded, which is when the Python
	// row gained cached, warning and limited.
	Read    bool
	Cached  bool
	Warning string
	Limited bool

	// Codex only.
	ResetCredits      *int
	ResetCreditsError string

	// Set by GroupAccounts.
	Grouped      bool
	DisplayAlias string
	Aliases      []string
	ProfileNotes []string
}

// MarshalJSON emits the Python dict for this row.
func (r Row) MarshalJSON() ([]byte, error) {
	windows := r.Windows
	if windows == nil {
		windows = []tui.Window{}
	}
	out := map[string]any{
		"provider": r.Provider, "alias": r.Alias, "email": r.Email, "plan": r.Plan,
		"workspace": r.Workspace, "active": r.Active, "saved": r.Saved,
		"can_switch": r.CanSwitch, "can_launch": r.CanLaunch, "windows": windows,
		"error": r.Error, "checked_at": r.CheckedAt,
	}
	switch r.Provider {
	case "claude":
		out["rate_limit_tier"] = r.RateLimitTier
		if r.Read {
			out["cached"] = r.Cached
			out["warning"] = r.Warning
			out["limited"] = r.Limited
		}
	case "codex":
		out["reset_credits"] = r.ResetCredits
		out["reset_credits_error"] = r.ResetCreditsError
	}
	if r.Grouped {
		aliases := r.Aliases
		if aliases == nil {
			aliases = []string{}
		}
		notes := r.ProfileNotes
		if notes == nil {
			notes = []string{}
		}
		out["display_alias"] = r.DisplayAlias
		out["aliases"] = aliases
		out["profile_notes"] = notes
	}
	return json.Marshal(out)
}

// Account converts a row to what the TUI renders.
func (r Row) Account() tui.Account {
	tier := ""
	if r.RateLimitTier != nil {
		tier = *r.RateLimitTier
	}
	windows := r.Windows
	if windows == nil {
		windows = []tui.Window{}
	}
	return tui.Account{
		Cached: r.Cached, Warning: r.Warning, DisplayAlias: r.DisplayAlias, Aliases: r.Aliases,
		ProfileNotes: r.ProfileNotes, ResetCredits: r.ResetCredits, ResetCreditsError: r.ResetCreditsError,
		Provider: r.Provider, Alias: r.Alias, Email: r.Email, Plan: r.Plan, RateLimitTier: tier,
		Workspace: r.Workspace, Active: r.Active, Saved: r.Saved, CanSwitch: r.CanSwitch,
		CanLaunch: r.CanLaunch, Limited: r.Limited, Windows: windows, Error: r.Error, CheckedAt: r.CheckedAt,
	}
}

// RowSnapshot is the bridge snapshot before conversion to tui.Snapshot; it
// marshals to exactly what `python -m hotseat.tui_bridge snapshot` printed.
type RowSnapshot struct {
	Accounts    []Row    `json:"accounts"`
	GeneratedAt float64  `json:"generated_at"`
	Errors      []string `json:"errors"`
	Sessions    int      `json:"sessions"`
}

// Snapshot converts the rows to what the TUI consumes.
func (s RowSnapshot) Snapshot() tui.Snapshot {
	accounts := make([]tui.Account, 0, len(s.Accounts))
	for _, row := range s.Accounts {
		accounts = append(accounts, row.Account())
	}
	errs := s.Errors
	if errs == nil {
		errs = []string{}
	}
	return tui.Snapshot{Accounts: accounts, GeneratedAt: s.GeneratedAt, Errors: errs, Sessions: s.Sessions}
}

// GroupAccounts displays each provider/email/workspace once without deleting
// any profiles.
//
// It keeps a successful profile for actions, but shows a short stable display
// alias. It never groups accounts across workspaces or merges unknown
// identities: a row without a real email or without a workspace is its own
// group, keyed by alias.
func GroupAccounts(rows []Row) []Row {
	type key struct{ provider, email, workspace, alias string }
	order := []key{}
	groups := map[key][]Row{}
	for _, row := range rows {
		k := key{provider: row.Provider, alias: row.Alias}
		if row.Email != "" && row.Email != "Unknown email" && row.Workspace != "" {
			k = key{provider: row.Provider, email: row.Email, workspace: row.Workspace}
		}
		if _, seen := groups[k]; !seen {
			order = append(order, k)
		}
		groups[k] = append(groups[k], row)
	}
	result := make([]Row, 0, len(order))
	for _, k := range order {
		members := groups[k]
		// Python's max keeps the first of equal keys.
		best := members[0]
		for _, member := range members[1:] {
			if rowRank(member) > rowRank(best) {
				best = member
			}
		}
		item := best
		item.Grouped = true
		item.Active = false
		item.Aliases = make([]string, 0, len(members))
		item.ProfileNotes = []string{}
		item.DisplayAlias = members[0].Alias
		for _, member := range members {
			if member.Active {
				item.Active = true
			}
			item.Aliases = append(item.Aliases, member.Alias)
			if len(member.Alias) < len(item.DisplayAlias) ||
				(len(member.Alias) == len(item.DisplayAlias) && member.Alias < item.DisplayAlias) {
				item.DisplayAlias = member.Alias
			}
			if member.Error != "" {
				note := member.Error
				if strings.Contains(member.Error, "token_revoked") {
					note = "Sign-in expired"
				}
				item.ProfileNotes = append(item.ProfileNotes, member.Alias+": "+note)
			}
		}
		if item.Provider == "codex" && item.CheckedAt != 0 && !hasSparkWindow(item.Windows) {
			// Design decision 4: kept verbatim.
			item.ProfileNotes = append(item.ProfileNotes, "OpenAI reports no separate Spark allowance for this account.")
		}
		result = append(result, item)
	}
	slices.SortStableFunc(result, func(a, b Row) int {
		if c := strings.Compare(a.Provider, b.Provider); c != 0 {
			return c
		}
		if a.Active != b.Active {
			if a.Active {
				return -1
			}
			return 1
		}
		if (a.Error != "") != (b.Error != "") {
			if a.Error == "" {
				return -1
			}
			return 1
		}
		return strings.Compare(a.DisplayAlias, b.DisplayAlias)
	})
	return result
}

// rowRank orders group members the way Python's max key did: a checked row
// without an error, then any row without an error, then an active one.
func rowRank(r Row) int {
	rank := 0
	if r.CheckedAt != 0 && r.Error == "" {
		rank += 4
	}
	if r.Error == "" {
		rank += 2
	}
	if r.Active {
		rank++
	}
	return rank
}

func hasSparkWindow(windows []tui.Window) bool {
	for _, w := range windows {
		label := strings.ToLower(w.Label)
		if strings.Contains(label, "bengalfox") || strings.Contains(label, "spark") {
			return true
		}
	}
	return false
}
