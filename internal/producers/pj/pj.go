// Package pj renders model-visible JSON the way the Python producers did:
// json.dumps(..., ensure_ascii=False) — insertion-ordered keys, ", " and ": "
// separators, no HTML escaping. Keeping the bytes the model sees unchanged
// across the rewrite keeps llm_calls comparable before and after cutover.
//
// One known difference remains: Python prints an integral float as 100.0,
// Go as 100 (the type is gone once a value is a float64).
package pj

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// KV is one key of an ordered object.
type KV struct {
	Key string
	Val any
}

// Obj is a JSON object that keeps key order (Python dict semantics).
type Obj []KV

// Get returns the value for key.
func (o Obj) Get(key string) (any, bool) {
	for _, kv := range o {
		if kv.Key == key {
			return kv.Val, true
		}
	}
	return nil, false
}

// Set overwrites key in place, or appends it (dict assignment).
func (o *Obj) Set(key string, val any) {
	for i := range *o {
		if (*o)[i].Key == key {
			(*o)[i].Val = val
			return
		}
	}
	*o = append(*o, KV{key, val})
}

// Del removes key (dict.pop).
func (o *Obj) Del(key string) {
	for i := range *o {
		if (*o)[i].Key == key {
			*o = append((*o)[:i], (*o)[i+1:]...)
			return
		}
	}
}

func (o Obj) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, kv := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		k, err := compact(kv.Key)
		if err != nil {
			return nil, err
		}
		b.Write(k)
		b.WriteByte(':')
		v, err := compact(kv.Val)
		if err != nil {
			return nil, err
		}
		b.Write(v)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

func compact(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return pyText(bytes.TrimRight(b.Bytes(), "\n"), false), nil
}

// Compact marshals v without HTML escaping (for stored JSON: features,
// payloads, kv values).
func Compact(v any) ([]byte, error) { return compact(v) }

// Dumps is json.dumps(v, ensure_ascii=False).
func Dumps(v any) (string, error) {
	b, err := compact(v)
	if err != nil {
		return "", err
	}
	return string(PySeparators(b)), nil
}

// PySeparators rewrites compact JSON to Python's default ", " / ": ".
func PySeparators(b []byte) []byte { return pyText(b, true) }

// unescaped are the escapes Go emits that json.dumps(ensure_ascii=False)
// does not: HTML characters (from json.RawMessage values that an escaping
// encoder produced upstream) and U+2028/U+2029, which Python prints raw.
var unescaped = map[string]string{"003c": "<", "003e": ">", "0026": "&", "2028": "\u2028", "2029": "\u2029"}

// pyText walks compact JSON: inside strings it undoes Go-only escapes;
// outside, with seps, it adds Python's separator spaces.
func pyText(b []byte, seps bool) []byte {
	out := make([]byte, 0, len(b)+len(b)/8)
	inStr := false
	for i := 0; i < len(b); i++ {
		c := b[i]
		switch {
		case inStr && c == '\\':
			if i+5 < len(b) && b[i+1] == 'u' {
				if r, ok := unescaped[strings.ToLower(string(b[i+2:i+6]))]; ok {
					out = append(out, r...)
					i += 5
					continue
				}
			}
			out = append(out, c)
			if i+1 < len(b) {
				i++
				out = append(out, b[i])
			}
		case inStr && c == '"':
			inStr = false
			out = append(out, c)
		case inStr:
			out = append(out, c)
		case c == '"':
			inStr = true
			out = append(out, c)
		case seps && (c == ',' || c == ':'):
			out = append(out, c, ' ')
		default:
			out = append(out, c)
		}
	}
	return out
}

// DecodeObj parses a JSON object keeping key order; values stay raw.
func DecodeObj(raw []byte) (Obj, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, false
	}
	o := Obj{}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, false
		}
		k, _ := t.(string)
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, false
		}
		o.Set(k, v)
	}
	return o, true
}

// Rows turns SQL rows into ordered objects (Python's dict(sqlite3.Row)):
// columns in SELECT order, INTEGER → int64, REAL → F, TEXT → string.
func Rows(rows *sql.Rows) ([]Obj, error) {
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	out := []Obj{}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		o := make(Obj, len(cols))
		for i, c := range cols {
			v := vals[i]
			switch x := v.(type) {
			case []byte:
				v = string(x)
			case float64:
				v = F(x)
			}
			o[i] = KV{c, v}
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// F is a Python float: it serialises as repr() does, so an integral value
// keeps its ".0" (Go's encoder would print 100.0 as 100).
type F float64

func (f F) MarshalJSON() ([]byte, error) { return []byte(Repr(float64(f))), nil }

// Fp is an optional F.
func Fp(p *float64) *F {
	if p == nil {
		return nil
	}
	f := F(*p)
	return &f
}

// PyNum is a JSON number as Python's json.loads typed it: int64 unless the
// literal has a fraction or exponent, then F.
func PyNum(n json.Number) any {
	if !strings.ContainsAny(string(n), ".eE") {
		if i, err := n.Int64(); err == nil {
			return i
		}
	}
	f, _ := n.Float64()
	return F(f)
}

// Float reads a number of any decoded shape (float64, F, int64, int,
// json.Number).
func Float(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case F:
		return float64(x), true
	case int64:
		return float64(x), true
	case int:
		return float64(x), true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	}
	return 0, false
}

// Repr is Python's repr(float): shortest round-trip digits, positional when
// the decimal exponent is in [-4, 16), scientific ("1e-05", "1e+16")
// otherwise, and a trailing ".0" when a positional value is integral.
func Repr(x float64) string {
	switch {
	case math.IsNaN(x):
		return "nan"
	case math.IsInf(x, 1):
		return "inf"
	case math.IsInf(x, -1):
		return "-inf"
	}
	e := strconv.FormatFloat(x, 'e', -1, 64) // d.ddde±XX
	i := strings.LastIndexByte(e, 'e')
	exp, _ := strconv.Atoi(e[i+1:])
	if x != 0 && (exp < -4 || exp >= 16) {
		mant := e[:i]
		sign := "+"
		if exp < 0 {
			sign, exp = "-", -exp
		}
		return fmt.Sprintf("%se%s%02d", mant, sign, exp)
	}
	s := strconv.FormatFloat(x, 'f', -1, 64)
	if !strings.Contains(s, ".") {
		s += ".0"
	}
	return s
}

// Str is Python's str() for a decoded JSON scalar (UseNumber not assumed).
func Str(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case string:
		return x
	case bool:
		if x {
			return "True"
		}
		return "False"
	case json.Number:
		return x.String()
	case float64:
		return Repr(x)
	case F:
		return Repr(float64(x))
	case int64:
		return strconv.FormatInt(x, 10)
	case int:
		return strconv.Itoa(x)
	}
	b, _ := compact(v)
	return string(b)
}

// Truncate is Python's s[:n] (code points, not bytes).
func Truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// O builds an ordered object from alternating keys and values.
func O(kv ...any) Obj {
	o := make(Obj, 0, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		o = append(o, KV{Key: kv[i].(string), Val: kv[i+1]})
	}
	return o
}
