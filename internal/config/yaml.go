package config

import (
	"fmt"
	"strings"
)

// This file is a reader for the subset of YAML that renv's configuration uses:
// nested block mappings, block sequences of scalars, quoted and unquoted
// scalars, and comments. It exists because the tool ships with no third-party
// dependencies and the standard library has no YAML support.
//
// It is deliberately strict. Anything outside the subset — tabs, flow syntax,
// nested structures inside a sequence, a misaligned block — is an error rather
// than a best-effort interpretation. A configuration file that silently parses
// into something other than what it looks like is how a sync tool ends up
// pointed at the wrong service.

type nodeKind int

const (
	scalarNode nodeKind = iota
	mappingNode
	sequenceNode
)

type node struct {
	kind nodeKind
	line int

	str  string
	seq  []*node
	keys []string
	m    map[string]*node
}

func (n *node) set(key string, child *node) {
	if n.m == nil {
		n.m = map[string]*node{}
	}
	if _, exists := n.m[key]; !exists {
		n.keys = append(n.keys, key)
	}
	n.m[key] = child
}

// child returns the mapping entry for key.
func (n *node) child(key string) (*node, bool) {
	if n == nil || n.kind != mappingNode {
		return nil, false
	}
	c, ok := n.m[key]
	return c, ok
}

// yamlError reports a problem at a specific line.
type yamlError struct {
	Line int
	Msg  string
}

func (e *yamlError) Error() string { return fmt.Sprintf("line %d: %s", e.Line, e.Msg) }

type rawLine struct {
	indent int
	text   string
	num    int
}

type parser struct {
	lines []rawLine
	pos   int
}

// parseYAML reads the supported subset into a node tree.
func parseYAML(src []byte) (*node, error) {
	p := &parser{}
	for i, raw := range strings.Split(string(src), "\n") {
		num := i + 1
		if strings.ContainsRune(raw, '\t') {
			return nil, &yamlError{Line: num, Msg: "tab indentation is not supported; use spaces"}
		}
		trimmed := strings.TrimLeft(raw, " ")
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		p.lines = append(p.lines, rawLine{
			indent: len(raw) - len(trimmed),
			text:   strings.TrimRight(trimmed, " "),
			num:    num,
		})
	}
	if len(p.lines) == 0 {
		return &node{kind: mappingNode, line: 1}, nil
	}
	if p.lines[0].indent != 0 {
		return nil, &yamlError{Line: p.lines[0].num, Msg: "document must start at column 0"}
	}

	n, err := p.parseBlock(0)
	if err != nil {
		return nil, err
	}
	if p.pos != len(p.lines) {
		return nil, &yamlError{Line: p.lines[p.pos].num, Msg: "unexpected indentation"}
	}
	return n, nil
}

func (p *parser) parseBlock(indent int) (*node, error) {
	if strings.HasPrefix(p.lines[p.pos].text, "- ") || p.lines[p.pos].text == "-" {
		return p.parseSequence(indent)
	}
	return p.parseMapping(indent)
}

func (p *parser) parseSequence(indent int) (*node, error) {
	n := &node{kind: sequenceNode, line: p.lines[p.pos].num}
	for p.pos < len(p.lines) {
		l := p.lines[p.pos]
		if l.indent < indent {
			break
		}
		if l.indent > indent {
			return nil, &yamlError{Line: l.num, Msg: "unexpected indentation inside a list"}
		}
		if !strings.HasPrefix(l.text, "- ") {
			if l.text == "-" {
				return nil, &yamlError{Line: l.num, Msg: "list items must be scalars"}
			}
			break
		}
		item := strings.TrimSpace(strings.TrimPrefix(l.text, "- "))
		value, err := scalarValue(item, l.num)
		if err != nil {
			return nil, err
		}
		if strings.Contains(value, ":") && !strings.Contains(item, "://") {
			return nil, &yamlError{Line: l.num, Msg: "nested mappings inside lists are not supported"}
		}
		n.seq = append(n.seq, &node{kind: scalarNode, str: value, line: l.num})
		p.pos++
	}
	return n, nil
}

func (p *parser) parseMapping(indent int) (*node, error) {
	n := &node{kind: mappingNode, line: p.lines[p.pos].num}
	for p.pos < len(p.lines) {
		l := p.lines[p.pos]
		if l.indent < indent {
			break
		}
		if l.indent > indent {
			return nil, &yamlError{Line: l.num, Msg: "unexpected indentation"}
		}
		if strings.HasPrefix(l.text, "- ") {
			return nil, &yamlError{Line: l.num, Msg: "list item where a mapping key was expected"}
		}

		key, rest, ok := splitKey(l.text)
		if !ok {
			// The line is not quoted back: a rejected line is unvalidated
			// content, and an error message is the wrong place to discover
			// what it held.
			return nil, &yamlError{Line: l.num, Msg: `expected "key: value"`}
		}
		if _, dup := n.m[key]; dup {
			return nil, &yamlError{Line: l.num, Msg: fmt.Sprintf("duplicate key %q", key)}
		}
		p.pos++

		if rest != "" {
			value, err := scalarValue(rest, l.num)
			if err != nil {
				return nil, err
			}
			n.set(key, &node{kind: scalarNode, str: value, line: l.num})
			continue
		}

		// A bare "key:" introduces a nested block, unless nothing is
		// indented under it, in which case it is an empty value.
		if p.pos < len(p.lines) && p.lines[p.pos].indent > indent {
			child, err := p.parseBlock(p.lines[p.pos].indent)
			if err != nil {
				return nil, err
			}
			n.set(key, child)
			continue
		}
		n.set(key, &node{kind: scalarNode, str: "", line: l.num})
	}
	return n, nil
}

// splitKey splits "key: value" on the first colon that is followed by a space
// or ends the line, so that values containing colons — URLs, most obviously —
// survive intact.
func splitKey(s string) (key, rest string, ok bool) {
	for i := 0; i < len(s); i++ {
		if s[i] != ':' {
			continue
		}
		if i == 0 {
			return "", "", false
		}
		if i == len(s)-1 {
			return s[:i], "", true
		}
		if s[i+1] == ' ' {
			return s[:i], strings.TrimSpace(s[i+2:]), true
		}
	}
	return "", "", false
}

// scalarValue strips an inline comment and one layer of quotes.
func scalarValue(s string, line int) (string, error) {
	if s == "" {
		return "", nil
	}
	if q := s[0]; q == '"' || q == '\'' {
		end := strings.IndexByte(s[1:], q)
		if end < 0 {
			return "", &yamlError{Line: line, Msg: "unterminated quoted value"}
		}
		return s[1 : end+1], nil
	}
	if i := strings.Index(s, " #"); i >= 0 {
		s = strings.TrimRight(s[:i], " ")
	}
	return s, nil
}
