// Package quota reads an account's Claude quota.
//
// The primary source is Anthropic's OAuth usage endpoint, the same one the Claude
// apps use for their own limit displays. It is strictly better than inferring
// quota from the rate-limit headers of a real request:
//
//   - it costs no inference and consumes no quota;
//   - it still answers when the account is rate limited, where a probe just fails;
//   - it reports per-model weekly limits, which the headers never expose.
//
// That last point matters. An account can sit at 53% of its overall weekly
// allowance while its weekly limit for one model family is completely spent, and
// only this endpoint can tell you that.
//
// The header probe (Probe, SummariseHeaders) is kept as a fallback, and the
// cooldown cache (Cache) retains dated results while the endpoint backs off.
package quota

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/mail"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// UsageEndpoint is the OAuth usage endpoint.
	UsageEndpoint = "https://api.anthropic.com/api/oauth/usage"
	// OAuthBeta is the anthropic-beta value that unlocks OAuth bearer tokens.
	OAuthBeta = "oauth-2025-04-20"
	// Timeout bounds every request this package makes.
	Timeout = 15 * time.Second
)

// SeverityOrder lists the severities the endpoint uses, worst first.
var SeverityOrder = []string{"critical", "serious", "warning", "normal"}

// Client is the HTTP client used by Fetch, Probe and ForToken. Tests inject their
// own through the *With variants rather than mutating it.
var Client = &http.Client{Timeout: Timeout}

// UsageError means the usage endpoint could not be reached, or refused the token.
type UsageError struct {
	Message string
	// RetryAfter is the parsed Retry-After header in seconds; nil when the
	// response carried none (Python: retry_after=None).
	RetryAfter *float64
}

func (e *UsageError) Error() string { return e.Message }

// Fetch returns the raw usage payload for an access token using Client.
func Fetch(ctx context.Context, token string) (map[string]any, error) {
	return FetchWith(ctx, Client, token)
}

// FetchWith is Fetch with an explicit HTTP client.
func FetchWith(ctx context.Context, client *http.Client, token string) (map[string]any, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, UsageEndpoint, nil)
	if err != nil {
		return nil, &UsageError{Message: err.Error()}
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("anthropic-beta", OAuthBeta)
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return nil, &UsageError{Message: err.Error()}
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
			return nil, &UsageError{Message: "the account rejected this token"}
		}
		return nil, &UsageError{
			Message:    fmt.Sprintf("usage endpoint returned %d", response.StatusCode),
			RetryAfter: retryAfter(response.Header.Get("Retry-After"), time.Now()),
		}
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, &UsageError{Message: err.Error()}
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, &UsageError{Message: err.Error()}
	}
	return payload, nil
}

// retryAfter parses a Retry-After header: a number of seconds, else an RFC 2822
// date (a naive date is UTC). Negative waits clamp to zero; unparsable is nil.
func retryAfter(header string, now time.Time) *float64 {
	if header == "" {
		return nil
	}
	if seconds, err := strconv.ParseFloat(strings.TrimSpace(header), 64); err == nil {
		return clampNonNegative(seconds)
	}
	at, err := mail.ParseDate(header)
	if err != nil {
		if at, err = http.ParseTime(header); err != nil {
			return nil
		}
	}
	return clampNonNegative(at.Sub(now).Seconds())
}

// clampNonNegative mirrors Python's max(0, x), which yields 0 for NaN too.
func clampNonNegative(x float64) *float64 {
	if math.IsNaN(x) || x < 0 {
		x = 0
	}
	return &x
}

// toFloat mirrors Python's float() on a decoded JSON value.
func toFloat(value any) (float64, bool) {
	switch x := value.(type) {
	case float64:
		return x, true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f, err == nil
	}
	return 0, false
}

