package claude

import "strings"

// Account is one signed-in Claude account.
//
// Token is held only long enough to probe quota and is never serialised: Public
// is the only view that leaves the process.
type Account struct {
	Alias            string
	Email            string
	Org              string
	OrgUUID          string
	Plan             string
	IsActive         bool
	AccessExpiresAt  *int64 // milliseconds since the epoch
	RefreshExpiresAt *int64 // milliseconds since the epoch
	// Token is excluded from JSON so that even a direct Marshal of an Account,
	// rather than of its Public view, can never carry it out of the process.
	Token string `json:"-"`

	RateLimitTier string
}

// PublicAccount is the view of an Account that may leave this process. It never
// includes a token; field names match the Python `public()` dictionary.
type PublicAccount struct {
	Alias            string  `json:"alias"`
	Email            *string `json:"email"`
	Org              *string `json:"org"`
	Plan             *string `json:"plan"`
	PlanLabel        string  `json:"plan_label"`
	RateLimitTier    *string `json:"rate_limit_tier"`
	IsActive         bool    `json:"is_active"`
	AccessExpiresAt  *int64  `json:"access_expires_at"`
	RefreshExpiresAt *int64  `json:"refresh_expires_at"`
}

var planNames = map[string]string{"max": "Max", "team": "Team", "pro": "Pro", "free": "Free"}

// PlanLabel is the plan as shown to people: Max, Team, Pro or Free, with the
// rate-limit multiplier appended when the tier carries one ("Max 20×").
func (a Account) PlanLabel() string {
	name, ok := planNames[a.Plan]
	if !ok {
		name = a.Plan
		if name == "" {
			name = "Unknown plan"
		}
	}
	switch {
	case strings.HasSuffix(a.RateLimitTier, "_max_20x"):
		return name + " 20×"
	case strings.HasSuffix(a.RateLimitTier, "_max_5x"):
		return name + " 5×"
	}
	return name
}

// Public is the view that may leave this process. Never includes a token.
func (a Account) Public() PublicAccount {
	return PublicAccount{
		Alias:            a.Alias,
		Email:            nullable(a.Email),
		Org:              nullable(a.Org),
		Plan:             nullable(a.Plan),
		PlanLabel:        a.PlanLabel(),
		RateLimitTier:    nullable(a.RateLimitTier),
		IsActive:         a.IsActive,
		AccessExpiresAt:  a.AccessExpiresAt,
		RefreshExpiresAt: a.RefreshExpiresAt,
	}
}

// String mirrors the Python dataclass repr, which excludes the token so an
// account can be logged without leaking it.
func (a Account) String() string {
	var b strings.Builder
	b.WriteString("Account{Alias:" + a.Alias)
	b.WriteString(" Email:" + a.Email)
	b.WriteString(" Org:" + a.Org)
	b.WriteString(" OrgUUID:" + a.OrgUUID)
	b.WriteString(" Plan:" + a.Plan)
	if a.IsActive {
		b.WriteString(" IsActive:true")
	} else {
		b.WriteString(" IsActive:false")
	}
	b.WriteString(" RateLimitTier:" + a.RateLimitTier + "}")
	return b.String()
}

// nullable maps the empty string to a JSON null, which is what the Python emits
// for a missing value.
func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// accountFromOauth builds the shared part of an Account from a `claudeAiOauth`
// object.
func accountFromOauth(alias string, oauth map[string]any) Account {
	return Account{
		Alias:            alias,
		Plan:             getString(oauth, "subscriptionType"),
		RateLimitTier:    getString(oauth, "rateLimitTier"),
		AccessExpiresAt:  getInt64(oauth, "expiresAt"),
		RefreshExpiresAt: getInt64(oauth, "refreshTokenExpiresAt"),
		Token:            getString(oauth, "accessToken"),
	}
}
