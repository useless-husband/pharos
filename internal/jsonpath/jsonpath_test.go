package jsonpath

import (
	"encoding/json"
	"testing"
)

func TestParseAndLookup(t *testing.T) {
	var doc any
	_ = json.Unmarshal([]byte(`{"status":"ok","data":{"items":[{"state":"ready"},{"state":"busy","n":2}]},"odd key":true}`), &doc)
	cases := []struct {
		path string
		want any
		ok   bool
	}{
		{"status", "ok", true},
		{"$.status", "ok", true},
		{"data.items[1].state", "busy", true},
		{"data.items[1].n", 2.0, true},
		{`["odd key"]`, true, true},
		{"data.items[5].state", nil, false},
		{"data.missing", nil, false},
		{"status.inner", nil, false},
	}
	for _, c := range cases {
		p, err := Parse(c.path)
		if err != nil {
			t.Fatalf("Parse(%q): %v", c.path, err)
		}
		got, ok := p.Lookup(doc)
		if ok != c.ok || (ok && !Equal(got, c.want)) {
			t.Errorf("Lookup(%q) = %v, %v; want %v, %v", c.path, got, ok, c.want, c.ok)
		}
	}
	var arr any
	_ = json.Unmarshal([]byte(`[{"id":7}]`), &arr)
	p, _ := Parse("[0].id")
	if v, ok := p.Lookup(arr); !ok || !Equal(v, 7) {
		t.Errorf("root array lookup = %v %v", v, ok)
	}
}

func TestParseErrors(t *testing.T) {
	for _, bad := range []string{"", "$", "a..b", "a[", "a[x]", "a[-1]", "a.", "a.[0]"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) should fail", bad)
		}
	}
}

func TestEqual(t *testing.T) {
	if !Equal(1.0, 1) || !Equal(true, true) || !Equal(nil, nil) || Equal("1", 1) || Equal(false, nil) {
		t.Error("Equal semantics")
	}
}

func TestString(t *testing.T) {
	p, _ := Parse("$.data.items[0].state")
	if p.String() != "data.items[0].state" {
		t.Errorf("String = %q", p.String())
	}
}
