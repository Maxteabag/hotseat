package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Maxteabag/hotseat/internal/claude"
	"github.com/Maxteabag/hotseat/internal/collect"
)

// autoCooldown is how long a watcher leaves the default alone after switching.
// Quota readings lag the API, so without it two near-limit accounts would trade
// the default back and forth.
const autoCooldown = 10 * time.Minute

// autoDecision is what one pass concluded and did.
type autoDecision struct {
	Action string   `json:"action"` // "none", "switched", "would_switch", "stuck"
	From   string   `json:"from"`
	To     string   `json:"to,omitempty"`
	Reason string   `json:"reason,omitempty"`
	Tried  []string `json:"tried,omitempty"`
	// Running is how many sessions were open at the switch. They keep the old
	// account until restarted.
	Running int `json:"running,omitempty"`
}

// canServe reports whether view can take work for models: not blocked on any of
// them. A blocked name matches a model ID when either contains the other, so
// "Sonnet" covers "claude-sonnet-5-5".
func canServe(view collect.AccountView, models []string) bool {
	if view.Usage == nil {
		return false
	}
	for _, blocked := range view.Usage.BlockedModels {
		b := strings.ToLower(blocked)
		for _, model := range models {
			m := strings.ToLower(model)
			if b != "" && (strings.Contains(m, b) || strings.Contains(b, m)) {
				return false
			}
		}
	}
	return true
}

// exhausted says why the account should be left, or "" when it is fine. An
// account whose quota could not be read is not judged: unknown is not empty.
func exhausted(view collect.AccountView, models []string, threshold float64) string {
	u := view.Usage
	switch {
	case u == nil:
		return ""
	case u.Limited:
		return "limited"
	case u.Used5h != nil && *u.Used5h >= threshold:
		return fmt.Sprintf("5-hour window at %.0f%%", *u.Used5h*100)
	case u.Used7d != nil && *u.Used7d >= threshold:
		return fmt.Sprintf("7-day window at %.0f%%", *u.Used7d*100)
	case !canServe(view, models):
		return "no quota left for the requested model"
	}
	return ""
}

// rankCandidates lists the accounts worth switching to, emptiest first.
func rankCandidates(views []collect.AccountView, models []string, threshold float64) []string {
	type scored struct {
		alias string
		load  float64
	}
	var found []scored
	for _, view := range views {
		if view.IsActive || strings.HasPrefix(view.Alias, "(") || view.Error != nil || view.Usage == nil {
			continue
		}
		if view.SigninDaysLeft != nil && *view.SigninDaysLeft <= 0 {
			continue
		}
		if exhausted(view, models, threshold) != "" {
			continue
		}
		load := 0.0
		for _, used := range []*float64{view.Usage.Used5h, view.Usage.Used7d} {
			if used != nil && *used > load {
				load = *used
			}
		}
		found = append(found, scored{view.Alias, load})
	}
	sort.SliceStable(found, func(i, j int) bool { return found[i].load < found[j].load })
	aliases := make([]string, len(found))
	for i, f := range found {
		aliases[i] = f.alias
	}
	return aliases
}

// autoPass looks at the snapshot once and, when the active account is spent,
// moves the default to the best account that a live API call confirms works.
func (a *app) autoPass(ctx context.Context, models []string, threshold float64, dryRun bool) (autoDecision, error) {
	snap, err := a.build(ctx)
	if err != nil {
		return autoDecision{}, err
	}
	var active *collect.AccountView
	for i := range snap.Accounts {
		if snap.Accounts[i].IsActive {
			active = &snap.Accounts[i]
		}
	}
	if active == nil {
		return autoDecision{}, fmt.Errorf("no active account; refusing to choose one blindly")
	}
	decision := autoDecision{Action: "none", From: active.Alias}
	reason := exhausted(*active, models, threshold)
	if reason == "" {
		return decision, nil
	}
	decision.Reason = reason
	backend := a.backend()
	for _, alias := range rankCandidates(snap.Accounts, models, threshold) {
		if dryRun {
			decision.Action, decision.To = "would_switch", alias
			return decision, nil
		}
		if _, err := a.verify(ctx, backend, alias); err != nil {
			decision.Tried = append(decision.Tried, alias+": "+err.Error())
			continue
		}
		if _, err := a.switchDefault(backend, alias, snap.Sessions); err != nil {
			return decision, err
		}
		decision.Action, decision.To, decision.Running = "switched", alias, snap.Sessions
		return decision, nil
	}
	decision.Action = "stuck"
	return decision, nil
}

