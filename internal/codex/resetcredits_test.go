package codex

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// rewriteTransport sends every request to the test server, whatever host the
// code asked for, so the real endpoint constant stays under test.
type rewriteTransport struct{ target string }

func (r rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL.Scheme = "http"
	clone.URL.Host = strings.TrimPrefix(r.target, "http://")
	return http.DefaultTransport.RoundTrip(clone)
}

type creditsFixture struct {
	path     string
	client   *http.Client
	mu       sync.Mutex
	requests []*http.Request
	body     string
	status   int
	calls    atomic.Int32
}

func newCreditsFixture(t *testing.T) *creditsFixture {
	t.Helper()
	f := &creditsFixture{path: filepath.Join(t.TempDir(), "auth.json"), body: `{"available_count":2}`, status: 200}
	raw, _ := json.Marshal(map[string]any{"tokens": map[string]any{"access_token": "SENSITIVE", "account_id": "workspace"}})
	if err := os.WriteFile(f.path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		f.mu.Lock()
		f.requests = append(f.requests, r.Clone(r.Context()))
		status, body := f.status, f.body
		f.mu.Unlock()
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	f.client = &http.Client{Transport: rewriteTransport{server.URL}}
	return f
}

func TestGetBalanceNeverRedeemsOrExportsTokens(t *testing.T) {
	f := newCreditsFixture(t)
	row := ReadBalance(context.Background(), f.client, f.path)
	if row.AvailableCount == nil || *row.AvailableCount != 2 || row.Error != nil {
		t.Fatalf("row = %+v", row)
	}
	if len(f.requests) != 1 {
		t.Fatalf("requests = %d", len(f.requests))
	}
	request := f.requests[0]
	if request.Method != http.MethodGet || request.ContentLength != 0 {
		t.Fatalf("method = %s length = %d", request.Method, request.ContentLength)
	}
	if request.URL.Path != "/backend-api/wham/rate-limit-reset-credits" {
		t.Fatalf("path = %s", request.URL.Path)
	}
	for key, want := range map[string]string{
		"Authorization": "Bearer SENSITIVE", "Chatgpt-Account-Id": "workspace", "Accept": "application/json",
		"User-Agent": "Codex Desktop", "Originator": "Codex Desktop", "Oai-Product-Sku": "CODEX",
	} {
		if got := request.Header.Get(key); got != want {
			t.Errorf("header %s = %q, want %q", key, got, want)
		}
	}
	raw, _ := json.Marshal(row)
	if strings.Contains(string(raw), "SENSITIVE") {
		t.Fatalf("token leaked: %s", raw)
	}
	if row.CheckedAt == 0 {
		t.Fatal("checked_at missing")
	}
}

func TestZeroIsASuccessfulBalance(t *testing.T) {
	f := newCreditsFixture(t)
	f.body = `{"available_count":0}`
	row := ReadBalance(context.Background(), f.client, f.path)
	if row.AvailableCount == nil || *row.AvailableCount != 0 || row.Error != nil {
		t.Fatalf("row = %+v", row)
	}
}

func TestHTTPFailureIsUnknownNotZero(t *testing.T) {
	f := newCreditsFixture(t)
	f.status = 401
	f.body = "unauthorized"
	row := ReadBalance(context.Background(), f.client, f.path)
	if row.AvailableCount != nil || row.Error == nil || *row.Error != "HTTP 401" {
		t.Fatalf("row = %+v", row)
	}
}

func TestBadPayloadsAreUnknown(t *testing.T) {
	f := newCreditsFixture(t)
	for _, value := range []string{`{}`, `[]`, `{"available_count":true}`, `{"available_count":-1}`,
		`{"available_count":"2"}`, `{"available_count":2.0}`, `not json`} {
		f.body = value
		row := ReadBalance(context.Background(), f.client, f.path)
		if row.AvailableCount != nil || row.Error == nil || *row.Error == "" {
			t.Errorf("%s: row = %+v", value, row)
		}
	}
}

func TestIncompleteCredentialsNeverQueryTheEndpoint(t *testing.T) {
	f := newCreditsFixture(t)
	if err := os.WriteFile(f.path, []byte(`{"tokens":{"access_token":"x"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	row := ReadBalance(context.Background(), f.client, f.path)
	if row.Error == nil || *row.Error != "No complete OAuth credentials" || f.calls.Load() != 0 {
		t.Fatalf("row = %+v calls = %d", row, f.calls.Load())
	}
	row = ReadBalance(context.Background(), f.client, filepath.Join(t.TempDir(), "missing.json"))
	if row.Error == nil || row.AvailableCount != nil {
		t.Fatalf("row = %+v", row)
	}
}

func TestUnknownAliasNeverQueriesEndpoint(t *testing.T) {
	f := newCreditsFixture(t)
	c := New(t.TempDir())
	c.HTTP = f.client
	_, err := c.Balances(context.Background(), []string{"../other"})
	var codexErr *Error
	if !errors.As(err, &codexErr) || codexErr.Msg != "Unknown Codex profiles: ../other" {
		t.Fatalf("err = %v", err)
	}
	if f.calls.Load() != 0 {
		t.Fatal("endpoint queried for an unknown alias")
	}
}

func TestBalancesAreReadPerAccountInOrder(t *testing.T) {
	f := newCreditsFixture(t)
	home := t.TempDir()
	c := New(home)
	c.HTTP = f.client
	for i, name := range []string{"alpha", "beta", "gamma", "delta"} {
		dir := filepath.Join(home, "profiles", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(map[string]any{"tokens": map[string]any{
			"id_token": idToken(name+"@example.com", "pro", name, future), "access_token": "tok",
			"account_id": name}})
		if i == 3 {
			raw = []byte(`{"tokens":{"id_token":"","access_token":""}}`)
		}
		if err := os.WriteFile(filepath.Join(dir, "auth.json"), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := c.Balances(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 {
		t.Fatalf("rows = %+v", rows)
	}
	for i, want := range []string{"alpha", "beta", "delta", "gamma"} {
		if rows[i].Alias != want {
			t.Fatalf("rows[%d].alias = %q", i, rows[i].Alias)
		}
	}
	if *rows[0].AvailableCount != 2 || *rows[0].Email != "alpha@example.com" {
		t.Fatalf("row = %+v", rows[0])
	}
	if rows[2].Error == nil || rows[2].Email != nil {
		t.Fatalf("delta must fail without credentials: %+v", rows[2])
	}
	if f.calls.Load() != 3 {
		t.Fatalf("calls = %d", f.calls.Load())
	}
	only, err := c.Balances(context.Background(), []string{"beta"})
	if err != nil || len(only) != 1 || only[0].Alias != "beta" {
		t.Fatalf("only = %+v err = %v", only, err)
	}
	raw, _ := json.Marshal(only[0])
	var shape map[string]any
	_ = json.Unmarshal(raw, &shape)
	for _, key := range []string{"alias", "email", "available_count", "checked_at", "error"} {
		if _, ok := shape[key]; !ok {
			t.Errorf("balance lacks %q: %s", key, raw)
		}
	}
}

func TestFetchResetCreditsMissingFileOrToken(t *testing.T) {
	f := newCreditsFixture(t)
	want := `{"available_count":0,"credits":[]}`
	got, _ := json.Marshal(FetchResetCredits(context.Background(), f.client, "/nonexistent/auth.json"))
	if string(got) != want {
		t.Fatalf("missing file = %s", got)
	}
	bad := filepath.Join(t.TempDir(), "bad_auth.json")
	if err := os.WriteFile(bad, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, _ = json.Marshal(FetchResetCredits(context.Background(), f.client, bad))
	if string(got) != want {
		t.Fatalf("no token = %s", got)
	}
	if f.calls.Load() != 0 {
		t.Fatal("endpoint queried without a token")
	}
}

func TestFetchResetCreditsReturnsThePayloadAndSwallowsFailures(t *testing.T) {
	f := newCreditsFixture(t)
	f.body = `{"available_count":1,"credits":[{"status":"available","title":"Full reset"}],"extra":true}`
	got := FetchResetCredits(context.Background(), f.client, f.path)
	if got["available_count"] != 1.0 || got["extra"] != true {
		t.Fatalf("payload = %v", got)
	}
	request := f.requests[0]
	if request.Header.Get("Chatgpt-Account-Id") != "workspace" || request.Header.Get("Authorization") != "Bearer SENSITIVE" {
		t.Fatalf("headers = %v", request.Header)
	}
	f.status = 500
	if raw, _ := json.Marshal(FetchResetCredits(context.Background(), f.client, f.path)); string(raw) != `{"available_count":0,"credits":[]}` {
		t.Fatalf("failure = %s", raw)
	}
	f.status, f.body = 200, `[]`
	if raw, _ := json.Marshal(FetchResetCredits(context.Background(), f.client, f.path)); string(raw) != `{"available_count":0,"credits":[]}` {
		t.Fatalf("non-object = %s", raw)
	}
}
