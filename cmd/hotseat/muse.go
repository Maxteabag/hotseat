package main

import (
	"context"
	"fmt"
)

// cmdMuse shows the Meta Muse Code account and its allowance windows.
func (a *app) cmdMuse(cmd *command, ctx context.Context, args []string) (int, error) {
	fs := a.flags(cmd)
	asJSON := jsonFlag(cmd, fs)
	if _, err := a.parse(cmd, fs, args, 0, 0); err != nil {
		return 2, err
	}
	overview, err := a.museOverview(ctx)
	if err != nil {
		a.errorln("✗ " + err.Error())
		return 1, nil
	}
	if *asJSON {
		return 0, a.emit(overview)
	}
	if overview == nil {
		a.println("Muse Code is not installed. Install it with: curl -fsSL https://dev.meta.ai/install.sh | bash")
		return 1, nil
	}
	if !overview.LoggedIn || overview.Account == nil {
		a.println("Muse Code is installed but not signed in. Run: muse login")
		return 1, nil
	}

	account := overview.Account
	tier := account.Tier
	if tier == "" {
		tier = "unknown plan"
	}
	a.printf("*%s  %s\n", account.Email, a.paint(tier, dim))
	if account.Error != "" {
		a.println(a.paint("  "+account.Error, yellow))
		return 1, nil
	}
	if len(account.Windows) == 0 {
		reason := account.Reason
		if reason == "" {
			reason = "no allowance reported"
		}
		a.println(a.paint("  No usage figures yet ("+reason+"); they appear after the first request in a new window.", dim))
		return 0, nil
	}
	a.printf(" %s %s  %s\n", leftPad("WINDOW", 8), rightPad("USED", 6), "RESETS")
	for _, w := range account.Windows {
		used := "—"
		colour := green
		if w.UsedPercent != nil {
			used = fmt.Sprintf("%d%%", pyRound(*w.UsedPercent))
			switch {
			case *w.UsedPercent >= 100:
				colour = red
			case *w.UsedPercent >= 80:
				colour = yellow
			}
		}
		a.printf(" %s %s  %s\n", leftPad(w.Label, 8), a.paint(rightPad(used, 6), colour), a.clock(w.ResetsAt))
	}
	return 0, nil
}
