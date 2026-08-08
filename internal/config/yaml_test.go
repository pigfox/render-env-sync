package config

import (
	"strings"
	"testing"
)

func mustParse(t *testing.T, src string) *node {
	t.Helper()
	n, err := parseYAML([]byte(src))
	if err != nil {
		t.Fatalf("parseYAML: %v", err)
	}
	return n
}

func mustFail(t *testing.T, src, want string) {
	t.Helper()
	_, err := parseYAML([]byte(src))
	if err == nil {
		t.Fatalf("expected an error for %q", src)
	}
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error = %v, want it to mention %q", err, want)
	}
}

func TestYAMLNesting(t *testing.T) {
	n := mustParse(t, `
a:
  b:
    c: value
list:
  - one
  - two
`)
	a, ok := n.child("a")
	if !ok {
		t.Fatal("missing a")
	}
	b, ok := a.child("b")
	if !ok {
		t.Fatal("missing a.b")
	}
	c, ok := b.child("c")
	if !ok {
		t.Fatal("missing a.b.c")
	}
	if c.str != "value" {
		t.Errorf("a.b.c = %q", c.str)
	}

	list, ok := n.child("list")
	if !ok || list.kind != sequenceNode {
		t.Fatal("missing list")
	}
	if len(list.seq) != 2 || list.seq[0].str != "one" || list.seq[1].str != "two" {
		t.Errorf("list = %+v", list.seq)
	}
}

// TestYAMLValuesContainingColons covers the case that a naive split on ":"
// mangles: a URL as a scalar value.
func TestYAMLValuesContainingColons(t *testing.T) {
	n := mustParse(t, "api_base: https://api.render.com/v1\nlist:\n  - https://a.example/x\n")
	v, _ := n.child("api_base")
	if v.str != "https://api.render.com/v1" {
		t.Errorf("api_base = %q", v.str)
	}
	l, _ := n.child("list")
	if l.seq[0].str != "https://a.example/x" {
		t.Errorf("list[0] = %q", l.seq[0].str)
	}
}

func TestYAMLComments(t *testing.T) {
	n := mustParse(t, `
# leading comment

key: value # trailing comment
quoted: "value # not a comment"
hashless: a#b
`)
	for _, tc := range []struct{ key, want string }{
		{"key", "value"},
		{"quoted", "value # not a comment"},
		{"hashless", "a#b"},
	} {
		v, ok := n.child(tc.key)
		if !ok {
			t.Fatalf("missing %s", tc.key)
		}
		if v.str != tc.want {
			t.Errorf("%s = %q, want %q", tc.key, v.str, tc.want)
		}
	}
}

func TestYAMLQuotes(t *testing.T) {
	n := mustParse(t, "d: \"double\"\ns: 'single'\nempty: \"\"\n")
	for _, tc := range []struct{ key, want string }{{"d", "double"}, {"s", "single"}, {"empty", ""}} {
		v, _ := n.child(tc.key)
		if v.str != tc.want {
			t.Errorf("%s = %q, want %q", tc.key, v.str, tc.want)
		}
	}
}

func TestYAMLEmptyAndBareKeys(t *testing.T) {
	n := mustParse(t, "")
	if n.kind != mappingNode || len(n.keys) != 0 {
		t.Fatalf("empty document = %+v", n)
	}

	n = mustParse(t, "a:\nb: 1\n")
	a, ok := n.child("a")
	if !ok || a.kind != scalarNode || a.str != "" {
		t.Fatalf("bare key = %+v", a)
	}

	// A bare key at the end of the document.
	n = mustParse(t, "b: 1\na:\n")
	if a, ok := n.child("a"); !ok || a.str != "" {
		t.Fatalf("trailing bare key = %+v", a)
	}
}

func TestYAMLChildOnNonMapping(t *testing.T) {
	n := mustParse(t, "list:\n  - a\n")
	l, _ := n.child("list")
	if _, ok := l.child("anything"); ok {
		t.Error("sequence returned a mapping child")
	}
	var nilNode *node
	if _, ok := nilNode.child("x"); ok {
		t.Error("nil node returned a child")
	}
}

