package quota

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Cache Claude quota reads and retain dated results while the API backs off.

// MinInterval is how long a successful read is reused, in seconds. It is also
// the base of the exponential backoff after a 429.
const MinInterval = 60

// DefaultRoot is ${XDG_CACHE_HOME:-~/.cache}/hotseat/quota-cooldowns.
func DefaultRoot() string {
	base := os.Getenv("XDG_CACHE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "."
		}
		base = filepath.Join(home, ".cache")
	}
	return filepath.Join(base, "hotseat", "quota-cooldowns")
}

// Filename is the cache file for a token: sha256(token) hex plus ".json", so the
// token itself never touches disk.
func Filename(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:]) + ".json"
}

// Cache is the per-token cooldown cache. The zero value uses DefaultRoot,
// time.Now and ForToken.
type Cache struct {
	// Root is the directory holding one JSON file per token.
	Root string
	// Now is the clock; tests fix it.
	Now func() time.Time
	// Fetch reads a fresh summary; defaults to ForToken.
	Fetch func(ctx context.Context, token string) (Summary, error)
	// Warn receives cache-file write failures, which never fail a read. Nil
	// discards them, as the Python did.
	Warn func(error)
}

// Result is a Summary plus the cache bookkeeping the bridge shows: when the
// numbers were actually read, whether they came from disk, and any throttling
// warning. It marshals as the summary dict with the _checked_at, _cached and
// _warning keys added.
type Result struct {
	Summary
	CheckedAt float64 `json:"_checked_at"`
	Cached    bool    `json:"_cached"`
	Warning   string  `json:"_warning"`
}

// MarshalJSON flattens the summary and the bookkeeping keys into one object.
func (r Result) MarshalJSON() ([]byte, error) {
	fields := r.Summary.fields()
	fields = append(fields,
		jsonField{"_checked_at", r.CheckedAt},
		jsonField{"_cached", r.Cached},
		jsonField{"_warning", r.Warning},
	)
	return marshalFields(fields)
}

// UnmarshalJSON is the inverse of MarshalJSON.
func (r *Result) UnmarshalJSON(data []byte) error {
	if err := r.Summary.UnmarshalJSON(data); err != nil {
		return err
	}
	var extra struct {
		CheckedAt float64 `json:"_checked_at"`
		Cached    bool    `json:"_cached"`
		Warning   string  `json:"_warning"`
	}
	if err := json.Unmarshal(data, &extra); err != nil {
		return err
	}
	r.CheckedAt, r.Cached, r.Warning = extra.CheckedAt, extra.Cached, extra.Warning
	return nil
}

// record is the on-disk shape, shared with the Python implementation.
type record struct {
	Data      json.RawMessage `json:"data"`
	CheckedAt float64         `json:"checked_at"`
	Until     float64         `json:"until"`
	Failures  float64         `json:"failures"`
}

// summary decodes the stored data, or nil when it is not a JSON object.
func (rec record) summary() *Summary {
	trimmed := strings.TrimSpace(string(rec.Data))
	if !strings.HasPrefix(trimmed, "{") {
		return nil
	}
	var s Summary
	if err := json.Unmarshal(rec.Data, &s); err != nil {
		return nil
	}
	return &s
}

func load(path string) record {
	var rec record
	raw, err := os.ReadFile(path)
	if err != nil {
		return record{}
	}
	if err := json.Unmarshal(raw, &rec); err != nil {
		return record{}
	}
	return rec
}

// store writes the record atomically: temp file in the same directory, fsync,
// 0600, rename. Failures are reported through warn and otherwise ignored.
func store(path string, rec record, warn func(error)) {
	err := func() error {
		dir := filepath.Dir(path)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		data, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		tmp, err := os.CreateTemp(dir, "tmp")
		if err != nil {
			return err
		}
		name := tmp.Name()
		defer os.Remove(name)
		if _, err := tmp.Write(data); err != nil {
			tmp.Close()
			return err
		}
		if err := tmp.Sync(); err != nil {
			tmp.Close()
			return err
		}
		if err := tmp.Chmod(0o600); err != nil {
			tmp.Close()
			return err
		}
		if err := tmp.Close(); err != nil {
			return err
		}
		return os.Rename(name, path)
	}()
	if err != nil && warn != nil {
		warn(fmt.Errorf("quota cache: %w", err))
	}
}

func (c *Cache) now() float64 {
	clock := c.Now
	if clock == nil {
		clock = time.Now
	}
	t := clock()
	return float64(t.UnixNano()) / 1e9
}

// cached builds the Result for a record that is still inside its cooldown.
// Without usable data the throttling message becomes the error itself.
func cached(rec record, now float64) (Result, error) {
	data := rec.summary()
	warning := ""
	if rec.Failures != 0 || data == nil {
		wait := int(math.Trunc(rec.Until-now)) + 1
		if wait < 1 {
			wait = 1
		}
		warning = fmt.Sprintf("Usage API throttled (HTTP 429); retry in %ds", wait)
	}
	if data == nil {
		return Result{}, &UsageError{Message: warning}
	}
	return Result{Summary: *data, CheckedAt: rec.CheckedAt, Cached: true, Warning: warning}, nil
}

// Read returns the usage summary for a token, from disk while its cooldown
// holds and otherwise freshly fetched. A 429 starts an exponential backoff
// (MinInterval doubling per failure, capped at five failures, never shorter
// than Retry-After) during which the last good numbers are served with a
// warning. Any other failure is returned as-is: an authentication rejection
// must never be masked by a cached success.
func (c *Cache) Read(ctx context.Context, token string) (Result, error) {
	root := c.Root
	if root == "" {
		root = DefaultRoot()
	}
	path := filepath.Join(root, Filename(token))
	now := c.now()
	rec := load(path)
	if rec.Until > now {
		return cached(rec, now)
	}
	fetch := c.Fetch
	if fetch == nil {
		fetch = ForToken
	}
	result, err := fetch(ctx, token)
	if err == nil {
		checked := c.now()
		data, marshalErr := json.Marshal(result)
		if marshalErr != nil {
			return Result{}, &UsageError{Message: marshalErr.Error()}
		}
		store(path, record{Data: data, CheckedAt: checked, Until: checked + MinInterval, Failures: 0}, c.Warn)
		return Result{Summary: result, CheckedAt: checked, Cached: false, Warning: ""}, nil
	}
	var usageErr *UsageError
	if errors.As(err, &usageErr) && strings.Contains(usageErr.Error(), "429") {
		failures := min(int(rec.Failures)+1, 5)
		delay := float64(MinInterval) * math.Pow(2, float64(failures-1))
		if usageErr.RetryAfter != nil && *usageErr.RetryAfter > delay {
			delay = *usageErr.RetryAfter
		}
		rec.Until = now + delay
		rec.Failures = float64(failures)
		store(path, rec, c.Warn)
		return cached(rec, now)
	}
	return Result{}, err
}

// Read is Cache.Read on the zero-value Cache: default root, clock and fetcher.
func Read(ctx context.Context, token string) (Result, error) {
	return (&Cache{}).Read(ctx, token)
}
