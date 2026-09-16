package quota

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// Header parsing. Fixtures mirror the shape the API actually returns.

func allowedHeaders() Headers {
	return Headers{
		"anthropic-ratelimit-unified-status":         "allowed",
		"anthropic-ratelimit-unified-5h-status":      "allowed",
		"anthropic-ratelimit-unified-5h-utilization": "0.36",
		"anthropic-ratelimit-unified-5h-reset":       "1800000000",
		"anthropic-ratelimit-unified-7d-status":      "allowed",
		"anthropic-ratelimit-unified-7d-utilization": "0.53",
		"anthropic-ratelimit-unified-7d-reset":       "1800600000",
	}
}

func withHeaders(base Headers, extra Headers) Headers {
	out := make(Headers, len(base)+len(extra))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func TestSummariseHeaders(t *testing.T) {
	allowed := allowedHeaders()
	tests := []struct {
		name    string
		headers Headers
		check   func(t *testing.T, out Summary)
	}{
		{"utilisation is read as a fraction", allowed, func(t *testing.T, out Summary) {
			near(t, out.Used5h, 0.36)
			near(t, out.Used7d, 0.53)
			near(t, out.Reset5h, 1800000000)
			near(t, out.Reset7d, 1800600000)
		}},
		{"allowed account is not limited", allowed, func(t *testing.T, out Summary) {
			if !out.Available || out.Limited {
				t.Fatalf("available=%v limited=%v", out.Available, out.Limited)
			}
		}},
		{"five hour rejection marks the account limited", withHeaders(allowed, Headers{
			"anthropic-ratelimit-unified-status":         "rejected",
			"anthropic-ratelimit-unified-5h-status":      "rejected",
			"anthropic-ratelimit-unified-5h-utilization": "1.06",
		}), func(t *testing.T, out Summary) {
			if !out.Limited {
				t.Fatal("expected limited")
			}
			if out.Used5h == nil || *out.Used5h <= 1.0 {
				t.Fatal("over-quota must not be clamped to 100%")
			}
		}},
		// A rejected 5-hour window can still serve requests from purchased usage.
		{"extra usage keeps an account usable", withHeaders(allowed, Headers{
			"anthropic-ratelimit-unified-status": "rejected",
		}), func(t *testing.T, out Summary) {
			if !out.Available {
				t.Fatal("expected available")
			}
		}},
		{"missing headers degrade to unknown", Headers{}, func(t *testing.T, out Summary) {
			if out.Used5h != nil || out.Reset5h != nil {
				t.Fatal("expected nil readings")
			}
			if out.Status != "unknown" || out.Status5h != "unknown" || out.Status7d != "unknown" {
				t.Fatalf("status=%q 5h=%q 7d=%q", out.Status, out.Status5h, out.Status7d)
			}
		}},
		{"garbage values do not raise", Headers{
			"anthropic-ratelimit-unified-5h-utilization": "not-a-number",
			"anthropic-ratelimit-unified-5h-reset":       "soon",
		}, func(t *testing.T, out Summary) {
			if out.Used5h != nil || out.Reset5h != nil {
				t.Fatal("expected nil readings")
			}
		}},
		{"reset falls back to the unified header", Headers{
			"anthropic-ratelimit-unified-reset": "1800000001",
		}, func(t *testing.T, out Summary) { near(t, out.Reset5h, 1800000001) }},
		{"negative utilisation clamps to zero", Headers{
			"anthropic-ratelimit-unified-5h-utilization": "-0.5",
		}, func(t *testing.T, out Summary) { near(t, out.Used5h, 0) }},
		{"overage, binding and fallback are carried", withHeaders(allowed, Headers{
			"anthropic-ratelimit-unified-overage-status":          "rejected",
			"anthropic-ratelimit-unified-overage-disabled-reason": "out_of_credits",
			"anthropic-ratelimit-unified-representative-claim":    "five_hour",
			"anthropic-ratelimit-unified-fallback":                "available",
			"anthropic-ratelimit-unified-fallback-percentage":     "0.5",
		}), func(t *testing.T, out Summary) {
			if out.OverageStatus != "rejected" || out.OverageReason != "out_of_credits" ||
				out.Binding != "five_hour" || out.Fallback != "available" {
				t.Fatalf("unexpected: %+v", out)
			}
			near(t, out.FallbackAt, 0.5)
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out := SummariseHeaders(tc.headers)
			if out.Source != SourceProbe {
				t.Fatalf("source = %v", out.Source)
			}
			tc.check(t, out)
		})
	}
}

type probeCall struct {
	model string
	body  string
	req   *http.Request
}

// probeServer answers each model with the given status, recording the calls.
func probeServer(t *testing.T, status map[string]int, calls *[]probeCall) *http.Client {
	t.Helper()
	return clientFor(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body := string(raw)
		model := ""
		for _, m := range ProbeModels {
			if strings.Contains(body, `"`+m+`"`) {
				model = m
			}
		}
		*calls = append(*calls, probeCall{model, body, r})
		for name, value := range allowedHeaders() {
			w.Header().Set(name, value)
		}
		w.Header().Set("X-Model", model)
		w.WriteHeader(status[model])
	})
}

