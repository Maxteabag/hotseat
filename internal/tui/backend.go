package tui

import "context"

type Window struct {
	Label string  `json:"label"`
	Used  float64 `json:"used"`
	Reset float64 `json:"reset"`
}
type Account struct {
	Cached            bool     `json:"cached"`
	Warning           string   `json:"warning"`
	DisplayAlias      string   `json:"display_alias"`
	Aliases           []string `json:"aliases"`
	ProfileNotes      []string `json:"profile_notes"`
	ResetCredits      *int     `json:"reset_credits"`
	ResetCreditsError string   `json:"reset_credits_error"`
	Provider          string   `json:"provider"`
	Alias             string   `json:"alias"`
	Email             string   `json:"email"`
	Plan              string   `json:"plan"`
	RateLimitTier     string   `json:"rate_limit_tier"`
	Workspace         string   `json:"workspace"`
	Active            bool     `json:"active"`
	Saved             bool     `json:"saved"`
	CanSwitch         bool     `json:"can_switch"`
	CanLaunch         bool     `json:"can_launch"`
	Limited           bool     `json:"limited"`
	Windows           []Window `json:"windows"`
	Error             string   `json:"error"`
	CheckedAt         float64  `json:"checked_at"`
	// NoResetReason: the reset shown for this account will not unblock it.
	NoResetReason string `json:"no_reset_reason"`
}

func (a Account) Name() string {
	if a.DisplayAlias != "" {
		return a.DisplayAlias
	}
	return a.Alias
}
func (a Account) ID() string { return a.Provider + ":" + a.Name() }
func (a Account) Status() string {
	if a.Error != "" {
		return "Check failed"
	}
	if a.CheckedAt == 0 || len(a.Windows) == 0 {
		return "Unknown"
	}
	if a.Limited {
		return "Limited"
	}
	for _, w := range a.Windows {
		if w.Used >= 1 {
			return "Some limits"
		}
	}
	return "Available"
}

type Snapshot struct {
	Accounts    []Account `json:"accounts"`
	GeneratedAt float64   `json:"generated_at"`
	Errors      []string  `json:"errors"`
	Sessions    int       `json:"sessions"`
}
type Backend interface {
	Snapshot(context.Context, bool) (Snapshot, error)
	Action(context.Context, Account, string) error
}
