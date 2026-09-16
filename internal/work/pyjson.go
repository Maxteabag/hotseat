package work

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// pyDumps encodes the revision material exactly as Python's json.dumps does with
// default arguments: ", " and ": " separators, ensure_ascii, float repr. Only the
// value kinds the material contains are handled; the revision must stay
// byte-identical to the Python one so a confirmation made against either
// implementation is honoured by the other.
func pyDumps(value any) string {
	var out strings.Builder
	pyEncode(&out, value)
	return out.String()
}

func pyEncode(out *strings.Builder, value any) {
	switch v := value.(type) {
	case nil:
		out.WriteString("null")
	case bool:
		if v {
			out.WriteString("true")
		} else {
			out.WriteString("false")
		}
	case int:
		out.WriteString(strconv.Itoa(v))
	case int64:
		out.WriteString(strconv.FormatInt(v, 10))
	case float64:
		out.WriteString(pyFloatRepr(v))
	case string:
		pyEncodeString(out, v)
	case []any:
		out.WriteByte('[')
		for i, item := range v {
			if i > 0 {
				out.WriteString(", ")
			}
			pyEncode(out, item)
		}
		out.WriteByte(']')
	default:
		panic(fmt.Sprintf("pyDumps: unsupported %T", value))
	}
}

// pyEncodeString mirrors json.encoder.py_encode_basestring_ascii.
func pyEncodeString(out *strings.Builder, s string) {
	out.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			out.WriteString(`\"`)
		case '\\':
			out.WriteString(`\\`)
		case '\n':
			out.WriteString(`\n`)
		case '\r':
			out.WriteString(`\r`)
		case '\t':
			out.WriteString(`\t`)
		case '\b':
			out.WriteString(`\b`)
		case '\f':
			out.WriteString(`\f`)
		default:
			if r >= ' ' && r <= '~' {
				out.WriteRune(r)
			} else if r > 0xffff {
				r -= 0x10000
				fmt.Fprintf(out, `\u%04x\u%04x`, 0xd800|(r>>10), 0xdc00|(r&0x3ff))
			} else {
				fmt.Fprintf(out, `\u%04x`, r)
			}
		}
	}
	out.WriteByte('"')
}

// pyFloatRepr formats a float the way Python's repr does: the shortest
// round-tripping digits, fixed notation for exponents in [-4, 16) with a ".0" on
// integral values, exponent notation otherwise.
func pyFloatRepr(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	}
	sci := strconv.FormatFloat(f, 'e', -1, 64)
	exp, _ := strconv.Atoi(sci[strings.IndexByte(sci, 'e')+1:])
	if f == 0 || (exp >= -4 && exp < 16) {
		fixed := strconv.FormatFloat(f, 'f', -1, 64)
		if !strings.ContainsAny(fixed, ".") {
			fixed += ".0"
		}
		return fixed
	}
	return sci
}
