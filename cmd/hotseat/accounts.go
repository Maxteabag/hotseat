package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Maxteabag/hotseat/internal/claude"
	"github.com/Maxteabag/hotseat/internal/collect"
)

func (a *app) cmdList(cmd *command, ctx context.Context, args []string) (int, error) {
	fs := a.flags(cmd)
	asJSON := jsonFlag(cmd, fs)
	if _, err := a.parse(cmd, fs, args, 0, 0); err != nil {
		return 2, err
	}
	snapshot, err := a.build(ctx)
	if err != nil {
		return 1, err
	}
	if *asJSON {
		return 0, a.emit(snapshot)
	}

	accounts := snapshot.Accounts
	if len(accounts) == 0 {
		a.println(deref(snapshot.Error, "No accounts found."))
		return 1, nil
	}

	a.printf("%s %s %s %s  %s %s  ACCOUNT\n", leftPad("ALIAS", 10), leftPad("PLAN", 6),
		rightPad("5-HOUR", 7), rightPad("7-DAY", 7), leftPad("STATUS", 12), rightPad("SIGN-IN", 8))
	for _, account := range accounts {
		status := statusWord(account)
		due := "—"
		if days := account.SigninDaysLeft; days != nil {
			if *days > 0 {
				due = fmt.Sprintf("%.0fd", *days)
			} else {
				due = "OVERDUE"
			}
			if *days <= collect.SigninWarnDays {
				due = a.paint(due, red)
			}
		}
		var statusText string
		switch status {
		case "available":
			statusText = a.paint(status, green)
		case "limited":
			statusText = a.paint(status, red)
		default:
			statusText = a.paint(status, yellow)
		}
		var used5h, used7d *float64
		var blocked []string
		if account.Usage != nil {
			used5h, used7d, blocked = account.Usage.Used5h, account.Usage.Used7d, account.Usage.BlockedModels
		}
		if len(blocked) > 0 && status == "available" {
			// Overall quota is fine but one model family is spent, which is the
			// difference between "usable" and "usable for what you wanted".
			statusText = a.paint("no "+blocked[0], yellow)
			status = "no " + blocked[0]
		}
		marker := " "
		if account.IsActive {
			marker = "*"
		}
		pad := runeLen(statusText) - runeLen(status)
		a.printf("%s%s %s %s %s  %s %s  %s\n", marker, leftPad(account.Alias, 9), leftPad(deref(account.Plan, "?"), 6),
			rightPad(pct(used5h), 7), rightPad(pct(used7d), 7), leftPad(statusText, 12+pad), rightPad(due, 8),
			deref(account.Email, "?"))
	}

	a.println()
	a.println(a.paint(fmt.Sprintf("%d Claude session(s) running.", snapshot.Sessions), dim))
	var soon []string
	for _, account := range accounts {
		if account.SigninDueSoon {
			soon = append(soon, account.Alias)
		}
	}
	if len(soon) > 0 {
		a.println(a.paint("Browser sign-in needed soon: "+strings.Join(soon, ", "), red))
	}
	return 0, nil
}

func (a *app) cmdShow(cmd *command, ctx context.Context, args []string) (int, error) {
	fs := a.flags(cmd)
	asJSON := jsonFlag(cmd, fs)
	positional, err := a.parse(cmd, fs, args, 1, 1, "alias")
	if err != nil {
		return 2, err
	}
	alias := positional[0]
	snapshot, err := a.build(ctx)
	if err != nil {
		return 1, err
	}
	var match *collect.AccountView
	for i := range snapshot.Accounts {
		if snapshot.Accounts[i].Alias == alias {
			match = &snapshot.Accounts[i]
			break
		}
	}
	if match == nil {
		fmt.Fprintf(a.stderr, "No account named '%s'\n", alias)
		return 1, nil
	}
	if *asJSON {
		return 0, a.emit(match)
	}

	usage := match.Usage
	a.printf("%s  %s\n", a.paint(match.Alias, bold), deref(match.Email, "?"))
	a.printf("  organisation   %s\n", deref(match.Org, "?"))
	a.printf("  plan           %s\n", deref(match.Plan, "?"))
	a.printf("  status         %s\n", statusWord(*match))
	var used5h, used7d, reset5h, reset7d *float64
	if usage != nil {
		used5h, used7d, reset5h, reset7d = usage.Used5h, usage.Used7d, usage.Reset5h, usage.Reset7d
	}
	a.printf("  5-hour         %s · resets %s\n", pct(used5h), a.clock(reset5h))
	a.printf("  7-day          %s · resets %s\n", pct(used7d), a.clock(reset7d))
	if usage != nil {
		for _, limit := range usage.Scoped {
			flag := ""
			if limit.Exhausted {
				flag = a.paint("  exhausted", red)
			}
			used := limit.Used
			a.printf("  %s %s%s\n", leftPad(limit.Model+" (weekly)", 14), pct(&used), flag)
		}
		if usage.Binding != "" {
			a.printf("  binding limit  %s\n", strings.ReplaceAll(usage.Binding, "_", " "))
		}
	}
	if match.AccessHoursLeft != nil {
		a.printf("  access token   %.1fh left\n", *match.AccessHoursLeft)
	}
	if match.SigninDaysLeft != nil {
		a.printf("  sign-in due    in %.0f days\n", *match.SigninDaysLeft)
	}
	if usage != nil && usage.OverageStatus != "" {
		reason := strings.ReplaceAll(usage.OverageReason, "_", " ")
		if reason != "" {
			reason = fmt.Sprintf(" (%s)", reason)
		}
		a.printf("  extra usage    %s%s\n", usage.OverageStatus, reason)
	}
	if match.Error != nil && *match.Error != "" {
		a.printf("  error          %s\n", *match.Error)
	}
	return 0, nil
}

