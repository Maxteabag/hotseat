package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Maxteabag/hotseat/internal/collect"
	"github.com/Maxteabag/hotseat/internal/quota"
)

func TestHomeDirectoryIsShortened(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip(err)
	}
	if got := homePath(filepath.Join(home, "work", "repo")); got != "~/work/repo" {
		t.Errorf("got %q", got)
	}
	if got := homePath("/var/tmp/thing"); got != "/var/tmp/thing" {
		t.Errorf("got %q", got)
	}
	if got := homePath(""); got != "" {
		t.Errorf("got %q", got)
	}
}

func TestPct(t *testing.T) {
	if got := pct(nil); got != "—" {
		t.Errorf("nil: %q", got)
	}
	for value, want := range map[float64]string{0.36: "36%", 1.03: "103%", 0: "0%", 0.005: "0%", 0.015: "2%"} {
		if got := pct(&value); got != want {
			t.Errorf("%v: got %q want %q (Python rounds half to even)", value, got, want)
		}
	}
}

func TestTokens(t *testing.T) {
	for count, want := range map[int64]string{999: "999", 1000: "1.0K", 225_000: "225.0K", 1_500_000: "1.5M",
		2_000_000_000: "2.0B", 1_500_000_000_000: "1,500.0B"} {
		if got := tokens(count); got != want {
			t.Errorf("%d: got %q want %q", count, got, want)
		}
	}
}

func TestStrip(t *testing.T) {
	if got := strip(red + "x" + reset + dim + "y" + reset); got != "xy" {
		t.Errorf("got %q", got)
	}
}

func TestPaintOnlyOnATerminal(t *testing.T) {
	a := &app{stdoutIsTTY: func() bool { return false }}
	if got := a.paint("x", red); got != "x" {
		t.Errorf("got %q", got)
	}
	a.stdoutIsTTY = func() bool { return true }
	if got := a.paint("x", red); got != "\033[31mx\033[0m" {
		t.Errorf("got %q", got)
	}
}

func TestClock(t *testing.T) {
	noon := time.Date(2026, 3, 4, 12, 0, 0, 0, time.Local)
	a := &app{now: func() time.Time { return noon }}
	if got := a.clock(nil); got != "—" {
		t.Errorf("nil: %q", got)
	}
	zero := 0.0
	if got := a.clock(&zero); got != "—" {
		t.Errorf("zero: %q", got)
	}
	sameDay := float64(noon.Add(2 * time.Hour).Unix())
	if got := a.clock(&sameDay); got != "14:00" {
		t.Errorf("same day: %q", got)
	}
	tomorrow := float64(noon.Add(26 * time.Hour).Unix())
	if got := a.clock(&tomorrow); got != "Thu 14:00" {
		t.Errorf("tomorrow: %q", got)
	}
}

func TestStatusWord(t *testing.T) {
	limited := &quota.Summary{Limited: true}
	cases := []struct {
		account collect.AccountView
		want    string
	}{
		{collect.AccountView{Error: ptrOf("boom")}, "unavailable"},
		{collect.AccountView{}, "unknown"},
		{collect.AccountView{Usage: limited}, "limited"},
		{collect.AccountView{Usage: &quota.Summary{}}, "available"},
	}
	for _, tc := range cases {
		if got := statusWord(tc.account); got != tc.want {
			t.Errorf("got %q want %q", got, tc.want)
		}
	}
}

func TestEmitMatchesPythonIndentation(t *testing.T) {
	out := &bytes.Buffer{}
	a := &app{stdout: out}
	if err := a.emit(map[string]any{"a": []int{1}, "b": "<x>"}); err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"a\": [\n    1\n  ],\n  \"b\": \"<x>\"\n}\n"
	if out.String() != want {
		t.Errorf("got %q want %q", out.String(), want)
	}
}

func TestPadding(t *testing.T) {
	if got := leftPad("ab", 4); got != "ab  " {
		t.Errorf("leftPad %q", got)
	}
	if got := rightPad("—", 3); got != "  —" {
		t.Errorf("rightPad counts characters, not bytes: %q", got)
	}
	if got := truncateRunes("héllo", 2); got != "hé" {
		t.Errorf("truncate %q", got)
	}
}
