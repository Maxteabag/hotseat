package collect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Maxteabag/hotseat/internal/claude"
	"github.com/Maxteabag/hotseat/internal/quota"
)

// No listening server or provider call: exercise the real typed HTTP boundary.
type authResponse struct {
	status int
	err    error
}

func (r authResponse) RoundTrip(req *http.Request) (*http.Response, error) {
	if r.err != nil {
		return nil, r.err
	}
	return &http.Response{StatusCode: r.status, Header: make(http.Header),
		Body: io.NopCloser(strings.NewReader(`{"five_hour":{"utilization":25}}`)), Request: req}, nil
}

func nativeUsageFailure(ctx context.Context, status int, cause error) error {
	_, err := quota.FetchWith(ctx, &http.Client{Transport: authResponse{status, cause}}, "SYNTHETIC-NEVER-EXPORT")
	return err
}

func accountJSON(t *testing.T, c *Collector) ([]byte, map[string]any) {
	t.Helper()
	snapshot, err := c.Build(context.Background())
	if err != nil || len(snapshot.Accounts) != 1 {
		t.Fatalf("fixture build err=%v rows=%d", err, len(snapshot.Accounts))
	}
	raw, err := json.Marshal(snapshot.Accounts[0])
	if err != nil {
		t.Fatal(err)
	}
	var row map[string]any
	if err := json.Unmarshal(raw, &row); err != nil {
		t.Fatal(err)
	}
	return raw, row
}

func TestStructuredAuthMetadataUsesNativeCauseAndPreservesFallbackPolicy(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		cause      error
		misleading bool
		wrapped    bool
		fallbackOK bool
		want       any
		fallbacks  int
	}{
		{name: "positive credential rejection", status: 401, want: "auth_error", fallbacks: 1},
		{name: "wrapped credential rejection", status: 401, wrapped: true, want: "auth_error", fallbacks: 1},
		{name: "generic forbidden is not relogin evidence", status: 403, want: "provider_error", fallbacks: 1},
		{name: "quota refused", status: 429, want: "provider_error", fallbacks: 1},
		{name: "provider outage", status: 503, want: "provider_error", fallbacks: 1},
		{name: "missing endpoint", status: 404, want: "provider_error", fallbacks: 1},
		{name: "timeout retains native cause", cause: context.DeadlineExceeded, want: "timeout", fallbacks: 1},
		{name: "cancel is not auth", cause: context.Canceled, want: "provider_error", fallbacks: 1},
		{name: "auth words are not typed evidence", misleading: true, want: "provider_error"},
		{name: "successful fallback clears earlier rejection", status: 401, fallbackOK: true, want: nil, fallbacks: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Unix(1700000000, 0)
			account := testAccount(now)
			account.OrgUUID = "fixture-org-A"
			c := newCollector(t, &fakeBackend{accounts: []claude.Account{account}})
			c.Now = func() float64 { return float64(now.Unix()) }
			usageCalls, headerCalls := 0, 0
			var original error
			c.Usage = func(ctx context.Context, _ string) (quota.Summary, error) {
				usageCalls++
				if tc.misleading {
					original = errors.New("401 invalid_grant expired login PRIVATE_SENTINEL")
				} else {
					original = nativeUsageFailure(ctx, tc.status, tc.cause)
				}
				if tc.wrapped {
					original = fmt.Errorf("fixture wrapper: %w", original)
				}
				return quota.Summary{}, original
			}
			c.HeaderProbe = func(context.Context, string) (quota.Headers, error) {
				headerCalls++
				if tc.fallbackOK {
					return quota.Headers{"anthropic-ratelimit-unified-status": "allowed"}, nil
				}
				return nil, &quota.ProbeError{Message: "fallback unavailable"}
			}
			_, row := accountJSON(t, c)
			code, exists := row["error_code"]
			if !exists || code != tc.want || row["org_uuid"] != account.OrgUUID {
				t.Fatalf("new metadata code=%v exists=%v org=%v", code, exists, row["org_uuid"])
			}
			if usageCalls != 1 || headerCalls != tc.fallbacks {
				t.Fatalf("legacy request counts changed: usage=%d header=%d", usageCalls, headerCalls)
			}
			if tc.fallbackOK {
				if row["error"] != nil || row["usage"] == nil {
					t.Fatal("successful fallback lost its legacy outcome")
				}
			} else if row["error"] != original.Error() || row["usage"] != nil {
				t.Fatal("selected primary error or absent quota changed")
			}
		})
	}
}

func TestStructuredAuthMetadataIsOnlyTwoAdditionsToFrozenLegacyAccountBytes(t *testing.T) {
	now := time.Unix(1700000000, 0)
	account := testAccount(now)
	account.OrgUUID = "fixture-org-A"
	c := newCollector(t, &fakeBackend{accounts: []claude.Account{account}})
	c.Now = func() float64 { return float64(now.Unix()) }
	c.Usage = func(ctx context.Context, _ string) (quota.Summary, error) {
		return quota.Summary{}, nativeUsageFailure(ctx, 401, nil)
	}
	raw, _ := accountJSON(t, c)
	// Explicit pre-feature wire golden: values, nulls and key order are contractual.
	legacy := `{"alias":"work","email":"user@example.com","org":"Example Org","plan":"team","plan_label":"Team","rate_limit_tier":null,"is_active":true,"access_expires_at":1700014400000,"refresh_expires_at":1701728000000,"error":"the account rejected this token","usage":null,"access_hours_left":4,"signin_days_left":20,"signin_due_soon":false}`
	want := strings.TrimSuffix(legacy, "}") + `,"org_uuid":"fixture-org-A","error_code":"auth_error"}`
	if string(raw) != want {
		t.Fatalf("additive wire golden differs: got %s", raw)
	}
}

