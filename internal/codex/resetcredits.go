package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"
)

// This file ports resetcredits: read Codex usage-reset balances. Nothing here
// redeems or buys resets.

// ResetEndpoint is the balance endpoint, queried with a plain GET.
const ResetEndpoint = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits"

// Reading is one balance lookup; AvailableCount is nil when it is unknown.
type Reading struct {
	AvailableCount *int    `json:"available_count"`
	CheckedAt      float64 `json:"checked_at"`
	Error          *string `json:"error"`
}

// Balance is a Reading labelled with the account it belongs to.
type Balance struct {
	Alias string  `json:"alias"`
	Email *string `json:"email"`
	Reading
}

// ReadBalance fetches the banked reset count for one credential file. Every
// failure is reported in Error, never as a Go error, and the token never
// leaves the request headers.
func ReadBalance(ctx context.Context, client *http.Client, path string) Reading {
	result := Reading{CheckedAt: float64(time.Now().UnixNano()) / 1e9}
	if client == nil {
		client = http.DefaultClient
	}
	count, err := readBalance(ctx, client, path)
	if err != nil {
		msg := err.Error()
		result.Error = &msg
		return result
	}
	result.AvailableCount = &count
	return result
}

type httpStatusError int

func (e httpStatusError) Error() string { return fmt.Sprintf("HTTP %d", int(e)) }

func readBalance(ctx context.Context, client *http.Client, path string) (int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	var stored map[string]any
	if err := json.Unmarshal(raw, &stored); err != nil {
		return 0, err
	}
	tokens, _ := stored["tokens"].(map[string]any)
	accessToken, accountID := stringOf(tokens["access_token"]), stringOf(tokens["account_id"])
	if accessToken == "" || accountID == "" {
		return 0, errors.New("No complete OAuth credentials")
	}
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, ResetEndpoint, nil)
	if err != nil {
		return 0, err
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set("ChatGPT-Account-ID", accountID)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "Codex Desktop")
	request.Header.Set("originator", "Codex Desktop")
	request.Header.Set("OAI-Product-Sku", "CODEX")
	response, err := client.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	if response.StatusCode >= 400 {
		return 0, httpStatusError(response.StatusCode)
	}
	decoder := json.NewDecoder(response.Body)
	decoder.UseNumber()
	var payload any
	if err := decoder.Decode(&payload); err != nil {
		return 0, err
	}
	object, ok := payload.(map[string]any)
	if !ok {
		return 0, errors.New("Invalid reset balance response")
	}
	// `type(count) is not int or count < 0`: a bool, a float and a string are
	// all rejected, and so is a negative count.
	literal, ok := object["available_count"].(json.Number)
	if !ok || strings.ContainsAny(literal.String(), ".eE") {
		return 0, errors.New("Reset balance response has no valid available_count")
	}
	count, err := strconv.Atoi(literal.String())
	if err != nil || count < 0 {
		return 0, errors.New("Reset balance response has no valid available_count")
	}
	return count, nil
}

// Balances reads the reset balance of every account, or only the named
// aliases, three at a time. An alias that is not a known profile is refused
// before anything is queried.
func (c *Codex) Balances(ctx context.Context, aliases []string) ([]Balance, error) {
	accounts := c.Accounts()
	if len(aliases) > 0 {
		known := map[string]bool{}
		for _, account := range accounts {
			known[account.Alias] = true
		}
		wanted := map[string]bool{}
		missing := []string{}
		for _, alias := range aliases {
			wanted[alias] = true
			if !known[alias] {
				missing = append(missing, alias)
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			return nil, &Error{"Unknown Codex profiles: " + strings.Join(uniqueSorted(missing), ", ")}
		}
		filtered := []Account{}
		for _, account := range accounts {
			if wanted[account.Alias] {
				filtered = append(filtered, account)
			}
		}
		accounts = filtered
	}
	rows := make([]Balance, len(accounts))
	group, ctx := errgroup.WithContext(ctx)
	group.SetLimit(3)
	for i, account := range accounts {
		group.Go(func() error {
			path := c.LiveAuth()
			if account.Saved {
				path = filepath.Join(c.ProfilesDir(), account.Alias, "auth.json")
			}
			rows[i] = Balance{Alias: account.Alias, Email: account.Email, Reading: ReadBalance(ctx, c.httpClient(), path)}
			return nil
		})
	}
	_ = group.Wait()
	return rows, nil
}

func uniqueSorted(values []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, v := range values {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

// FetchResetCredits returns the raw reset-credit payload for a credential
// file, for the profile probe. Every failure collapses to an empty balance:
// the probe result must never be lost to a credits lookup.
func FetchResetCredits(ctx context.Context, client *http.Client, authPath string) map[string]any {
	empty := func() map[string]any { return map[string]any{"available_count": 0, "credits": []any{}} }
	raw, err := os.ReadFile(authPath)
	if err != nil {
		return empty()
	}
	var stored map[string]any
	if err := json.Unmarshal(raw, &stored); err != nil {
		return empty()
	}
	tokens, _ := stored["tokens"].(map[string]any)
	accessToken := stringOf(tokens["access_token"])
	if accessToken == "" {
		return empty()
	}
	if client == nil {
		client = http.DefaultClient
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, ResetEndpoint, nil)
	if err != nil {
		return empty()
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set("User-Agent", "Codex Desktop")
	request.Header.Set("originator", "Codex Desktop")
	request.Header.Set("OAI-Product-Sku", "CODEX")
	request.Header.Set("Accept", "application/json")
	if accountID := stringOf(tokens["account_id"]); accountID != "" {
		request.Header.Set("ChatGPT-Account-ID", accountID)
	}
	response, err := client.Do(request)
	if err != nil {
		return empty()
	}
	defer response.Body.Close()
	if response.StatusCode >= 400 {
		return empty()
	}
	var payload map[string]any
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil || payload == nil {
		return empty()
	}
	return payload
}
