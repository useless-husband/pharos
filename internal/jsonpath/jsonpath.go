// Package jsonpath evaluates the small path language used in HTTP body
// assertions: dot-separated keys with optional array indexes, e.g.
// "data.items[0].state" or "[2].id". A leading "$." is accepted.
package jsonpath

import (
	"fmt"
	"strconv"
	"strings"
)

// Step is one segment of a path: an object key or an array index.
type Step struct {
	Key   string
	Index int
	IsIdx bool
}

// Path is a parsed path.
type Path []Step

func (p Path) String() string {
	var b strings.Builder
	for i, s := range p {
		if s.IsIdx {
			fmt.Fprintf(&b, "[%d]", s.Index)
			continue
		}
		if i > 0 {
			b.WriteByte('.')
		}
		b.WriteString(s.Key)
	}
	return b.String()
}

// Parse parses a path expression.
func Parse(expr string) (Path, error) {
	s := strings.TrimSpace(expr)
	s = strings.TrimPrefix(s, "$")
	s = strings.TrimPrefix(s, ".")
	if s == "" {
		return nil, fmt.Errorf("json path %q is empty", expr)
	}
	var p Path
	i := 0
	for i < len(s) {
		switch s[i] {
		case '.':
			i++
			if i >= len(s) || s[i] == '.' || s[i] == '[' {
				return nil, fmt.Errorf("json path %q: empty key at position %d", expr, i)
			}
		case '[':
			end := strings.IndexByte(s[i:], ']')
			if end < 0 {
				return nil, fmt.Errorf("json path %q: missing ]", expr)
			}
			inner := s[i+1 : i+end]
			if len(inner) >= 2 && (inner[0] == '"' || inner[0] == '\'') && inner[len(inner)-1] == inner[0] {
				p = append(p, Step{Key: inner[1 : len(inner)-1]})
			} else {
				n, err := strconv.Atoi(inner)
				if err != nil || n < 0 {
					return nil, fmt.Errorf("json path %q: invalid index [%s]", expr, inner)
				}
				p = append(p, Step{Index: n, IsIdx: true})
			}
			i += end + 1
		default:
			j := i
			for j < len(s) && s[j] != '.' && s[j] != '[' {
				j++
			}
			p = append(p, Step{Key: s[i:j]})
			i = j
		}
	}
	return p, nil
}

// Lookup resolves the path against a value decoded by encoding/json.
func (p Path) Lookup(v any) (any, bool) {
	cur := v
	for _, s := range p {
		if s.IsIdx {
			arr, ok := cur.([]any)
			if !ok || s.Index >= len(arr) {
				return nil, false
			}
			cur = arr[s.Index]
			continue
		}
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = obj[s.Key]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// Equal compares a decoded JSON value with an expected value from YAML.
// Numbers compare numerically, so `equals: 1` matches 1.0 in the body.
func Equal(got, want any) bool {
	switch w := want.(type) {
	case nil:
		return got == nil
	case bool:
		g, ok := got.(bool)
		return ok && g == w
	case string:
		g, ok := got.(string)
		return ok && g == w
	case int:
		g, ok := got.(float64)
		return ok && g == float64(w)
	case int64:
		g, ok := got.(float64)
		return ok && g == float64(w)
	case uint64:
		g, ok := got.(float64)
		return ok && g == float64(w)
	case float64:
		g, ok := got.(float64)
		return ok && g == w
	}
	return fmt.Sprint(got) == fmt.Sprint(want)
}