// lockAuto stops two passes (a watcher and a Clarp hook, say) switching at
// once. The returned func releases it; nil means another pass holds it.
func (a *app) lockAuto(backend claude.Backend) (func(), error) {
	store, ok := backend.(claude.ProfileStore)
	if !ok {
		return func() {}, nil
	}
	handle, err := os.OpenFile(filepath.Join(store.ProfilesDir(), ".auto.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if !tryLock(handle) {
		handle.Close()
		return nil, nil
	}
	return func() { handle.Close() }, nil
}

// cmdAuto moves the default account off an exhausted one.
func (a *app) cmdAuto(cmd *command, ctx context.Context, args []string) (int, error) {
	fs := a.flags(cmd)
	asJSON := jsonFlag(cmd, fs)
	watch := fs.Bool("watch", false, "Keep running, checking every --interval")
	interval := fs.Duration("interval", 2*time.Minute, "Time between checks with --watch")
	percent := fs.Float64("threshold", 95, "Leave an account at this percent of a window")
	dryRun := fs.Bool("dry-run", false, "Say what would change, change nothing")
	selector := fs.Bool("select", false, "Clarp selector: read {\"models\": [...]} on stdin, print JSON")
	if _, err := a.parse(cmd, fs, args, 0, 0); err != nil {
		return 2, err
	}
	threshold := *percent / 100
	if threshold <= 0 || threshold > 1 {
		return 2, &usageError{"hotseat auto", "--threshold must be between 0 and 100"}
	}
	var models []string
	if *selector {
		var request struct {
			Models []string `json:"models"`
		}
		if err := json.NewDecoder(a.stdin).Decode(&request); err != nil || len(request.Models) == 0 {
			return 2, &usageError{"hotseat auto", `--select needs {"models": [...]} on stdin`}
		}
		models = request.Models
		*watch = false
	}

	release, err := a.lockAuto(a.backend())
	if err != nil {
		return 1, err
	}
	if release == nil {
		if *selector {
			return 1, a.emit(map[string]any{"available": false, "error": "another switch is running"})
		}
		a.errorln(a.paint("✗ another hotseat auto is switching right now", red))
		return 1, nil
	}
	defer release()

	var cooldownUntil time.Time
	for {
		code := 0
		if a.now().Before(cooldownUntil) {
			if !*asJSON {
				a.println(a.paint("cooling down after the last switch", dim))
			}
		} else {
			decision, err := a.autoPass(ctx, models, threshold, *dryRun)
			if err != nil {
				if !*watch {
					if *selector {
						return 1, a.emit(map[string]any{"available": false, "error": err.Error()})
					}
					return 1, err
				}
				a.errorln(a.paint("✗ "+err.Error(), red))
			} else {
				if decision.Action == "switched" {
					cooldownUntil = a.now().Add(autoCooldown)
				}
				if decision.Action == "stuck" {
					code = 1
				}
				a.reportAuto(decision, *asJSON, *selector, models)
			}
		}
		if !*watch {
			return code, nil
		}
		select {
		case <-ctx.Done():
			return 0, nil
		case <-time.After(*interval):
		}
	}
}

func (a *app) reportAuto(d autoDecision, asJSON, selector bool, models []string) {
	switch {
	case selector:
		profile := d.From
		if d.To != "" {
			profile = d.To
		}
		a.emit(map[string]any{"available": d.Action != "stuck", "profile": profile, "models": models})
	case asJSON:
		a.emit(d)
	case d.Action == "none":
		a.println(a.paint(fmt.Sprintf("✓ %s has quota; nothing to do", d.From), green))
	case d.Action == "switched":
		a.println(a.paint(fmt.Sprintf("✓ %s is %s; default is now %s", d.From, d.Reason, d.To), green))
		if d.Running > 0 {
			a.println(fmt.Sprintf("  %d running session(s) keep %s until restarted; `hotseat resume --go` continues any a limit stops", d.Running, d.From))
		}
	case d.Action == "would_switch":
		a.println(fmt.Sprintf("%s is %s; would switch to %s", d.From, d.Reason, d.To))
	default:
		a.println(a.paint(fmt.Sprintf("✗ %s is %s and no other account can take over", d.From, d.Reason), red))
		for _, why := range d.Tried {
			a.println("  " + why)
		}
	}
}