func (a *app) cmdVerify(cmd *command, ctx context.Context, args []string) (int, error) {
	fs := a.flags(cmd)
	asJSON := jsonFlag(cmd, fs)
	positional, err := a.parse(cmd, fs, args, 1, 1, "alias")
	if err != nil {
		return 2, err
	}
	alias := positional[0]
	result, err := a.verify(ctx, a.backend(), alias)
	if err != nil {
		if *asJSON {
			return 1, a.emit(map[string]any{"alias": alias, "ok": false, "error": err.Error()})
		}
		a.errorln(a.paint("✗ "+err.Error(), red))
		return 1, nil
	}
	if *asJSON {
		return 0, a.emit(result)
	}
	a.println(a.paint(fmt.Sprintf("✓ %s is live (checked against the API just now)", alias), green))
	return 0, nil
}

// cmdUse replaces this shell with a session pinned to the account.
func (a *app) cmdUse(cmd *command, _ context.Context, args []string) (int, error) {
	if len(args) == 0 {
		return 2, &usageError{"hotseat use", "the following arguments are required: alias"}
	}
	if args[0] == "-h" || args[0] == "--help" {
		a.printCommandHelp(cmd, a.flags(cmd))
		return 0, nil
	}
	if err := a.execSession(a.backend(), args[0], args[1:]); err != nil {
		a.errorln(a.paint("✗ "+err.Error(), red))
		return 1, nil
	}
	return 0, nil // not reached: the process has been replaced
}

func (a *app) cmdWindow(cmd *command, _ context.Context, args []string) (int, error) {
	fs := a.flags(cmd)
	asJSON := jsonFlag(cmd, fs)
	positional, err := a.parse(cmd, fs, args, 1, 1, "alias")
	if err != nil {
		return 2, err
	}
	alias := positional[0]
	result, err := a.launch(a.backend(), alias)
	if err != nil {
		a.errorln(a.paint("✗ "+err.Error(), red))
		return 1, nil
	}
	if *asJSON {
		return 0, a.emit(result)
	}
	a.printf("Opened a %s window pinned to %s.\n", result.Terminal, alias)
	return 0, nil
}

// cmdSwitch changes the machine-wide default. The dangerous one.
func (a *app) cmdSwitch(cmd *command, _ context.Context, args []string) (int, error) {
	fs := a.flags(cmd)
	asJSON := jsonFlag(cmd, fs)
	yes := fs.Bool("yes", false, "Skip the confirmation")
	positional, err := a.parse(cmd, fs, args, 1, 1, "alias")
	if err != nil {
		return 2, err
	}
	alias := positional[0]
	backend := a.backend()
	sessions := a.runningSessions()
	if !*yes {
		a.printf("Switching the default to %s retargets %d running session(s) mid-conversation.\n", alias, sessions)
		a.printf("To pin one session instead, leaving the others alone: hotseat use %s\n", alias)
		if !a.stdinIsTTY() {
			a.errorln("Refusing without --yes.")
			return 1, nil
		}
		if !a.confirm("Switch anyway? [y/N] ") {
			a.println("Cancelled.")
			return 1, nil
		}
	}
	result, err := a.switchDefault(backend, alias, sessions)
	if err != nil {
		a.errorln(a.paint("✗ "+err.Error(), red))
		return 1, nil
	}
	if *asJSON {
		return 0, a.emit(result)
	}
	a.printf("Default account is now %s.\n", alias)
	return 0, nil
}

