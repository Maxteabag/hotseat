package quota

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Parsing the OAuth usage payload.
//
// Fixtures mirror the real shape, including the per-model weekly limit that the
// rate-limit headers never expose.

const usagePayload = `{
    "five_hour": {"utilization": 36.0, "resets_at": "2026-09-14T16:50:00+00:00",
                  "locked_reason": null},
    "seven_day": {"utilization": 53.0, "resets_at": "2026-09-20T15:00:00+00:00",
                  "locked_reason": null},
    "extra_usage": {"is_enabled": false, "disabled_reason": "out_of_credits"},
    "limits": [
        {"kind": "session", "percent": 36, "severity": "normal",
         "resets_at": "2026-09-14T16:50:00+00:00", "scope": null},
        {"kind": "weekly_all", "percent": 53, "severity": "normal",
         "resets_at": "2026-09-20T15:00:00+00:00", "scope": null},
        {"kind": "weekly_scoped", "percent": 100, "severity": "critical",
         "resets_at": "2026-09-20T15:00:00+00:00",
         "scope": {"model": {"display_name": "Fable", "id": "fable"}}}
    ]
}`

func decode(t *testing.T, raw string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	return out
}

func payload(t *testing.T) map[string]any { return decode(t, usagePayload) }

// with returns a shallow copy of m with key replaced, like {**m, key: value}.
func with(m map[string]any, key string, value any) map[string]any {
	out := make(map[string]any, len(m)+1)
	for k, v := range m {
		out[k] = v
	}
	out[key] = value
	return out
}

func near(t *testing.T, got *float64, want float64) {
	t.Helper()
	if got == nil {
		t.Fatalf("got nil, want %v", want)
	}
	if math.Abs(*got-want) > 1e-7 {
		t.Fatalf("got %v, want %v", *got, want)
	}
}

// handlerTransport serves requests in-process, so the request URL stays the
// real endpoint and nothing touches the network.
type handlerTransport struct{ handler http.Handler }

func (h handlerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	resp := rec.Result()
	resp.Request = req
	return resp, nil
}

type errTransport struct{ err error }

func (e errTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, e.err }

func clientFor(h http.HandlerFunc) *http.Client {
	return &http.Client{Transport: handlerTransport{h}}
}