// truthy mirrors Python's bool() on a decoded JSON value.
func truthy(value any) bool {
	switch x := value.(type) {
	case nil:
		return false
	case bool:
		return x
	case float64:
		return x != 0
	case string:
		return x != ""
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

// stringValue returns value when it is a non-empty string.
func stringValue(value any) string {
	s, _ := value.(string)
	return s
}

// percent is utilisation as a fraction. The endpoint sends percentages, not fractions.
func percent(window any) *float64 {
	m, ok := window.(map[string]any)
	if !ok {
		return nil
	}
	value, present := m["utilization"]
	if !present || value == nil {
		return nil
	}
	f, ok := toFloat(value)
	if !ok {
		return nil
	}
	f /= 100.0
	return &f
}

// Layouts accepted by datetime.fromisoformat: offset-aware first, then naive
// forms which Python interprets in local time.
var (
	isoAwareLayouts = []string{
		"2006-01-02T15:04:05Z07:00", "2006-01-02T15:04:05-0700", "2006-01-02T15:04:05-07",
		"2006-01-02 15:04:05Z07:00", "2006-01-02 15:04:05-0700", "2006-01-02 15:04:05-07",
		"2006-01-02T15:04Z07:00", "2006-01-02T15:04-0700", "2006-01-02T15:04-07",
	}
	isoNaiveLayouts = []string{
		"2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02T15:04", "2006-01-02 15:04",
		"2006-01-02T15", "2006-01-02",
	}
)

func parseISO(stamp string) (time.Time, bool) {
	for _, layout := range isoAwareLayouts {
		if t, err := time.Parse(layout, stamp); err == nil {
			return t, true
		}
	}
	for _, layout := range isoNaiveLayouts {
		if t, err := time.ParseInLocation(layout, stamp, time.Local); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// epoch reads a window's resets_at as epoch seconds.
func epoch(window any) *float64 {
	m, ok := window.(map[string]any)
	if !ok {
		return nil
	}
	stamp, ok := m["resets_at"].(string)
	if !ok || stamp == "" {
		return nil
	}
	t, ok := parseISO(stamp)
	if !ok {
		return nil
	}
	seconds := float64(t.Unix())
	return &seconds
}

// scopedLimits returns the per-model weekly limits, worst first.
func scopedLimits(payload map[string]any) []ScopedLimit {
	found := []ScopedLimit{}
	limits, _ := payload["limits"].([]any)
	for _, raw := range limits {
		limit, ok := raw.(map[string]any)
		if !ok || limit["kind"] != "weekly_scoped" {
			continue
		}
		scope, _ := limit["scope"].(map[string]any)
		model, _ := scope["model"].(map[string]any)
		name := stringValue(model["display_name"])
		if name == "" {
			name = stringValue(model["id"])
		}
		if name == "" {
			continue
		}
		pct, ok := toFloat(limit["percent"])
		if !ok {
			continue
		}
		used := pct / 100.0
		severity := stringValue(limit["severity"])
		if severity == "" {
			severity = "normal"
		}
		found = append(found, ScopedLimit{
			Model:     name,
			Used:      used,
			Severity:  severity,
			Reset:     epoch(limit),
			Exhausted: used >= 1.0,
		})
	}
	sort.SliceStable(found, func(i, j int) bool { return found[i].Used > found[j].Used })
	return found
}

// worstSeverity picks the worst severity among the limits, "normal" when there
// are none. Unknown severities rank after the known ones, and ties keep the first.
func worstSeverity(limits any) string {
	list, _ := limits.([]any)
	best, bestRank, found := "", 0, false
	for _, raw := range list {
		limit, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		severity := stringValue(limit["severity"])
		rank := 99
		for i, known := range SeverityOrder {
			if known == severity {
				rank = i
				break
			}
		}
		if !found || rank < bestRank {
			best, bestRank, found = severity, rank, true
		}
	}
	if !found {
		return "normal"
	}
	return best
}

// windowLimited: a locked window is exhausted even when the number has already
// rolled over.
func windowLimited(window any, used *float64) bool {
	m, ok := window.(map[string]any)
	if !ok {
		return false
	}
	return truthy(m["locked_reason"]) || (used != nil && *used >= 1.0)
}

// SummariseUsage reduces a usage payload to what the interfaces render.
// Utilization arrives as a percentage and is divided by 100.
func SummariseUsage(payload map[string]any) Summary {
	five := payload["five_hour"]
	seven := payload["seven_day"]
	extra, _ := payload["extra_usage"].(map[string]any)
	scoped := scopedLimits(payload)

	used5h := percent(five)
	used7d := percent(seven)
	limited := windowLimited(five, used5h) || windowLimited(seven, used7d)

	status := "allowed"
	if limited {
		status = "rejected"
	}
	blocked := []string{}
	for _, s := range scoped {
		if s.Exhausted {
			blocked = append(blocked, s.Model)
		}
	}
	return Summary{
		Source:    SourceUsage,
		Status:    status,
		Available: !limited,
		Limited:   limited,
		Severity:  worstSeverity(payload["limits"]),
		Used5h:    used5h,
		Reset5h:   epoch(five),
		Used7d:    used7d,
		Reset7d:   epoch(seven),
		// Per-model weekly limits. An account can be fine overall and still have
		// nothing left for one model.
		Scoped:            scoped,
		BlockedModels:     blocked,
		ExtraUsageEnabled: truthy(extra["is_enabled"]),
		ExtraUsageReason:  stringValue(extra["disabled_reason"]),
		Plan:              stringValue(payload["plan_type"]),
	}
}

// ForToken fetches and summarises the usage for an access token using Client.
func ForToken(ctx context.Context, token string) (Summary, error) {
	return ForTokenWith(ctx, Client, token)
}

// ForTokenWith is ForToken with an explicit HTTP client.
func ForTokenWith(ctx context.Context, client *http.Client, token string) (Summary, error) {
	payload, err := FetchWith(ctx, client, token)
	if err != nil {
		return Summary{}, err
	}
	return SummariseUsage(payload), nil
}