func TestYAMLErrors(t *testing.T) {
	tests := []struct{ name, src, want string }{
		{"tab", "a:\n\tb: 1\n", "tab indentation"},
		{"leading indent", "  a: 1\n", "column 0"},
		{"no colon", "just words\n", `expected "key: value"`},
		{"colon first", ": value\n", `expected "key: value"`},
		{"duplicate key", "a: 1\na: 2\n", "duplicate key"},
		{"unterminated quote", `a: "open` + "\n", "unterminated quoted value"},
		{"over indent", "a: 1\n    b: 2\n", "unexpected indentation"},
		{"list where mapping expected", "a:\n  b: 1\n  - c\n", "list item where a mapping key"},
		{"nested map in list", "a:\n  - b: 1\n", "nested mappings inside lists"},
		{"bare dash", "a:\n  -\n", "list items must be scalars"},
		{"list over indent", "a:\n  - one\n      - two\n", "unexpected indentation inside a list"},
		{"unterminated quote in list", "a:\n  - \"open\n", "unterminated quoted value"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) { mustFail(t, tc.src, tc.want) })
	}
}

func TestYAMLErrorReportsLine(t *testing.T) {
	_, err := parseYAML([]byte("a: 1\nb: 2\njust words\n"))
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "line 3") {
		t.Errorf("error should name the line: %v", err)
	}
}

func TestYAMLTrailingContentAfterList(t *testing.T) {
	n := mustParse(t, "list:\n  - a\nafter: 1\n")
	if v, ok := n.child("after"); !ok || v.str != "1" {
		t.Fatalf("after = %+v", v)
	}
}

func TestSplitKey(t *testing.T) {
	tests := []struct {
		in        string
		key, rest string
		ok        bool
	}{
		{"a: b", "a", "b", true},
		{"a:", "a", "", true},
		{"url: https://x/y", "url", "https://x/y", true},
		{"noColon", "", "", false},
		{":leading", "", "", false},
		{"a:b", "", "", false},
	}
	for _, tc := range tests {
		key, rest, ok := splitKey(tc.in)
		if key != tc.key || rest != tc.rest || ok != tc.ok {
			t.Errorf("splitKey(%q) = %q, %q, %v; want %q, %q, %v",
				tc.in, key, rest, ok, tc.key, tc.rest, tc.ok)
		}
	}
}

func TestStringListOnUnsupportedKind(t *testing.T) {
	n := mustParse(t, "m:\n  a: 1\n")
	m, _ := n.child("m")
	if _, err := stringList(m, "m"); err == nil {
		t.Error("expected an error for a mapping where a list belongs")
	}
}

func TestYAMLTrailingUnconsumedLines(t *testing.T) {
	// A top-level sequence followed by a mapping key: the sequence parser
	// stops, and the leftover line must be an error rather than silently
	// dropped configuration.
	mustFail(t, "- a\n- b\nafter: 1\n", "unexpected indentation")

	// A mapping key at list indentation, immediately after list items.
	mustFail(t, "a:\n  - one\n  b: 2\n", "unexpected indentation")
}

func TestScalarValueDirect(t *testing.T) {
	// The empty case is unreachable through parseYAML, which only calls
	// scalarValue with a non-empty remainder, but the function is also the
	// obvious place to add a caller later.
	got, err := scalarValue("", 1)
	if err != nil || got != "" {
		t.Errorf("scalarValue(\"\") = %q, %v", got, err)
	}

	got, err = scalarValue("bare  # comment", 1)
	if err != nil || got != "bare" {
		t.Errorf("scalarValue = %q, %v", got, err)
	}

	if _, err := scalarValue(`'unterminated`, 7); err == nil {
		t.Error("expected an error for an unterminated quote")
	} else if !strings.Contains(err.Error(), "line 7") {
		t.Errorf("error should name the line: %v", err)
	}
}

func TestExpandPathWithoutHome(t *testing.T) {
	t.Setenv("HOME", "")
	if _, err := expandPath("~/x"); err == nil {
		t.Skip("home directory still resolvable on this platform")
	}
}

func TestExpandPath(t *testing.T) {
	got, err := expandPath("/absolute/path")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/absolute/path" {
		t.Errorf("expandPath = %q", got)
	}

	got, err = expandPath("~")
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(got, "~") {
		t.Errorf("bare ~ not expanded: %q", got)
	}

	got, err = expandPath("~/sub/file")
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(got, "~") || !strings.HasSuffix(got, "sub/file") {
		t.Errorf("expandPath = %q", got)
	}
}