func TestSummariseUsage(t *testing.T) {
	base := payload(t)
	tests := []struct {
		name    string
		payload map[string]any
		check   func(t *testing.T, out Summary)
	}{
		{"percentages become fractions", base, func(t *testing.T, out Summary) {
			near(t, out.Used5h, 0.36)
			near(t, out.Used7d, 0.53)
		}},
		{"reset times parse to epochs", base, func(t *testing.T, out Summary) {
			if out.Reset5h == nil || out.Reset7d == nil {
				t.Fatal("reset times missing")
			}
			if *out.Reset5h != math.Trunc(*out.Reset5h) {
				t.Fatalf("reset_5h %v is not integral", *out.Reset5h)
			}
			if want := float64(time.Date(2026, 9, 14, 16, 50, 0, 0, time.UTC).Unix()); *out.Reset5h != want {
				t.Fatalf("reset_5h = %v, want %v", *out.Reset5h, want)
			}
			if *out.Reset7d <= *out.Reset5h {
				t.Fatalf("reset_7d %v not after reset_5h %v", *out.Reset7d, *out.Reset5h)
			}
		}},
		// The headline case: plenty of overall quota, nothing left for one model.
		{"a spent model limit is reported", base, func(t *testing.T, out Summary) {
			if len(out.BlockedModels) != 1 || out.BlockedModels[0] != "Fable" {
				t.Fatalf("blocked_models = %v", out.BlockedModels)
			}
			if !out.Available {
				t.Fatal("the account itself is still usable")
			}
			if out.Severity != "critical" {
				t.Fatalf("severity = %q", out.Severity)
			}
		}},
		{"scoped limits are worst first", with(base, "limits", append(append([]any{}, base["limits"].([]any)...),
			map[string]any{"kind": "weekly_scoped", "percent": 10.0, "severity": "normal",
				"scope": map[string]any{"model": map[string]any{"display_name": "Sonnet"}}})),
			func(t *testing.T, out Summary) {
				var names []string
				for _, s := range out.Scoped {
					names = append(names, s.Model)
				}
				if strings.Join(names, ",") != "Fable,Sonnet" {
					t.Fatalf("scoped order = %v", names)
				}
				if out.Scoped[1].Reset != nil {
					t.Fatal("Sonnet has no resets_at and must report nil")
				}
			}},
		{"exhausted session marks the account limited", with(base, "five_hour",
			map[string]any{"utilization": 100.0, "resets_at": nil, "locked_reason": nil}),
			func(t *testing.T, out Summary) {
				if !out.Limited || out.Status != "rejected" {
					t.Fatalf("limited=%v status=%q", out.Limited, out.Status)
				}
			}},
		{"a locked window counts as limited even below full", with(base, "five_hour",
			map[string]any{"utilization": 12.0, "locked_reason": "spend_cap"}),
			func(t *testing.T, out Summary) {
				if !out.Limited {
					t.Fatal("expected limited")
				}
			}},
		{"missing windows do not raise", map[string]any{}, func(t *testing.T, out Summary) {
			if out.Used5h != nil {
				t.Fatalf("used_5h = %v", *out.Used5h)
			}
			if out.Scoped == nil || len(out.Scoped) != 0 {
				t.Fatalf("scoped = %#v, want empty non-nil", out.Scoped)
			}
			if out.Limited {
				t.Fatal("unexpectedly limited")
			}
			if out.Severity != "normal" || out.Status != "allowed" {
				t.Fatalf("severity=%q status=%q", out.Severity, out.Status)
			}
		}},
		{"malformed values are ignored", decode(t, `{"five_hour": {"utilization": "lots", "resets_at": "whenever"},
			"limits": [{"kind": "weekly_scoped", "percent": null,
			            "scope": {"model": {"display_name": "Fable"}}}]}`),
			func(t *testing.T, out Summary) {
				if out.Used5h != nil || out.Reset5h != nil {
					t.Fatal("malformed values must be nil")
				}
				if len(out.Scoped) != 0 {
					t.Fatalf("scoped = %v", out.Scoped)
				}
			}},
		{"scoped limit without a model name is skipped",
			decode(t, `{"limits": [{"kind": "weekly_scoped", "percent": 50, "scope": {}}]}`),
			func(t *testing.T, out Summary) {
				if len(out.Scoped) != 0 {
					t.Fatalf("scoped = %v", out.Scoped)
				}
			}},
		{"extra usage state is carried", base, func(t *testing.T, out Summary) {
			if out.ExtraUsageEnabled {
				t.Fatal("extra usage should be disabled")
			}
			if out.ExtraUsageReason != "out_of_credits" {
				t.Fatalf("extra_usage_reason = %q", out.ExtraUsageReason)
			}
		}},
		{"nil payload is an empty summary", nil, func(t *testing.T, out Summary) {
			if out.Limited || out.Used7d != nil || len(out.BlockedModels) != 0 {
				t.Fatalf("unexpected: %+v", out)
			}
		}},
		{"unknown severities rank after known ones", with(base, "limits", []any{
			map[string]any{"kind": "session", "severity": "bogus"},
			map[string]any{"kind": "weekly_all", "severity": "warning"},
		}), func(t *testing.T, out Summary) {
			if out.Severity != "warning" {
				t.Fatalf("severity = %q", out.Severity)
			}
		}},
		{"percent given as a string is still parsed", decode(t, `{"five_hour": {"utilization": "36.0"}}`),
			func(t *testing.T, out Summary) { near(t, out.Used5h, 0.36) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out := SummariseUsage(tc.payload)
			if out.Source != SourceUsage {
				t.Fatalf("source = %v", out.Source)
			}
			tc.check(t, out)
		})
	}
}

func TestParseISOMatchesPython(t *testing.T) {
	utc := time.Date(2026, 9, 14, 16, 50, 0, 0, time.UTC)
	tests := []struct {
		stamp string
		want  time.Time
	}{
		{"2026-09-14T16:50:00+00:00", utc},
		{"2026-09-14T16:50:00Z", utc},
		{"2026-09-14T16:50:00.250+00:00", utc.Add(250 * time.Millisecond)},
		{"2026-09-14T18:50:00+02:00", utc},
		{"2026-09-14 16:50:00+0000", utc},
		// Naive stamps are local time, as datetime.timestamp() treats them.
		{"2026-09-14T16:50:00", time.Date(2026, 9, 14, 16, 50, 0, 0, time.Local)},
		{"2026-09-14", time.Date(2026, 9, 14, 0, 0, 0, 0, time.Local)},
	}
	for _, tc := range tests {
		got, ok := parseISO(tc.stamp)
		if !ok || !got.Equal(tc.want) {
			t.Errorf("parseISO(%q) = %v, %v; want %v", tc.stamp, got, ok, tc.want)
		}
	}
	for _, bad := range []string{"", "whenever", "14/09/2026", "2026-09-14T16:50:00+00:00Z"} {
		if _, ok := parseISO(bad); ok {
			t.Errorf("parseISO(%q) unexpectedly parsed", bad)
		}
	}
}