func TestProbe(t *testing.T) {
	ctx := context.Background()
	t.Run("success returns lower-cased headers after one request", func(t *testing.T) {
		var calls []probeCall
		client := probeServer(t, map[string]int{ProbeModels[0]: 200, ProbeModels[1]: 200}, &calls)
		headers, err := ProbeWith(ctx, client, "tok-123")
		if err != nil {
			t.Fatal(err)
		}
		if len(calls) != 1 || calls[0].model != ProbeModels[0] {
			t.Fatalf("calls = %+v", calls)
		}
		want := `{"model": "claude-haiku-4-5-20251001", "max_tokens": 1, "messages": [{"role": "user", "content": "hi"}]}`
		if calls[0].body != want {
			t.Fatalf("body = %s", calls[0].body)
		}
		req := calls[0].req
		if req.Method != http.MethodPost || req.URL.String() != APIURL {
			t.Fatalf("request %s %s", req.Method, req.URL)
		}
		for name, want := range map[string]string{
			"Authorization": "Bearer tok-123", "anthropic-version": AnthropicVersion,
			"anthropic-beta": OAuthBeta, "Content-Type": "application/json", "User-Agent": UserAgent} {
			if v := req.Header.Get(name); v != want {
				t.Errorf("header %s = %q, want %q", name, v, want)
			}
		}
		if headers["anthropic-ratelimit-unified-5h-utilization"] != "0.36" || headers["x-model"] != ProbeModels[0] {
			t.Fatalf("headers = %v", headers)
		}
		for name := range headers {
			if name != strings.ToLower(name) {
				t.Fatalf("header %q not lower-cased", name)
			}
		}
	})
	t.Run("a retired model falls through to the next", func(t *testing.T) {
		var calls []probeCall
		client := probeServer(t, map[string]int{ProbeModels[0]: 404, ProbeModels[1]: 429}, &calls)
		headers, err := ProbeWith(ctx, client, "tok")
		if err != nil {
			t.Fatal(err)
		}
		if len(calls) != 2 || calls[1].model != ProbeModels[1] {
			t.Fatalf("calls = %+v", calls)
		}
		// A 429 is a perfectly good answer.
		if headers["x-model"] != ProbeModels[1] {
			t.Fatalf("headers = %v", headers)
		}
	})
	t.Run("a server error still yields its headers", func(t *testing.T) {
		var calls []probeCall
		client := probeServer(t, map[string]int{ProbeModels[0]: 500}, &calls)
		headers, err := ProbeWith(ctx, client, "tok")
		if err != nil || headers["anthropic-ratelimit-unified-status"] != "allowed" || len(calls) != 1 {
			t.Fatalf("headers=%v err=%v calls=%d", headers, err, len(calls))
		}
	})
	t.Run("401 and 403 reject the token", func(t *testing.T) {
		for _, code := range []int{401, 403} {
			var calls []probeCall
			client := probeServer(t, map[string]int{ProbeModels[0]: code}, &calls)
			_, err := ProbeWith(ctx, client, "tok")
			var pe *ProbeError
			if !errors.As(err, &pe) || pe.Error() != "the account rejected this token" || len(calls) != 1 {
				t.Fatalf("%d: err=%v calls=%d", code, err, len(calls))
			}
		}
	})
	t.Run("every model retired names the last one", func(t *testing.T) {
		var calls []probeCall
		client := probeServer(t, map[string]int{ProbeModels[0]: 404, ProbeModels[1]: 404}, &calls)
		_, err := ProbeWith(ctx, client, "tok")
		var pe *ProbeError
		if !errors.As(err, &pe) || pe.Error() != "model claude-3-5-haiku-20241022 unavailable" {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("network failure is a ProbeError with the transport message", func(t *testing.T) {
		client := &http.Client{Transport: errTransport{errors.New("connection refused")}}
		_, err := ProbeWith(ctx, client, "tok")
		var pe *ProbeError
		if !errors.As(err, &pe) || !strings.Contains(pe.Error(), "connection refused") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("a 404 followed by a network failure reports the failure", func(t *testing.T) {
		n := 0
		client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			n++
			if n == 1 {
				return handlerTransport{http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(404)
				})}.RoundTrip(r)
			}
			return nil, errors.New("timed out")
		})}
		_, err := ProbeWith(ctx, client, "tok")
		if err == nil || !strings.Contains(err.Error(), "timed out") || n != 2 {
			t.Fatalf("err=%v n=%d", err, n)
		}
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
