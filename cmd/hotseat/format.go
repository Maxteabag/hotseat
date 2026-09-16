package main

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/Maxteabag/hotseat/internal/collect"
)

// The same ANSI codes the Python used, enabled only when stdout is a terminal.
const (
	bold   = "\033[1m"
	dim    = "\033[2m"
	red    = "\033[31m"
	green  = "\033[32m"
	yellow = "\033[33m"
	reset  = "\033[0m"
)

var ansiPattern = regexp.MustCompile("\033\\[[0-9;]*m")

// paint wraps text in a colour code when stdout is a terminal.
func (a *app) paint(text, code string) string {
	if a.colour() {
		return code + text + reset
	}
	return text
}

// emit prints payload as indented JSON, the way `json.dump(indent=2)` did.
func (a *app) emit(payload any) error {
	encoder := json.NewEncoder(a.stdout)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	return encoder.Encode(payload)
}

func (a *app) println(args ...any) { fmt.Fprintln(a.stdout, args...) }

func (a *app) printf(format string, args ...any) { fmt.Fprintf(a.stdout, format, args...) }

func (a *app) errorln(args ...any) { fmt.Fprintln(a.stderr, args...) }

// strip is text without colour codes, for width calculations.
func strip(text string) string { return ansiPattern.ReplaceAllString(text, "") }

// homePath shortens a path for display, so the useful end is not pushed off
// the line.
func homePath(path string) string {
	if path == "" {
		return ""
	}
	home := homeDir()
	if home != "" && strings.HasPrefix(path, home) {
		return "~" + path[len(home):]
	}
	return path
}

// pyRound is Python's round(): half to even.
func pyRound(value float64) int { return int(math.RoundToEven(value)) }

func pct(value *float64) string {
	if value == nil {
		return "—"
	}
	return fmt.Sprintf("%d%%", pyRound(*value*100))
}

// tokens is a readable token count. 225000 is 225K, not a misleading 0M.
func tokens(count int64) string {
	for _, unit := range []struct {
		limit  float64
		suffix string
	}{{1e9, "B"}, {1e6, "M"}, {1e3, "K"}} {
		if float64(count) >= unit.limit {
			return groupThousands(fmt.Sprintf("%.1f", float64(count)/unit.limit)) + unit.suffix
		}
	}
	return fmt.Sprintf("%d", count)
}

// groupThousands inserts commas into the integer part of a formatted number,
// like Python's "," format specifier.
func groupThousands(number string) string {
	whole, fraction, _ := strings.Cut(number, ".")
	var out strings.Builder
	for i, digit := range whole {
		if i > 0 && (len(whole)-i)%3 == 0 {
			out.WriteByte(',')
		}
		out.WriteRune(digit)
	}
	if fraction != "" {
		out.WriteByte('.')
		out.WriteString(fraction)
	}
	return out.String()
}

// clock renders an epoch as a wall-clock time, with the weekday when it is not today.
func (a *app) clock(epoch *float64) string {
	if epoch == nil || *epoch == 0 {
		return "—"
	}
	when := epochTime(*epoch)
	if when.Format("2006-01-02") == a.now().Format("2006-01-02") {
		return when.Format("15:04")
	}
	return when.Format("Mon 15:04")
}

func epochTime(epoch float64) time.Time {
	seconds, fraction := math.Modf(epoch)
	return time.Unix(int64(seconds), int64(fraction*1e9)).In(time.Local)
}

func statusWord(account collect.AccountView) string {
	if account.Error != nil && *account.Error != "" {
		return "unavailable"
	}
	if account.Usage == nil {
		return "unknown"
	}
	if account.Usage.Limited {
		return "limited"
	}
	return "available"
}

// deref is Python's `value or fallback` for optional strings.
func deref(value *string, fallback string) string {
	if value == nil || *value == "" {
		return fallback
	}
	return *value
}

// runeLen counts characters the way Python's len() does for width padding.
func runeLen(s string) int { return len([]rune(s)) }

// leftPad and rightPad are Python's :<N and :>N, counting characters.
func leftPad(s string, width int) string {
	if n := runeLen(s); n < width {
		return s + strings.Repeat(" ", width-n)
	}
	return s
}

func rightPad(s string, width int) string {
	if n := runeLen(s); n < width {
		return strings.Repeat(" ", width-n) + s
	}
	return s
}

// truncateRunes is Python's s[:n].
func truncateRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) > n {
		return string(runes[:n])
	}
	return s
}
