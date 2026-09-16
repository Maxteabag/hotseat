package codex

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// The helpers here reproduce the Python semantics the ported modules relied
// on when they handled loosely typed JSON: truthiness, "is this a number and
// not a bool", insertion-ordered objects and repr()-style formatting.

// truthy mirrors Python's bool() on a decoded JSON value.
func truthy(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case bool:
		return v
	case string:
		return v != ""
	case float64:
		return v != 0
	case json.Number:
		f, err := v.Float64()
		return err != nil || f != 0
	case []any:
		return len(v) > 0
	case map[string]any:
		return len(v) > 0
	case *orderedObject:
		return v != nil && len(v.keys) > 0
	}
	return true
}

// number accepts an int or float JSON value and rejects bools, like the
// Python `isinstance(x, (int, float)) and not isinstance(x, bool)` checks.
func number(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	}
	return 0, false
}

// stringOf returns a JSON string value, or "" for anything else.
func stringOf(value any) string {
	s, _ := value.(string)
	return s
}

// stringPtr returns a JSON string value as a pointer, nil for anything else.
func stringPtr(value any) *string {
	if s, ok := value.(string); ok {
		return &s
	}
	return nil
}

func numberPtr(value any) *float64 {
	if f, ok := number(value); ok {
		return &f
	}
	return nil
}

func ptrEqual[T comparable](a, b *T) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// orderedObject is a JSON object that remembers key order, which Python dicts
// do and Go maps do not. Values are decoded recursively into orderedObject,
// []any, string, json.Number, bool or nil.
type orderedObject struct {
	keys   []string
	values map[string]any
}

func (o *orderedObject) get(key string) any {
	if o == nil {
		return nil
	}
	return o.values[key]
}

// decodeOrdered decodes raw JSON keeping object key order and number literals.
func decodeOrdered(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	value, err := decodeValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err == nil {
		return nil, fmt.Errorf("extra data after JSON value")
	}
	return value, nil
}

func decodeValue(dec *json.Decoder) (any, error) {
	token, err := dec.Token()
	if err != nil {
		return nil, err
	}
	return decodeFrom(dec, token)
}

func decodeFrom(dec *json.Decoder, token json.Token) (any, error) {
	switch t := token.(type) {
	case json.Delim:
		switch t {
		case '{':
			obj := &orderedObject{values: map[string]any{}}
			for dec.More() {
				keyToken, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key, ok := keyToken.(string)
				if !ok {
					return nil, fmt.Errorf("object key is not a string")
				}
				value, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				if _, seen := obj.values[key]; !seen {
					obj.keys = append(obj.keys, key)
				}
				obj.values[key] = value
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return obj, nil
		case '[':
			list := []any{}
			for dec.More() {
				value, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				list = append(list, value)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return list, nil
		}
		return nil, fmt.Errorf("unexpected delimiter %v", t)
	default:
		return token, nil
	}
}

// pyRepr formats a decoded JSON value the way Python's str() shows the
// dict json.loads would have produced. It is used for error payloads that
// the Python surfaced to users as `str(msg["error"])`.
func pyRepr(value any) string {
	switch v := value.(type) {
	case nil:
		return "None"
	case bool:
		if v {
			return "True"
		}
		return "False"
	case string:
		return pyQuote(v)
	case json.Number:
		return v.String()
	case float64:
		if v == float64(int64(v)) {
			return strconv.FormatInt(int64(v), 10)
		}
		return strconv.FormatFloat(v, 'g', -1, 64)
	case []any:
		parts := make([]string, len(v))
		for i, item := range v {
			parts[i] = pyRepr(item)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case *orderedObject:
		parts := make([]string, 0, len(v.keys))
		for _, key := range v.keys {
			parts = append(parts, pyQuote(key)+": "+pyRepr(v.values[key]))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, key := range keys {
			parts = append(parts, pyQuote(key)+": "+pyRepr(v[key]))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return fmt.Sprint(value)
}

// pyQuote is Python's repr() of a str: single quotes unless the text holds a
// single quote and no double quote.
func pyQuote(s string) string {
	quote := "'"
	if strings.Contains(s, "'") && !strings.Contains(s, "\"") {
		quote = "\""
	}
	var b strings.Builder
	b.WriteString(quote)
	for _, r := range s {
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case string(r) == quote:
			b.WriteString(`\` + quote)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteString(quote)
	return b.String()
}
