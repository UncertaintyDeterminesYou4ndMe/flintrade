package pj

import (
	"encoding/json"
	"os"
	"strconv"
	"testing"
)

func TestDumpsMatchesPython(t *testing.T) {
	o := Obj{{"b", 1}, {"a", []any{"x<y", "中文"}}, {"c", Obj{{"k", "a, b: c \"q\""}}}}
	got, _ := Dumps(o)
	// python3 -c 'import json;print(json.dumps({"b":1,"a":["x<y","中文"],"c":{"k":"a, b: c \"q\""}},ensure_ascii=False))'
	want := `{"b": 1, "a": ["x<y", "中文"], "c": {"k": "a, b: c \"q\""}}`
	if got != want {
		t.Fatalf("\n got %s\nwant %s", got, want)
	}
	if Repr(5) != "5.0" || Repr(-440.12) != "-440.12" || Repr(pt1()+0.2) != "0.30000000000000004" {
		t.Fatal("Repr")
	}
	if Truncate("中文abc", 2) != "中文" {
		t.Fatal("Truncate counts runes")
	}
	o2, ok := DecodeObj([]byte(`{"z":1,"_id":2,"a":{"q":1}}`))
	o2.Set("_id", "x")
	o2.Set("n", 3)
	if b, _ := Compact(o2); !ok || string(b) != `{"z":1,"_id":"x","a":{"q":1},"n":3}` {
		t.Fatalf("order: %s", b)
	}
}
func pt1() float64 { return 0.1 } // a variable: constant 0.1+0.2 is exact in Go

// Repr against Python repr() over the exponent boundaries (golden from
// scratchpad repr_golden.py; floats carried as hex so nothing is re-rounded).
func TestReprGolden(t *testing.T) {
	raw, err := os.ReadFile("testdata/repr_golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases [][2]string
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		x, err := strconv.ParseFloat(c[0], 64)
		if err != nil {
			t.Fatal(err)
		}
		if got := Repr(x); got != c[1] {
			t.Errorf("Repr(%s) = %s, want %s", c[0], got, c[1])
		}
	}
}

// HTML escapes inside upstream raw JSON are undone (Python prints the
// characters), while an escaped backslash before "u003c" stays literal.
func TestPyTextUnescape(t *testing.T) {
	bs := `\` // one backslash
	in := `{"n":"` + bs + "u003cp" + bs + "u003e " + bs + "u0026 x" + bs + bs + "u003c " + bs + `u2028"}`
	o := O("a", json.RawMessage(in), "b", "<ok>")
	got, _ := Dumps(o)
	want := `{"a": {"n": "<p> & x` + bs + bs + "u003c " + string(rune(0x2028)) + `"}, "b": "<ok>"}`
	if got != want {
		t.Fatalf("\n got %q\nwant %q", got, want)
	}
}