func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	ptr := func(f float64) *float64 { return &f }
	tests := []struct {
		header string
		want   *float64
	}{
		{"", nil},
		{"30", ptr(30)},
		{" 12.5 ", ptr(12.5)},
		{"-5", ptr(0)},
		{"nan", ptr(0)},
		{"garbage", nil},
		{"Wed, 16 Sep 2026 12:02:00 GMT", ptr(120)},
		{"Wed, 16 Sep 2026 12:02:00 +0000", ptr(120)},
		{"Wed, 16 Sep 2026 14:02:00 +0200", ptr(120)},
		{"Wed, 16 Sep 2026 12:02:00 -0000", ptr(120)},
		{"Wed, 16 Sep 2026 11:00:00 GMT", ptr(0)},
	}
	for _, tc := range tests {
		got := retryAfter(tc.header, now)
		switch {
		case tc.want == nil && got != nil:
			t.Errorf("retryAfter(%q) = %v, want nil", tc.header, *got)
		case tc.want != nil && got == nil:
			t.Errorf("retryAfter(%q) = nil, want %v", tc.header, *tc.want)
		case tc.want != nil && *got != *tc.want:
			t.Errorf("retryAfter(%q) = %v, want %v", tc.header, *got, *tc.want)
		}
	}
}

func TestFetch(t *testing.T) {
	ctx := context.Background()
	t.Run("success sends the OAuth headers and decodes the body", func(t *testing.T) {
		var got *http.Request
		client := clientFor(func(w http.ResponseWriter, r *http.Request) {
			got = r
			_, _ = w.Write([]byte(usagePayload))
		})
		out, err := FetchWith(ctx, client, "tok-123")
		if err != nil {
			t.Fatal(err)
		}
		if got.Method != http.MethodGet || got.URL.String() != UsageEndpoint {
			t.Fatalf("request %s %s", got.Method, got.URL)
		}
		for name, want := range map[string]string{
			"Authorization": "Bearer tok-123", "anthropic-beta": OAuthBeta, "Accept": "application/json"} {
			if v := got.Header.Get(name); v != want {
				t.Errorf("header %s = %q, want %q", name, v, want)
			}
		}
		if _, ok := out["five_hour"]; !ok {
			t.Fatalf("payload not decoded: %v", out)
		}
	})
	t.Run("401 and 403 reject the token", func(t *testing.T) {
		for _, code := range []int{401, 403} {
			client := clientFor(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "5")
				w.WriteHeader(code)
			})
			_, err := FetchWith(ctx, client, "tok")
			var ue *UsageError
			if !errors.As(err, &ue) || ue.Error() != "the account rejected this token" || ue.RetryAfter != nil {
				t.Fatalf("%d: err = %#v", code, err)
			}
		}
	})
	t.Run("429 carries Retry-After seconds", func(t *testing.T) {
		client := clientFor(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(429)
		})
		_, err := FetchWith(ctx, client, "tok")
		var ue *UsageError
		if !errors.As(err, &ue) || ue.Error() != "usage endpoint returned 429" {
			t.Fatalf("err = %v", err)
		}
		near(t, ue.RetryAfter, 30)
	})
	t.Run("503 carries an RFC 2822 Retry-After date", func(t *testing.T) {
		client := clientFor(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", time.Now().Add(10*time.Minute).UTC().Format(http.TimeFormat))
			w.WriteHeader(503)
		})
		_, err := FetchWith(ctx, client, "tok")
		var ue *UsageError
		if !errors.As(err, &ue) || ue.Error() != "usage endpoint returned 503" || ue.RetryAfter == nil {
			t.Fatalf("err = %#v", err)
		}
		if *ue.RetryAfter < 590 || *ue.RetryAfter > 601 {
			t.Fatalf("retry_after = %v, want about 600", *ue.RetryAfter)
		}
	})
	t.Run("500 without Retry-After has no wait", func(t *testing.T) {
		client := clientFor(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) })
		_, err := FetchWith(ctx, client, "tok")
		var ue *UsageError
		if !errors.As(err, &ue) || ue.Error() != "usage endpoint returned 500" || ue.RetryAfter != nil {
			t.Fatalf("err = %#v", err)
		}
	})
	t.Run("garbage Retry-After is ignored", func(t *testing.T) {
		client := clientFor(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "soon")
			w.WriteHeader(429)
		})
		_, err := FetchWith(ctx, client, "tok")
		var ue *UsageError
		if !errors.As(err, &ue) || ue.RetryAfter != nil {
			t.Fatalf("err = %#v", err)
		}
	})
	t.Run("undecodable body is a UsageError", func(t *testing.T) {
		client := clientFor(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("<html>")) })
		_, err := FetchWith(ctx, client, "tok")
		var ue *UsageError
		if !errors.As(err, &ue) || ue.Error() == "" {
			t.Fatalf("err = %#v", err)
		}
	})
	t.Run("network failure is a UsageError", func(t *testing.T) {
		client := &http.Client{Transport: errTransport{errors.New("connection refused")}}
		_, err := FetchWith(ctx, client, "tok")
		var ue *UsageError
		if !errors.As(err, &ue) || !strings.Contains(ue.Error(), "connection refused") {
			t.Fatalf("err = %#v", err)
		}
	})
	t.Run("ForTokenWith summarises", func(t *testing.T) {
		client := clientFor(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(usagePayload)) })
		out, err := ForTokenWith(ctx, client, "tok")
		if err != nil {
			t.Fatal(err)
		}
		near(t, out.Used7d, 0.53)
		if len(out.BlockedModels) != 1 {
			t.Fatalf("blocked = %v", out.BlockedModels)
		}
	})
	t.Run("ForTokenWith passes errors through", func(t *testing.T) {
		client := clientFor(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) })
		if _, err := ForTokenWith(ctx, client, "tok"); err == nil {
			t.Fatal("expected an error")
		}
	})
}

