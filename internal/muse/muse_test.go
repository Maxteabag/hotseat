package muse

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const authFixture = `{"schema_version":1,"providers":{"meta":{"access_token":"SECRET-ACCESS","api_key":"SECRET-KEY",
"api_base_url":"https://api.meta.ai/v1","mechanism":"oauth","obtained_via":"device_code","subs_active":true,
"user_email":"someone@example.com","user_full_name":"Some One"}}}`

const usageFixture = `{"allowance":{"asOfMs":1792909547527,"source":"mint","tier":"Muse Code Power Usage",
"weekly":{"resetsAtMs":1792972800000,"usedPercent":12.5},"window":{"resetsAtMs":1792927534000,"usedPercent":40}},
"hint":null,"reason":null,"schemaVersion":1}`

func writeAuth(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func runner(stdout, stderr string, code int, calls *[][]string) Runner {
	return func(_ context.Context, argv []string) ([]byte, []byte, int, error) {
		if calls != nil {
			*calls = append(*calls, argv)
		}
		return []byte(stdout), []byte(stderr), code, nil
	}
}

func TestOverviewReadsIdentityAndAllowance(t *testing.T) {
	var calls [][]string
	m := &Muse{ConfigDir: writeAuth(t, authFixture), Binary: "/bin/muse", Run: runner(usageFixture, "", 0, &calls)}
	overview, err := m.Overview(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !overview.Installed || !overview.LoggedIn || overview.Account == nil {
		t.Fatalf("overview = %+v", overview)
	}
	a := overview.Account
	if a.Email != "someone@example.com" || a.Name != "Some One" || !a.Subscribed || a.Tier != "Muse Code Power Usage" {
		t.Fatalf("account = %+v", a)
	}
	if len(a.Windows) != 2 || a.Windows[0].Label != "5-hour" || a.Windows[1].Label != "weekly" {
		t.Fatalf("windows = %+v", a.Windows)
	}
	if *a.Windows[0].UsedPercent != 40 || *a.Windows[1].ResetsAt != 1792972800 {
		t.Fatalf("window values = %+v %+v", a.Windows[0], a.Windows[1])
	}
	if a.AsOf == nil || *a.AsOf < 1792909547 || *a.AsOf > 1792909548 {
		t.Fatalf("as_of = %v", a.AsOf)
	}
	if len(calls) != 1 || strings.Join(calls[0], " ") != "/bin/muse usage --json" {
		t.Fatalf("calls = %v", calls)
	}
}

func TestOverviewNeverExposesTokens(t *testing.T) {
	m := &Muse{ConfigDir: writeAuth(t, authFixture), Binary: "/bin/muse", Run: runner(usageFixture, "", 0, nil)}
	overview, _ := m.Overview(context.Background())
	out := strings.Join([]string{overview.Account.Email, overview.Account.Name, overview.Account.Mechanism}, " ")
	if strings.Contains(out, "SECRET") {
		t.Fatalf("token leaked: %s", out)
	}
}

func TestOverviewAbsentWhenNotInstalledAndNotSignedIn(t *testing.T) {
	m := &Muse{ConfigDir: t.TempDir(), Run: runner("", "", 0, nil)}
	// Overview would fall back to Find(), which may see a real muse on a
	// developer machine; the resolved-binary form forces "absent".
	got, err := m.overview(context.Background(), "")
	if err != nil || got != nil {
		t.Fatalf("got %+v, %v; want nil", got, err)
	}
}

func TestOverviewInstalledButSignedOut(t *testing.T) {
	m := &Muse{ConfigDir: t.TempDir(), Binary: "/bin/muse", Run: runner("", "", 0, nil)}
	overview, err := m.Overview(context.Background())
	if err != nil || overview == nil || !overview.Installed || overview.LoggedIn || overview.Account != nil {
		t.Fatalf("overview = %+v, %v", overview, err)
	}
}

func TestOverviewNoUsageYetIsAReason(t *testing.T) {
	body := `{"allowance":null,"hint":"use muse once","reason":"server_sent_no_usage","schemaVersion":1}`
	m := &Muse{ConfigDir: writeAuth(t, authFixture), Binary: "/bin/muse", Run: runner(body, "", 0, nil)}
	overview, _ := m.Overview(context.Background())
	a := overview.Account
	if a.Error != "" || a.Reason != "server_sent_no_usage" || a.Hint != "use muse once" || len(a.Windows) != 0 {
		t.Fatalf("account = %+v", a)
	}
}

func TestOverviewReportsCLIFailure(t *testing.T) {
	m := &Muse{ConfigDir: writeAuth(t, authFixture), Binary: "/bin/muse",
		Run: runner("", "muse usage: not logged in: run `muse login` first", 1, nil)}
	overview, _ := m.Overview(context.Background())
	if !strings.Contains(overview.Account.Error, "not logged in") {
		t.Fatalf("error = %q", overview.Account.Error)
	}
}

func TestParseUsageRejectsUnknownSchema(t *testing.T) {
	var a Account
	if err := ParseUsage([]byte(`{"schemaVersion":2}`), &a); err == nil {
		t.Fatal("want an error for schemaVersion 2")
	}
}
