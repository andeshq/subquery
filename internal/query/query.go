// Package query parses the PostgREST query string grammar into an AST.
//
// Parsing is schema-agnostic: it validates syntax only, leaving column
// resolution and SQL generation to internal/sqlgen.
package query

import (
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/andeshq/subquery/internal/pgrst"
)

// CountMode is the Prefer: count preference.
type CountMode int

const (
	CountNone CountMode = iota
	CountExact
	CountPlanned
	CountEstimated
)

// Query is a parsed request.
type Query struct {
	Select []SelectItem
	Where  []Filter
	Order  []OrderTerm
	Limit  *int64
	Offset int64
	Count  CountMode

	// Write-only parameters.
	Columns    []string
	OnConflict []string
}

// ParseOptions selects the parameter vocabulary.
type ParseOptions struct {
	// Write reserves columns and on_conflict for insertion/upsert.
	Write bool
}

// Error is a client-facing query error. Code defaults to PGRST100 (parse
// error) and may be a more specific code such as PGRST108.
type Error struct {
	Code    string
	Param   string
	Message string
	Details string
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func errf(param, format string, args ...any) *Error {
	return &Error{Code: pgrst.ParseError, Param: param, Message: fmt.Sprintf(format, args...)}
}

// reserved are query parameters that are not column filters.
var reserved = map[string]bool{
	"select":  true,
	"order":   true,
	"limit":   true,
	"offset":  true,
	"or":      true,
	"and":     true,
	"not.or":  true,
	"not.and": true,
}

// splitIdentList splits a comma-separated column list. Names are validated
// against the schema, then quoted, by the generator.
func splitIdentList(v string) []string {
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Parse turns a read query string into a Query.
func Parse(values url.Values) (*Query, error) {
	return ParseWith(values, ParseOptions{})
}

// ParseWith turns a query string into a Query.
func ParseWith(values url.Values, opts ParseOptions) (*Query, error) {
	q := &Query{}

	if opts.Write {
		if v := values.Get("columns"); v != "" {
			q.Columns = splitIdentList(v)
		}
		if v := values.Get("on_conflict"); v != "" {
			q.OnConflict = splitIdentList(v)
		}
	}

	if v := values.Get("select"); v != "" {
		items, err := parseSelect(v)
		if err != nil {
			return nil, err
		}
		q.Select = items
	}
	if v := values.Get("order"); v != "" {
		terms, err := parseOrder(v)
		if err != nil {
			return nil, err
		}
		q.Order = terms
	}
	if v := values.Get("limit"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return nil, errf("limit", "failed to parse limit %q", v)
		}
		q.Limit = new(n)
	}
	if v := values.Get("offset"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return nil, errf("offset", "failed to parse offset %q", v)
		}
		q.Offset = n
	}

	for _, key := range slices.Sorted(maps.Keys(values)) {
		if reserved[key] || (opts.Write && (key == "columns" || key == "on_conflict")) {
			continue
		}
		for _, raw := range values[key] {
			if strings.Contains(key, ".") {
				if err := distribute(q.Select, key, raw); err != nil {
					return nil, err
				}
				continue
			}
			// A single-segment key naming an embed may only carry the
			// existence filters is.null / not.is.null.
			if embed := findEmbed(q.Select, key); embed != nil {
				if applyNullFilter(embed, raw) {
					continue
				}
				return nil, &Error{
					Code:    pgrst.InvalidEmbedFilter,
					Param:   key,
					Message: fmt.Sprintf("An embedded resource can only be filtered using the is.null or not.is.null operators, not %q", raw),
				}
			}
			f, err := parseComparison(key, raw)
			if err != nil {
				return nil, err
			}
			q.Where = append(q.Where, f)
		}
	}

	if v := values.Get("or"); v != "" {
		f, err := parseLogicalList("or", v, false)
		if err != nil {
			return nil, err
		}
		q.Where = append(q.Where, f)
	}
	if v := values.Get("and"); v != "" {
		f, err := parseLogicalList("and", v, false)
		if err != nil {
			return nil, err
		}
		q.Where = append(q.Where, f)
	}
	if v := values.Get("not.or"); v != "" {
		f, err := parseLogicalList("or", v, true)
		if err != nil {
			return nil, err
		}
		q.Where = append(q.Where, f)
	}
	if v := values.Get("not.and"); v != "" {
		f, err := parseLogicalList("and", v, true)
		if err != nil {
			return nil, err
		}
		q.Where = append(q.Where, f)
	}

	return q, nil
}

// splitTop splits s on sep, ignoring separators inside (), {}, [], or quotes.
func splitTop(s string, sep byte) []string {
	var parts []string
	depth := 0
	quote := byte(0)
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '(' || c == '{' || c == '[':
			depth++
		case c == ')' || c == '}' || c == ']':
			depth--
		case c == sep && depth == 0:
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	parts = append(parts, s[start:])
	return parts
}

// splitList parses a parenthesised or braced list such as (1,2,"a,b") .
func splitList(s string) ([]string, bool) {
	if len(s) < 2 {
		return nil, false
	}
	open, close := s[0], s[len(s)-1]
	if (open != '(' || close != ')') && (open != '{' || close != '}') {
		return nil, false
	}
	inner := s[1 : len(s)-1]
	if inner == "" {
		return []string{}, true
	}
	items := splitTop(inner, ',')
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, unquoteValue(strings.TrimSpace(item)))
	}
	return out, true
}

// unquoteValue removes a single layer of double quotes, unescaping "" pairs.
func unquoteValue(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		inner := s[1 : len(s)-1]
		return strings.ReplaceAll(inner, `""`, `"`)
	}
	return s
}

func trimPrefixFold(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix) {
		return s[len(prefix):], true
	}
	return s, false
}
