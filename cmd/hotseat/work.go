package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/Maxteabag/hotseat/internal/clarp"
	"github.com/Maxteabag/hotseat/internal/work"
)

// cmdResume lists, and optionally continues, work that a usage limit stopped.
//
// With Clarp state on this machine this is the Clarp plugin's override, which
// merged native sessions with Clarp agents and continued the Clarp ones; without
// it, it is the core command, which only lists native sessions.
func (a *app) cmdResume(cmd *command, ctx context.Context, args []string) (int, error) {
	fs := a.flags(cmd)
	asJSON := jsonFlag(cmd, fs)
	goFlag := fs.Bool("go", false, "Continue ready work through installed plugins")
	kind := fs.String("kind", "", "native, or a source provided by an installed plugin")
	cause := fs.String("cause", "", "Only this stop cause")
	days := fs.Int("days", work.DefaultWindowDays, "How far back to look")
	if _, err := a.parse(cmd, fs, args, 0, 0); err != nil {
		return 2, err
	}
	if *cause != "" && *cause != "usage_limit" && *cause != "queue_paused" {
		return 2, &usageError{"hotseat resume",
			fmt.Sprintf("argument --cause: invalid choice: '%s' (choose from 'usage_limit', 'queue_paused')", *cause)}
	}

	items, err := a.stopped(ctx, *days)
	if err != nil {
		a.errorln("✗ " + err.Error())
		return 1, nil
	}
	filtered := make([]work.StoppedItem, 0, len(items))
	for _, item := range items {
		if *kind != "" && item.Kind != *kind {
			continue
		}
		if *cause != "" && item.Cause != *cause {
			continue
		}
		filtered = append(filtered, item)
	}
	items = filtered

	snapshot, err := a.build(ctx)
	if err != nil {
		return 1, err
	}
	accounts := accountQuotas(snapshot.Accounts)
	defaultAcc := defaultAlias(snapshot.Accounts)
	for i := range items {
		readiness := work.ReadinessOf(items[i], accounts, defaultAcc)
		items[i].Readiness = &readiness
	}

	withClarp := a.clarpAvailable()
	if *asJSON {
		payload := map[string]any{"items": items, "default_account": nullable(defaultAcc)}
		failed := false
		if *goFlag {
			results := []any{}
			for _, item := range items {
				if !item.Readiness.Ready || (withClarp && item.Kind != "clarp") {
					continue
				}
				result, err := a.continueItem(ctx, item)
				if err != nil {
					failed = true
					results = append(results, map[string]any{"id": item.ID, "continued": false, "error": err.Error()})
					continue
				}
				results = append(results, continuationJSON(result))
			}
			payload["results"] = results
		}
		if err := a.emit(payload); err != nil {
			return 1, err
		}
		if failed {
			return 1, nil
		}
		return 0, nil
	}

	if !withClarp {
		for _, item := range items {
			a.printf("%s · %s\n", item.Name, item.CWD)
			a.printf("  claude --resume %s\n", item.ID)
		}
		if len(items) == 0 {
			a.println("No stopped native sessions found.")
		}
		if *goFlag {
			a.println("Native sessions require a terminal; use a resume command above.")
		}
		return 0, nil
	}

	if len(items) == 0 {
		a.printf("Nothing was stopped by a usage limit in the last %d days.\n", *days)
		return 0, nil
	}

	var ready, blocked []work.StoppedItem
	for _, item := range items {
		if item.Readiness.Ready {
			ready = append(ready, item)
		} else {
			blocked = append(blocked, item)
		}
	}

	causes := map[string]string{"usage_limit": "usage limit", "queue_paused": "queue paused",
		"waiting_for_account": "waiting for quota"}
	for _, item := range items {
		when := "—"
		if item.StoppedAt != 0 {
			when = epochTime(item.StoppedAt).Format("02 Jan 15:04")
		}
		state := item.Readiness
		var shown string
		switch {
		case state.Hard != nil && *state.Hard != "":
			shown = a.paint(*state.Hard, yellow)
		case item.Kind == "native":
			shown = a.paint("needs a terminal", dim)
		default:
			shown = a.paint("ready", green)
		}
		if state.Warning != nil && *state.Warning != "" && (state.Hard == nil || *state.Hard == "") {
			shown += a.paint(fmt.Sprintf("  (%s)", *state.Warning), dim)
		}
		cause, ok := causes[item.Cause]
		if !ok {
			cause = "stopped"
		}
		waiting := ""
		if item.Pending != 0 {
			waiting = a.paint(fmt.Sprintf(" · %d waiting", item.Pending), red)
		}

		a.printf("%s%s\n", a.paint(truncateRunes(item.Name, 28), bold), waiting)
		a.printf("  %s\n", a.paint(item.Kind+" · "+cause+" · "+when, dim))
		if item.CWD != "" {
			a.printf("  %s\n", a.paint(homePath(item.CWD), dim))
		}
		if item.LastMessage != "" {
			a.printf("  %s %s\n", a.paint("said:", dim), item.LastMessage)
		}
		a.printf("  %s\n", shown)
		a.println()
	}

	a.println()
	starving := work.StarvingModels(items, accounts)
	if len(starving) > 0 {
		a.println(a.paint("--- one model is holding up the rest ---", yellow))
		models := make([]string, 0, len(starving))
		for model := range starving {
			models = append(models, model)
		}
		sort.Strings(models)
		for _, model := range models {
			names := starving[model]
			who := strings.Join(names[:minInt(4, len(names))], ", ")
			if len(names) > 4 {
				who += "…"
			}
			a.printf("  No account can serve %s, wanted by %s.\n", a.paint(model, bold), who)
		}
		a.println("  Clarp requires one account to serve every parked agent at once, so" +
			"\n  these keep the other parked agents waiting too. Move them to a model" +
			"\n  that has quota, or release them, and the rest recover.")
		a.println()
	}

	a.println(a.paint("Inspect any of these: hotseat inspect <name>", dim))
	var paused []work.StoppedItem
	pendingTurns := 0
	for _, item := range items {
		if item.Cause == "queue_paused" {
			paused = append(paused, item)
			pendingTurns += item.Pending
		}
	}
	if len(paused) > 0 {
		holding := ""
		if pendingTurns != 0 {
			holding = fmt.Sprintf(", holding %d turn(s) that will not run", pendingTurns)
		}
		a.println(a.paint(fmt.Sprintf("%d agent(s) have a paused queue", len(paused))+holding+
			". A prompt would queue behind the pause, not run, so these are "+
			"not continued here. Clear the pause in the Clarp app.", yellow))
	}
	exhausted := 0
	for _, item := range blocked {
		if item.Cause != "queue_paused" {
			exhausted++
		}
	}
	if exhausted > 0 {
		a.println(a.paint(fmt.Sprintf("%d cannot continue yet; continuing into an exhausted account just fails again.", exhausted), dim))
	}

	var clarpReady, native []work.StoppedItem
	for _, item := range ready {
		switch item.Kind {
		case "clarp":
			clarpReady = append(clarpReady, item)
		case "native":
			native = append(native, item)
		}
	}

	if !*goFlag {
		if len(clarpReady) > 0 {
			a.printf("%d Clarp agent(s) can be continued now: %s\n", len(clarpReady), a.paint("hotseat resume --go", bold))
		}
		if len(native) > 0 {
			a.printf("%d native session(s) have no supervisor to prompt. Resume one yourself:\n", len(native))
			for _, item := range native[:minInt(5, len(native))] {
				a.printf("  cd %s && claude --resume %s\n", item.CWD, item.ID)
			}
			if len(native) > 5 {
				a.printf("  ... and %d more (--json for all)\n", len(native)-5)
			}
		}
		return 0, nil
	}

	if len(clarpReady) == 0 {
		a.println("Nothing to continue.")
		return 0, nil
	}

	failures := 0
	for _, item := range clarpReady {
		if _, err := a.continueItem(ctx, item); err != nil {
			failures++
			a.errorln(a.paint(fmt.Sprintf("✗ %s: %s", item.Name, err.Error()), red))
			continue
		}
		a.println(a.paint("✓ continued "+item.Name, green))
	}
	if failures > 0 {
		return 1, nil
	}
	return 0, nil
}

