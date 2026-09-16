package quota

import (
	"bytes"
	"encoding/json"
	"strconv"
)

// Source says which reader produced a Summary. It selects the JSON key set, so
// that a Summary marshals to exactly the dict the corresponding Python function
// returned: usage.summarise and ratelimits.summarise share the headline keys but
// each carries fields the other never emits.
type Source int

const (
	// SourceUsage is the OAuth usage endpoint (usage.summarise). It is the zero
	// value because it is the only source the cooldown cache ever stores.
	SourceUsage Source = iota
	// SourceProbe is the rate-limit header probe (ratelimits.summarise).
	SourceProbe
)

// ScopedLimit is one per-model weekly limit, separate from the overall allowance.
type ScopedLimit struct {
	Model     string   `json:"model"`
	Used      float64  `json:"used"`
	Severity  string   `json:"severity"`
	Reset     *float64 `json:"reset"` // epoch seconds; nil when the endpoint gave none
	Exhausted bool     `json:"exhausted"`
}

// Summary is what the interfaces render: the union of the dicts returned by the
// Python usage.summarise and ratelimits.summarise, with Source deciding which
// keys are emitted.
//
// Nullability: numbers that Python leaves as None are pointers (nil marshals to
// null; 0 is a real reading). Strings that Python leaves as None are plain
// strings and marshal to null when empty (Severity, ExtraUsageReason, Plan,
// OverageStatus, OverageReason, Binding, Fallback). Reset times are epoch
// seconds.
type Summary struct {
	Source Source `json:"-"`

	// Emitted by both sources.
	Status    string   `json:"status"`
	Available bool     `json:"available"`
	Limited   bool     `json:"limited"`
	Used5h    *float64 `json:"used_5h"`
	Reset5h   *float64 `json:"reset_5h"`
	Used7d    *float64 `json:"used_7d"`
	Reset7d   *float64 `json:"reset_7d"`

	// Emitted by SourceUsage. Scoped and BlockedModels are also emitted for
	// SourceProbe when non-nil, which is how collect marks a header-probe
	// fallback as having no per-model information.
	Severity          string        `json:"severity"`
	Scoped            []ScopedLimit `json:"scoped"`
	BlockedModels     []string      `json:"blocked_models"`
	ExtraUsageEnabled bool          `json:"extra_usage_enabled"`
	ExtraUsageReason  string        `json:"extra_usage_reason"`
	Plan              string        `json:"plan"`

	// Emitted by SourceProbe.
	Status5h      string   `json:"status_5h"`
	Status7d      string   `json:"status_7d"`
	OverageStatus string   `json:"overage_status"`
	OverageReason string   `json:"overage_reason"`
	Binding       string   `json:"binding"`
	Fallback      string   `json:"fallback"`
	FallbackAt    *float64 `json:"fallback_at"`

	// Degraded is set by the collector on a header-probe fallback
	// ("per-model limits unavailable"); emitted only when non-empty.
	Degraded string `json:"degraded"`
}

type jsonField struct {
	key   string
	value any
}

// nullable maps the empty string to JSON null, the way Python emits None.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (s Summary) fields() []jsonField {
	var out []jsonField
	add := func(key string, value any) { out = append(out, jsonField{key, value}) }
	switch s.Source {
	case SourceProbe:
		add("status", s.Status)
		add("status_5h", s.Status5h)
		add("status_7d", s.Status7d)
		add("available", s.Available)
		add("limited", s.Limited)
		add("used_5h", s.Used5h)
		add("reset_5h", s.Reset5h)
		add("used_7d", s.Used7d)
		add("reset_7d", s.Reset7d)
		add("overage_status", nullable(s.OverageStatus))
		add("overage_reason", nullable(s.OverageReason))
		add("binding", nullable(s.Binding))
		add("fallback", nullable(s.Fallback))
		add("fallback_at", s.FallbackAt)
		if s.Scoped != nil {
			add("scoped", s.Scoped)
		}
		if s.BlockedModels != nil {
			add("blocked_models", s.BlockedModels)
		}
	default:
		add("status", s.Status)
		add("available", s.Available)
		add("limited", s.Limited)
		add("severity", nullable(s.Severity))
		add("used_5h", s.Used5h)
		add("reset_5h", s.Reset5h)
		add("used_7d", s.Used7d)
		add("reset_7d", s.Reset7d)
		scoped := s.Scoped
		if scoped == nil {
			scoped = []ScopedLimit{}
		}
		add("scoped", scoped)
		blocked := s.BlockedModels
		if blocked == nil {
			blocked = []string{}
		}
		add("blocked_models", blocked)
		add("extra_usage_enabled", s.ExtraUsageEnabled)
		add("extra_usage_reason", nullable(s.ExtraUsageReason))
		add("plan", nullable(s.Plan))
	}
	if s.Degraded != "" {
		add("degraded", s.Degraded)
	}
	return out
}

// marshalFields writes an object with the keys in the given order, matching the
// insertion order of the Python dicts.
func marshalFields(fields []jsonField) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, f := range fields {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.WriteString(strconv.Quote(f.key))
		buf.WriteByte(':')
		value, err := json.Marshal(f.value)
		if err != nil {
			return nil, err
		}
		buf.Write(value)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// MarshalJSON emits exactly the keys the Python summarise for s.Source returned.
func (s Summary) MarshalJSON() ([]byte, error) {
	return marshalFields(s.fields())
}

// UnmarshalJSON reads either dict shape; the source is inferred from the keys
// present (severity marks the usage endpoint, status_5h the header probe).
func (s *Summary) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	type plain Summary
	var p plain
	if err := json.Unmarshal(data, &p); err != nil {
		return err
	}
	*s = Summary(p)
	if _, usage := raw["severity"]; !usage {
		if _, probe := raw["status_5h"]; probe {
			s.Source = SourceProbe
		}
	}
	return nil
}