func TestStructuredAuthMetadataKeepsSameNameAndEmailOrganizationsDistinct(t *testing.T) {
	now := time.Unix(1700000000, 0)
	first, second := testAccount(now), testAccount(now)
	first.Alias, first.OrgUUID = "first", "fixture-org-A"
	second.Alias, second.OrgUUID = "second", "fixture-org-B"
	second.IsActive = false
	c := newCollector(t, &fakeBackend{accounts: []claude.Account{first, second}})
	c.Now = func() float64 { return float64(now.Unix()) }
	c.Usage = func(context.Context, string) (quota.Summary, error) { return quota.Summary{Status: "allowed"}, nil }
	snapshot, err := c.Build(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(snapshot.Accounts)
	var rows []map[string]any
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	if rows[0]["org_uuid"] != first.OrgUUID || rows[1]["org_uuid"] != second.OrgUUID {
		t.Fatal("org UUID came from display name or another account")
	}
	if rows[0]["email"] != rows[1]["email"] || rows[0]["org"] != rows[1]["org"] {
		t.Fatal("fixture must share the old display identity")
	}
}

func TestStructuredAuthMetadataDoesNotPromoteGenericRefreshFailureOrProbeOldToken(t *testing.T) {
	now := time.Unix(1700000000, 0)
	account := testAccount(now)
	account.AccessExpiresAt = ms(now.Add(-time.Hour))
	c := newCollector(t, &fakeBackend{accounts: []claude.Account{account}})
	c.Now = func() float64 { return float64(now.Unix()) }
	c.AutoRefresh = func(claude.Backend, *claude.Account) error {
		return &claude.RefreshError{Msg: "invalid_grant 401 revoked login"}
	}
	c.Usage = func(context.Context, string) (quota.Summary, error) {
		t.Fatal("old token was probed")
		return quota.Summary{}, nil
	}
	c.HeaderProbe = func(context.Context, string) (quota.Headers, error) { t.Fatal("old token was probed"); return nil, nil }
	_, row := accountJSON(t, c)
	if row["error_code"] != "provider_error" || row["usage"] != nil {
		t.Fatal("generic refresh failure became authentication evidence or usable quota")
	}
}

func TestStructuredAuthMetadataPreservesTypedRefreshCauseWithoutProbingOldToken(t *testing.T) {
	now := time.Unix(1700000000, 0)
	account := testAccount(now)
	account.AccessExpiresAt = ms(now.Add(-time.Hour))
	c := newCollector(t, &fakeBackend{accounts: []claude.Account{account}})
	c.Now = func() float64 { return float64(now.Unix()) }
	c.AutoRefresh = func(claude.Backend, *claude.Account) error {
		// Real typed boundary over an in-process response, not an Error() string.
		cause := nativeUsageFailure(context.Background(), 401, nil)
		return &claude.RefreshError{Msg: "fixture refresh rejection", Err: fmt.Errorf("cause: %w", cause)}
	}
	c.Usage = func(context.Context, string) (quota.Summary, error) {
		t.Fatal("superseded access token was probed")
		return quota.Summary{}, nil
	}
	c.HeaderProbe = func(context.Context, string) (quota.Headers, error) {
		t.Fatal("superseded access token was probed")
		return nil, nil
	}
	_, row := accountJSON(t, c)
	if row["error_code"] != "auth_error" || row["usage"] != nil ||
		row["error"] != "Access token expired; refresh failed: fixture refresh rejection" {
		t.Fatal("typed cause or legacy refresh outcome changed")
	}
}

func TestStructuredAuthMetadataDoesNotAuthorizeAReboundRefreshIdentity(t *testing.T) {
	now := time.Unix(1700000000, 0)
	account := testAccount(now)
	account.AccessExpiresAt = ms(now.Add(-time.Hour))
	c := newCollector(t, &fakeBackend{accounts: []claude.Account{account}})
	c.Now = func() float64 { return float64(now.Unix()) }
	c.AutoRefresh = func(_ claude.Backend, account *claude.Account) error {
		account.OrgUUID = "rebound-org"
		return &claude.RefreshError{Msg: "fixture rejection", Err: nativeUsageFailure(context.Background(), 401, nil)}
	}
	c.Usage = func(context.Context, string) (quota.Summary, error) {
		t.Fatal("unexpected probe")
		return quota.Summary{}, nil
	}
	_, row := accountJSON(t, c)
	if row["error_code"] != "provider_error" || row["usage"] != nil {
		t.Fatal("a different identity inherited authentication evidence")
	}
}