// cmdRefresh brings expired saved profiles back to full-length access tokens.
func (a *app) cmdRefresh(cmd *command, _ context.Context, args []string) (int, error) {
	fs := a.flags(cmd)
	asJSON := jsonFlag(cmd, fs)
	force := fs.Bool("force", false, "Refresh even if the token is still valid")
	aliases, err := a.parse(cmd, fs, args, 0, -1)
	if err != nil {
		return 2, err
	}
	backend := a.backend()
	if len(aliases) == 0 {
		accounts, err := backend.Accounts()
		if err != nil {
			a.errorln(a.paint("✗ "+err.Error(), red))
			return 1, nil
		}
		for _, account := range accounts {
			if !strings.HasPrefix(account.Alias, "(") && (*force || a.expired(account)) {
				aliases = append(aliases, account.Alias)
			}
		}
		if len(aliases) == 0 {
			if *asJSON {
				a.println(`{"refreshed": []}`)
			} else {
				a.println(a.paint("Every saved profile still has a usable token.", dim))
			}
			return 0, nil
		}
	}
	results := make([]any, 0, len(aliases))
	failed := false
	for _, alias := range aliases {
		out, err := a.refresh(backend, alias, *force)
		if err != nil {
			failed = true
			results = append(results, map[string]any{"alias": alias, "refreshed": false, "error": err.Error()})
			if !*asJSON {
				a.printf("%s %s: %s\n", a.paint("✗", red), alias, err.Error())
			}
			continue
		}
		results = append(results, out)
		if *asJSON {
			continue
		}
		if out.Refreshed {
			how := "through the CLI"
			if out.Source == "session" {
				how = "from its pinned session"
			}
			a.printf("%s %s: refreshed %s, %.1fh left\n", a.paint("✓", green), alias, how, out.HoursLeft)
		} else {
			a.printf("%s %s: %s, %.1fh left\n", a.paint("·", dim), alias, out.Source, out.HoursLeft)
		}
	}
	if *asJSON {
		encoded, err := json.MarshalIndent(map[string]any{"refreshed": results}, "", "  ")
		if err != nil {
			return 1, err
		}
		a.println(string(encoded))
	}
	if failed {
		return 1, nil
	}
	return 0, nil
}

func (a *app) cmdModels(cmd *command, _ context.Context, args []string) (int, error) {
	fs := a.flags(cmd)
	asJSON := jsonFlag(cmd, fs)
	days := fs.Int("days", claude.DefaultDays, "")
	if _, err := a.parse(cmd, fs, args, 0, 0); err != nil {
		return 2, err
	}
	usage := a.modelUsage(*days)
	if usage == nil {
		a.errorln("No local usage statistics found.")
		return 1, nil
	}
	if *asJSON {
		return 0, a.emit(usage)
	}

	a.printf("Last %d days, every account on this machine: %s tokens\n", usage.Days, tokens(usage.TotalTokens))
	if usage.Stale {
		a.println(a.paint(fmt.Sprintf("Counts run to %s; today is not included yet.", deref(usage.AsOf, "None")), dim))
	}
	a.println()
	for _, model := range usage.Models {
		// A share too small to fill a block still gets a tick, so it is visibly
		// present rather than appearing to be zero.
		bar := strings.Repeat("█", pyRound(model.Share*28))
		if bar == "" {
			bar = "▏"
		}
		share := model.Share * 100
		if share >= 0.1 {
			a.printf("  %s %s %5.1f%%  %s\n", leftPad(model.Label, 14), rightPad(tokens(model.Tokens), 8), share, bar)
		} else {
			a.printf("  %s %s %s%%  %s\n", leftPad(model.Label, 14), rightPad(tokens(model.Tokens), 8), rightPad("<0.1", 5), bar)
		}
	}
	return 0, nil
}

func (a *app) cmdStatusline(cmd *command, _ context.Context, args []string) (int, error) {
	fs := a.flags(cmd)
	if _, err := a.parse(cmd, fs, args, 0, 0); err != nil {
		return 2, err
	}
	if line := a.statusline(); line != "" {
		a.println(line)
	}
	return 0, nil
}
