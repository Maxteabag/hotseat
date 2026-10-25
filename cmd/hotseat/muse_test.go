package main

import (
	"context"
	"strings"
	"testing"

	"github.com/Maxteabag/hotseat/internal/muse"
)

func museFixture() *muse.Overview {
	used5, used7 := 42.0, 81.0
	reset5, reset7 := 1767272400.0, 1767700800.0
	return &muse.Overview{Installed: true, LoggedIn: true, Account: &muse.Account{
		Email: "user@example.com", Tier: "Muse Code Power Usage", Subscribed: true,
		Windows: []muse.Window{{Label: "5-hour", UsedPercent: &used5, ResetsAt: &reset5},
			{Label: "weekly", UsedPercent: &used7, ResetsAt: &reset7}}}}
}

func TestMuseRenders(t *testing.T) {
	h := newHarness(t)
	h.museOverview = func(context.Context) (*muse.Overview, error) { return museFixture(), nil }
	if code := h.run("muse"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.err.String())
	}
	for _, want := range []string{"*user@example.com", "Muse Code Power Usage", "5-hour", "42%", "weekly", "81%"} {
		if !strings.Contains(h.out.String(), want) {
			t.Errorf("missing %q in:\n%s", want, h.out.String())
		}
	}
}

func TestMuseAbsentSaysHowToInstall(t *testing.T) {
	h := newHarness(t)
	if code := h.run("muse"); code != 1 || !strings.Contains(h.out.String(), "not installed") {
		t.Errorf("exit %d, out %q", code, h.out.String())
	}
}

func TestMuseSignedOutSaysHowToLogIn(t *testing.T) {
	h := newHarness(t)
	h.museOverview = func(context.Context) (*muse.Overview, error) { return &muse.Overview{Installed: true}, nil }
	if code := h.run("muse"); code != 1 || !strings.Contains(h.out.String(), "muse login") {
		t.Errorf("exit %d, out %q", code, h.out.String())
	}
}

func TestMuseNoFiguresYetIsNotAFailure(t *testing.T) {
	h := newHarness(t)
	h.museOverview = func(context.Context) (*muse.Overview, error) {
		return &muse.Overview{Installed: true, LoggedIn: true, Account: &muse.Account{Email: "user@example.com",
			Reason: "server_sent_no_usage", Windows: []muse.Window{}}}, nil
	}
	if code := h.run("muse"); code != 0 || !strings.Contains(h.out.String(), "server_sent_no_usage") {
		t.Errorf("exit %d, out %q", code, h.out.String())
	}
}

func TestMuseJSON(t *testing.T) {
	h := newHarness(t)
	h.museOverview = func(context.Context) (*muse.Overview, error) { return museFixture(), nil }
	if code := h.run("muse", "--json"); code != 0 || !strings.Contains(h.out.String(), `"tier": "Muse Code Power Usage"`) {
		t.Errorf("exit %d, out %q", code, h.out.String())
	}
}
