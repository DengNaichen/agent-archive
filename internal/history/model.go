// Package history owns private snapshots and the source-specific log formats.
package history

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/DengNaichen/agent-archive/internal/fault"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
)

type Event struct {
	Source     string `json:"source"`
	SessionID  string `json:"session_id"`
	EventID    string `json:"event_id"`
	EventTime  string `json:"event_time"`
	EventType  string `json:"event_type"`
	Subtype    string `json:"subtype"`
	TurnID     string `json:"turn_id"`
	Activity   string `json:"activity_class"`
	Model      string `json:"model"`
	Role       string `json:"role"`
	Phase      string `json:"phase"`
	RecordSHA  string `json:"record_sha256"`
	RecordJSON string `json:"record_json"`
	Locations  string `json:"source_locations_json"`
}

var Columns = []string{"source", "session_id", "event_id", "event_time", "event_type", "subtype", "turn_id", "activity_class", "model", "role", "phase", "record_sha256", "record_json", "source_locations_json"}

func SHA(b []byte) string                   { return fmt.Sprintf("%x", sha256.Sum256(b)) }
func Str(m map[string]any, k string) string { s, _ := m[k].(string); return s }
func Obj(v any) map[string]any {
	m, _ := v.(map[string]any)
	if m == nil {
		return map[string]any{}
	}
	return m
}
func Decode(b []byte) (map[string]any, error) {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var m map[string]any
	if err := d.Decode(&m); err != nil {
		return nil, fmt.Errorf("decode record: %w", err)
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return nil, fault.New("record has trailing data")
	}
	if m == nil {
		return nil, fault.New("record is not an object")
	}
	return m, nil
}

// Python-compatible JSON canonicalization keeps IDs compatible with the prototype.
// Unknown fields and large integer tokens are preserved; floats follow Python's
// shortest-roundtrip formatting with its fixed/exponential cutoffs.
func quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 32 {
				fmt.Fprintf(&b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}
func number(n json.Number) (string, error) {
	s := n.String()
	if !strings.ContainsAny(s, ".eE") {
		if s == "-0" {
			return "0", nil
		}
		return s, nil
	}
	f, e := strconv.ParseFloat(s, 64)
	if e != nil || math.IsInf(f, 0) || math.IsNaN(f) {
		return "", fault.New("non-finite numeric token")
	}
	format := byte('g')
	if f == 0 || (math.Abs(f) >= 1e-4 && math.Abs(f) < 1e16) {
		format = 'f'
	}
	s = strconv.FormatFloat(f, format, -1, 64)
	if !strings.ContainsAny(s, ".eE") {
		s += ".0"
	}
	return s, nil
}
func Canonical(v any) (string, error) {
	switch x := v.(type) {
	case nil:
		return "null", nil
	case string:
		return quote(x), nil
	case bool:
		return strconv.FormatBool(x), nil
	case json.Number:
		return number(x)
	case []any:
		a := make([]string, len(x))
		for i, v := range x {
			s, e := Canonical(v)
			if e != nil {
				return "", e
			}
			a[i] = s
		}
		return "[" + strings.Join(a, ",") + "]", nil
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		a := make([]string, 0, len(keys))
		for _, k := range keys {
			s, e := Canonical(x[k])
			if e != nil {
				return "", e
			}
			a = append(a, quote(k)+":"+s)
		}
		return "{" + strings.Join(a, ",") + "}", nil
	default:
		return "", fault.New("unsupported canonical value")
	}
}
func Fingerprint(v any) (string, error) {
	s, e := Canonical(v)
	if e != nil {
		return "", e
	}
	return SHA([]byte(s)), nil
}

// CompactOriginal normalizes escapes/numbers while preserving object key order.
func CompactOriginal(b []byte) (string, error) {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	return ordered(d)
}
func ordered(d *json.Decoder) (string, error) {
	t, e := d.Token()
	if e != nil {
		return "", e
	}
	if delim, ok := t.(json.Delim); ok {
		var a []string
		switch delim {
		case '{':
			for d.More() {
				k, e := d.Token()
				if e != nil {
					return "", e
				}
				v, e := ordered(d)
				if e != nil {
					return "", e
				}
				a = append(a, quote(k.(string))+":"+v)
			}
			if _, e = d.Token(); e != nil {
				return "", e
			}
			return "{" + strings.Join(a, ",") + "}", nil
		case '[':
			for d.More() {
				v, e := ordered(d)
				if e != nil {
					return "", e
				}
				a = append(a, v)
			}
			if _, e = d.Token(); e != nil {
				return "", e
			}
			return "[" + strings.Join(a, ",") + "]", nil
		}
		return "", fault.New("invalid delimiter")
	}
	return Canonical(t)
}