// continuationJSON is the dict the Python continue_item returned: five keys for
// a native item, three for a Clarp one.
func continuationJSON(result work.Continuation) map[string]any {
	if result.Kind == "clarp" {
		return map[string]any{"id": result.ID, "kind": result.Kind, "continued": result.Continued}
	}
	return map[string]any{"id": result.ID, "kind": result.Kind, "continued": result.Continued,
		"command": result.Command, "cwd": nullable(result.CWD)}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// cmdInspect shows what one stopped piece of work was doing, so you can decide about it.
func (a *app) cmdInspect(cmd *command, ctx context.Context, args []string) (int, error) {
	fs := a.flags(cmd)
	asJSON := jsonFlag(cmd, fs)
	full := fs.Bool("full", false, "Also show the last few exchanges")
	positional, err := a.parse(cmd, fs, args, 1, 1, "id")
	if err != nil {
		return 2, err
	}
	detail, err := a.inspect(ctx, positional[0])
	if err != nil {
		a.errorln("✗ " + err.Error())
		return 1, nil
	}
	if *asJSON {
		return 0, a.emit(detail)
	}

	backend := detail.Backend
	if backend == "" {
		backend = "?"
	}
	head := fmt.Sprintf("%s  %s · %s", a.paint(detail.Name, bold), detail.Kind, backend)
	if detail.Model != nil && *detail.Model != "" {
		head += " · " + *detail.Model
	}
	a.println(head)
	if detail.CWD != nil && *detail.CWD != "" {
		branch := ""
		if detail.Branch != nil && *detail.Branch != "" {
			branch = fmt.Sprintf(" (%s)", *detail.Branch)
		}
		a.printf("  %s %s%s\n", a.paint("working in", dim), *detail.CWD, branch)
	}
	if detail.LastActivity != nil && *detail.LastActivity != "" {
		a.printf("  %s %s\n", a.paint("last activity", dim), *detail.LastActivity)
	}
	a.println()

	if detail.Summary != "" {
		a.println(a.paint("What it was working on", bold))
		a.printf("  %s\n", detail.Summary)
		a.println()
	}
	if detail.LastUser != "" {
		a.println(a.paint("Last asked", bold))
		a.printf("  %s\n", detail.LastUser)
		a.println()
	}
	if detail.LastAssistant != "" {
		a.println(a.paint("Last said", bold))
		a.printf("  %s\n", detail.LastAssistant)
		a.println()
	}
	for _, item := range queuedTurns(detail.Queued) {
		a.println(a.paint(fmt.Sprintf("Queued and waiting (%s)", deref(item.Origin, "unknown")), yellow))
		a.printf("  %s\n", item.Text)
		a.println()
	}
	if *full {
		a.println(a.paint("Recent exchange", bold))
		for _, turn := range detail.Recent {
			who := detail.Name
			if turn.Role == "user" {
				who = "you"
			}
			a.printf("  %s %s\n", a.paint(who+":", dim), turn.Text)
		}
	}
	return 0, nil
}

// queuedTurns reads the queued entries, whatever concrete type produced them.
func queuedTurns(queued []any) []clarp.QueuedTurn {
	out := make([]clarp.QueuedTurn, 0, len(queued))
	for _, entry := range queued {
		if turn, ok := entry.(clarp.QueuedTurn); ok {
			out = append(out, turn)
			continue
		}
		raw, err := json.Marshal(entry)
		if err != nil {
			continue
		}
		var turn clarp.QueuedTurn
		if json.Unmarshal(raw, &turn) == nil {
			out = append(out, turn)
		}
	}
	return out
}

// cmdClarp lists Clarp agents, and which account the running ones are spending.
func (a *app) cmdClarp(cmd *command, ctx context.Context, args []string) (int, error) {
	fs := a.flags(cmd)
	asJSON := jsonFlag(cmd, fs)
	live := fs.Bool("live", false, "")
	backend := fs.String("backend", "", "")
	if _, err := a.parse(cmd, fs, args, 0, 0); err != nil {
		return 2, err
	}
	if !a.clarpAvailable() {
		fmt.Fprintf(a.stderr, "✗ Clarp is not installed on this machine: no state database at %s.\n", clarp.DefaultStatePath())
		return 1, nil
	}
	snapshot, err := a.build(ctx)
	if err != nil {
		return 1, err
	}
	report, err := a.clarpReport(ctx, defaultAlias(snapshot.Accounts), *live, *backend)
	if err != nil {
		a.errorln("✗ " + err.Error())
		return 1, nil
	}
	if *asJSON {
		return 0, a.emit(report.Overview)
	}

	agents := report.Agents
	if len(agents) == 0 {
		if report.Filtered {
			a.println("No Clarp agents match.")
		} else {
			a.println("No Clarp agents defined.")
		}
		return 0, nil
	}

	a.printf("%s %s %s ACCOUNT\n", leftPad("AGENT", 26), leftPad("BACKEND", 8), leftPad("STATE", 8))
	for _, agent := range agents {
		state := a.paint("idle", dim)
		if agent.Live {
			state = a.paint("live", green)
		}
		pad := runeLen(state) - 4
		var account string
		switch {
		case agent.Live:
			account = deref(agent.Account, "default")
			if agent.Pinned {
				account += " (pinned)"
			}
		case deref(agent.Backend, "") == clarp.ClaudeBackend:
			account = a.paint("would use "+deref(agent.WouldUse, "default"), dim)
		default:
			account = "—"
		}
		name := deref(agent.Persona, agent.Session)
		a.printf("%s %s %s %s\n", leftPad(truncateRunes(name, 25), 26), leftPad(deref(agent.Backend, "?"), 8),
			leftPad(state, 8+pad), account)
	}

	a.println()
	overview := report.Overview
	backends := make([]string, 0, len(overview.ByBackend))
	for name := range overview.ByBackend {
		backends = append(backends, name)
	}
	sort.Strings(backends)
	parts := make([]string, 0, len(backends))
	for _, name := range backends {
		parts = append(parts, fmt.Sprintf("%d %s", overview.ByBackend[name], name))
	}
	a.println(a.paint(fmt.Sprintf("%d agents (%s); %d live.", overview.Total, strings.Join(parts, ", "), overview.Live), dim))
	if len(overview.LiveByAccount) > 0 {
		aliases := make([]string, 0, len(overview.LiveByAccount))
		for alias := range overview.LiveByAccount {
			aliases = append(aliases, alias)
		}
		sort.Strings(aliases)
		spend := make([]string, 0, len(aliases))
		for _, alias := range aliases {
			spend = append(spend, fmt.Sprintf("%s: %d", alias, overview.LiveByAccount[alias]))
		}
		a.printf("Live Claude agents by account — %s\n", strings.Join(spend, ", "))
	} else if overview.ClaudeBacked > 0 {
		a.println(a.paint(fmt.Sprintf("%d agents are Claude-backed; none are running, so none are drawing quota right now.",
			overview.ClaudeBacked), dim))
	}
	return 0, nil
}