func TestSummaryJSONMatchesPythonDicts(t *testing.T) {
	t.Run("usage key set and order", func(t *testing.T) {
		raw, err := json.Marshal(SummariseUsage(payload(t)))
		if err != nil {
			t.Fatal(err)
		}
		want := `{"status":"allowed","available":true,"limited":false,"severity":"critical",` +
			`"used_5h":0.36,"reset_5h":1789404600,"used_7d":0.53,"reset_7d":1789916400,` +
			`"scoped":[{"model":"Fable","used":1,"severity":"critical","reset":1789916400,"exhausted":true}],` +
			`"blocked_models":["Fable"],"extra_usage_enabled":false,"extra_usage_reason":"out_of_credits","plan":null}`
		if string(raw) != want {
			t.Fatalf("got  %s\nwant %s", raw, want)
		}
	})
	t.Run("empty usage emits every key with nulls and empty lists", func(t *testing.T) {
		raw, err := json.Marshal(SummariseUsage(nil))
		if err != nil {
			t.Fatal(err)
		}
		want := `{"status":"allowed","available":true,"limited":false,"severity":"normal",` +
			`"used_5h":null,"reset_5h":null,"used_7d":null,"reset_7d":null,"scoped":[],"blocked_models":[],` +
			`"extra_usage_enabled":false,"extra_usage_reason":null,"plan":null}`
		if string(raw) != want {
			t.Fatalf("got  %s\nwant %s", raw, want)
		}
	})
	t.Run("probe key set and order", func(t *testing.T) {
		raw, err := json.Marshal(SummariseHeaders(nil))
		if err != nil {
			t.Fatal(err)
		}
		want := `{"status":"unknown","status_5h":"unknown","status_7d":"unknown","available":false,"limited":true,` +
			`"used_5h":null,"reset_5h":null,"used_7d":null,"reset_7d":null,"overage_status":null,` +
			`"overage_reason":null,"binding":null,"fallback":null,"fallback_at":null}`
		if string(raw) != want {
			t.Fatalf("got  %s\nwant %s", raw, want)
		}
	})
	t.Run("probe fallback carries the collector's extra keys", func(t *testing.T) {
		s := SummariseHeaders(nil)
		s.Scoped, s.BlockedModels, s.Degraded = []ScopedLimit{}, []string{}, "per-model limits unavailable"
		raw, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(string(raw), `"fallback_at":null,"scoped":[],"blocked_models":[],"degraded":"per-model limits unavailable"}`) {
			t.Fatalf("got %s", raw)
		}
	})
	t.Run("round trip keeps values and infers the source", func(t *testing.T) {
		for _, s := range []Summary{SummariseUsage(payload(t)), SummariseHeaders(allowedHeaders())} {
			raw, err := json.Marshal(s)
			if err != nil {
				t.Fatal(err)
			}
			var back Summary
			if err := json.Unmarshal(raw, &back); err != nil {
				t.Fatal(err)
			}
			again, _ := json.Marshal(back)
			if string(again) != string(raw) || back.Source != s.Source {
				t.Fatalf("round trip changed:\n%s\n%s", raw, again)
			}
		}
	})
}
