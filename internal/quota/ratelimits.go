package quota

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
)

// Read an account's quota straight from the API's rate-limit headers.
//
// A minimal request is the only way to learn an account's standing when the
// usage endpoint is unavailable: the numbers arrive as response headers rather
// than from any status endpoint. The request is deliberately as small as
// possible and its answer is discarded.

const (
	// APIURL is the messages endpoint the probe posts to.
	APIURL = "https://api.anthropic.com/v1/messages"
	// AnthropicVersion is the API version header the probe sends.
	AnthropicVersion = "2023-06-01"
	// UserAgent identifies the probe.
	UserAgent = "hotseat/0.1"
)

// ProbeModels are tried in order; a retired model answers 404 and the next one is used.
var ProbeModels = []string{"claude-haiku-4-5-20251001", "claude-3-5-haiku-20241022"}

// ProbeError means the API could not be reached, or refused the token outright.
type ProbeError struct {
	Message string
}

func (e *ProbeError) Error() string { return e.Message }

// Headers are response headers with lower-cased names; the last value wins for
// a repeated header, as with Python's dict comprehension over headers.items().
type Headers map[string]string

func lowerHeaders(h http.Header) Headers {
	out := make(Headers, len(h))
	for name, values := range h {
		if len(values) == 0 {
			continue
		}
		out[strings.ToLower(name)] = values[len(values)-1]
	}
	return out
}

// Probe returns the rate-limit headers for an access token using Client.
//
// A 429 is a perfectly good answer: it still carries the headers that say when
// the limit resets, which is exactly what the dashboard needs to show.
func Probe(ctx context.Context, token string) (Headers, error) {
	return ProbeWith(ctx, Client, token)
}

// ProbeWith is Probe with an explicit HTTP client.
func ProbeWith(ctx context.Context, client *http.Client, token string) (Headers, error) {
	lastError := ""
	for _, model := range ProbeModels {
		name, _ := json.Marshal(model)
		body := fmt.Sprintf(`{"model": %s, "max_tokens": 1, "messages": [{"role": "user", "content": "hi"}]}`, name)
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, APIURL, strings.NewReader(body))
		if err != nil {
			lastError = err.Error()
			continue
		}
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("anthropic-version", AnthropicVersion)
		request.Header.Set("anthropic-beta", OAuthBeta)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("User-Agent", UserAgent)
		response, err := client.Do(request)
		if err != nil {
			lastError = err.Error()
			continue
		}
		headers := lowerHeaders(response.Header)
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		response.Body.Close()
		switch {
		case response.StatusCode >= 200 && response.StatusCode <= 299:
			return headers, nil
		case response.StatusCode == http.StatusNotFound:
			lastError = fmt.Sprintf("model %s unavailable", model)
			continue
		case response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden:
			return nil, &ProbeError{Message: "the account rejected this token"}
		default:
			return headers, nil
		}
	}
	if lastError == "" {
		lastError = "no response from the API"
	}
	return nil, &ProbeError{Message: lastError}
}

// fraction: utilisation arrives as a ready-made fraction, for example 0.36 for 36%.
func fraction(headers Headers, name string) *float64 {
	raw, ok := headers[name]
	if !ok {
		return nil
	}
	value, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		return nil
	}
	if math.IsNaN(value) || value < 0 {
		value = 0
	}
	return &value
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// epochHeader returns the first header among names that is a plain digit string.
func epochHeader(headers Headers, names ...string) *float64 {
	for _, name := range names {
		raw := headers[name]
		if !isDigits(raw) {
			continue
		}
		value, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			continue
		}
		return &value
	}
	return nil
}

func headerOr(headers Headers, name, fallback string) string {
	if value, ok := headers[name]; ok {
		return value
	}
	return fallback
}

// SummariseHeaders reduces the raw headers to the few numbers the dashboard renders.
// Utilization is already a fraction and is not rescaled.
//
// An account can be rejected on its 5-hour window yet still serve requests from
// purchased extra usage, so availability follows the status fields rather than
// being inferred from utilisation.
func SummariseHeaders(headers Headers) Summary {
	unified := headerOr(headers, "anthropic-ratelimit-unified-status", "unknown")
	status5h := headerOr(headers, "anthropic-ratelimit-unified-5h-status", "unknown")
	status7d := headerOr(headers, "anthropic-ratelimit-unified-7d-status", "unknown")
	available := unified == "allowed" || status5h == "allowed"
	return Summary{
		Source:    SourceProbe,
		Status:    unified,
		Status5h:  status5h,
		Status7d:  status7d,
		Available: available,
		Limited:   !available,
		Used5h:    fraction(headers, "anthropic-ratelimit-unified-5h-utilization"),
		Reset5h: epochHeader(headers, "anthropic-ratelimit-unified-5h-reset",
			"anthropic-ratelimit-unified-reset"),
		Used7d:        fraction(headers, "anthropic-ratelimit-unified-7d-utilization"),
		Reset7d:       epochHeader(headers, "anthropic-ratelimit-unified-7d-reset"),
		OverageStatus: headers["anthropic-ratelimit-unified-overage-status"],
		OverageReason: headers["anthropic-ratelimit-unified-overage-disabled-reason"],
		// Which window is currently the binding constraint, straight from the API.
		Binding:    headers["anthropic-ratelimit-unified-representative-claim"],
		Fallback:   headers["anthropic-ratelimit-unified-fallback"],
		FallbackAt: fraction(headers, "anthropic-ratelimit-unified-fallback-percentage"),
	}
}
