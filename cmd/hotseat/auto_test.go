package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Maxteabag/hotseat/internal/claude"
	"github.com/Maxteabag/hotseat/internal/collect"
	"github.com/Maxteabag/hotseat/internal/quota"
)

func autoView(alias string, active bool, used5, used7 float64, blocked ...string) collect.AccountView {
	view := collect.AccountView{
		PublicAccount: claude.PublicAccount{Alias: alias, IsActive: active},
		Usage:         &quota.Summary{Available: true, Used5h: &used5, Used7d: &used7, BlockedModels: blocked},
	}
	view.Usage.Limited = used5 >= 1 || used7 >= 1
	return view
}

func autoHarness(t *testing.T, views ...collect.AccountView) (*harness, *[]string) {
	h := newHarness(t)
	h.build = func(context.Context) (collect.Snapshot, error) {
		return collect.Snapshot{Sessions: 2, Accounts: views}, nil
	}
	var switched []string
	h.switchDefault = func(_ claude.Backend, alias string, _ int) (claude.SwitchResult, error) {
		switched = append(switched, alias)
		return claude.SwitchResult{Alias: alias, Switched: true}, nil
	}
	h.verify = func(context.Context, claude.Backend, string) (claude.VerifyResult, error) {
		return claude.VerifyResult{OK: true}, nil
	}
	return h, &switched
}

func TestAutoLeavesAHealthyAccountAlone(t *testing.T) {
	h, switched := autoHarness(t, autoView("a", true, 0.4, 0.5), autoView("b", false, 0.1, 0.1))
	if code := h.run("auto"); code != 0 || len(*switched) != 0 {
		t.Errorf("exit %d, switched %v", code, *switched)
	}
}

func TestAutoSwitchesToTheEmptiestAccount(t *testing.T) {
	h, switched := autoHarness(t,
		autoView("a", true, 1.0, 0.8),
		autoView("busy", false, 0.9, 0.5),
		autoView("fresh", false, 0.1, 0.3))
	if code := h.run("auto"); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, h.err.String())
	}
	if len(*switched) != 1 || (*switched)[0] != "fresh" {
		t.Errorf("switched %v", *switched)
	}
}

func TestAutoSwitchesBeforeTheLimitAtThreshold(t *testing.T) {
	h, switched := autoHarness(t, autoView("a", true, 0.96, 0.2), autoView("b", false, 0.1, 0.1))
	h.run("auto")
	if len(*switched) != 1 {
		t.Errorf("switched %v", *switched)
	}
	h, switched = autoHarness(t, autoView("a", true, 0.96, 0.2), autoView("b", false, 0.1, 0.1))
	h.run("auto", "--threshold", "99")
	if len(*switched) != 0 {
		t.Errorf("threshold 99 should keep a: %v", *switched)
	}
}

func TestAutoSkipsAccountsThatCannotServe(t *testing.T) {
	expired := -1.0
	signedOut := autoView("old", false, 0.0, 0.0)
	signedOut.SigninDaysLeft = &expired
	broken := autoView("broken", false, 0.0, 0.0)
	msg := "401"
	broken.Error = &msg
	h, switched := autoHarness(t,
		autoView("a", true, 1.0, 0.8),
		signedOut, broken,
		autoView("sonnet-out", false, 0.1, 0.1, "Sonnet"),
		autoView("ok", false, 0.5, 0.5))
	h.run("auto")
	if len(*switched) != 1 || (*switched)[0] != "sonnet-out" {
		// Without a model request, a blocked model does not matter.
		t.Errorf("switched %v", *switched)
	}
	if got := rankCandidates(h.build2(), []string{"claude-sonnet-5-5"}, 0.95); len(got) != 1 || got[0] != "ok" {
		t.Errorf("sonnet request ranked %v", got)
	}
}

func (h *harness) build2() []collect.AccountView {
	snap, _ := h.build(context.Background())
	return snap.Accounts
}

func TestAutoTriesTheNextCandidateWhenTheLiveCheckFails(t *testing.T) {
	h, switched := autoHarness(t,
		autoView("a", true, 1.0, 0.8),
		autoView("dead", false, 0.0, 0.0),
		autoView("good", false, 0.3, 0.3))
	h.verify = func(_ context.Context, _ claude.Backend, alias string) (claude.VerifyResult, error) {
		if alias == "dead" {
			return claude.VerifyResult{}, errors.New("401")
		}
		return claude.VerifyResult{OK: true}, nil
	}
	h.run("auto")
	if len(*switched) != 1 || (*switched)[0] != "good" {
		t.Errorf("switched %v", *switched)
	}
}

func TestAutoWithNowhereToGoFailsAndKeepsTheDefault(t *testing.T) {
	h, switched := autoHarness(t, autoView("a", true, 1.0, 0.8), autoView("b", false, 1.0, 1.0))
	if code := h.run("auto"); code != 1 || len(*switched) != 0 {
		t.Errorf("exit %d, switched %v", code, *switched)
	}
	if !strings.Contains(h.out.String(), "no other account") {
		t.Errorf("out %q", h.out.String())
	}
}

func TestAutoDryRunChangesNothing(t *testing.T) {
	h, switched := autoHarness(t, autoView("a", true, 1.0, 0.8), autoView("b", false, 0.1, 0.1))
	h.verify = func(context.Context, claude.Backend, string) (claude.VerifyResult, error) {
		t.Error("dry run must not call the API")
		return claude.VerifyResult{}, nil
	}
	if code := h.run("auto", "--dry-run", "--json"); code != 0 || len(*switched) != 0 {
		t.Fatalf("exit %d, switched %v", code, *switched)
	}
	if got := h.json(t); got["action"] != "would_switch" || got["to"] != "b" {
		t.Errorf("decision %v", got)
	}
}

func TestAutoSelectorSpeaksClarpsProtocol(t *testing.T) {
	h, _ := autoHarness(t, autoView("a", true, 1.0, 0.8), autoView("b", false, 0.1, 0.1))
	h.stdin = strings.NewReader(`{"models":["claude-sonnet-5-5"]}`)
	if code := h.run("auto", "--select"); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, h.err.String())
	}
	got := h.json(t)
	if got["available"] != true || got["profile"] != "b" {
		t.Errorf("answer %v", got)
	}

	h, _ = autoHarness(t, autoView("a", true, 1.0, 0.8), autoView("b", false, 1.0, 1.0))
	h.stdin = strings.NewReader(`{"models":["m"]}`)
	h.run("auto", "--select")
	if got := h.json(t); got["available"] != false {
		t.Errorf("nowhere to go should be unavailable: %v", got)
	}
}

func TestAutoRefusesWithoutAnActiveAccount(t *testing.T) {
	h, switched := autoHarness(t, autoView("a", false, 1.0, 1.0), autoView("b", false, 0.1, 0.1))
	if code := h.run("auto"); code != 1 || len(*switched) != 0 {
		t.Errorf("exit %d, switched %v", code, *switched)
	}
}
