// Package sqlgen turns a parsed query and a schema relation into a
// parameterized SQL statement.
//
// Every request-derived value enters the statement as a text bind parameter
// that is then cast explicitly, and every name enters as a schema-resolved,
// quoted identifier. No request text is ever spliced into the SQL string.
package sqlgen

import (
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Builder accumulates SQL text and bind arguments.
type Builder struct {
	sb   strings.Builder
	args []any
}

func (b *Builder) Write(s string) { b.sb.WriteString(s) }
func (b *Builder) String() string { return b.sb.String() }
func (b *Builder) Args() []any    { return b.args }

// Param binds a text value and returns its placeholder. Values are always
// passed as text and cast explicitly by the caller.
func (b *Builder) Param(value string) string {
	b.args = append(b.args, value)
	return "$" + strconv.Itoa(len(b.args))
}

// Ident quotes a single identifier.
func Ident(name string) string { return pgx.Identifier{name}.Sanitize() }

// Qualified quotes a schema-qualified relation name.
func Qualified(schema, name string) string { return pgx.Identifier{schema, name}.Sanitize() }

// ArrayLiteral renders values as a PostgreSQL array literal.
func ArrayLiteral(values []string) string { return arrayLiteral(values) }

// arrayLiteral renders values as a PostgreSQL array literal. Elements are
// quoted and escaped so the literal is unambiguous regardless of content.
func arrayLiteral(values []string) string {
	var sb strings.Builder
	sb.WriteByte('{')
	for i, v := range values {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(arrayElement(v))
	}
	sb.WriteByte('}')
	return sb.String()
}

func arrayElement(v string) string {
	if v == "" {
		return `""`
	}
	needsQuote := strings.EqualFold(v, "null")
	for i := 0; i < len(v) && !needsQuote; i++ {
		switch v[i] {
		case ',', '{', '}', '"', '\\', ' ', '\t', '\n', '\r':
			needsQuote = true
		}
	}
	if !needsQuote {
		return v
	}
	var sb strings.Builder
	sb.WriteByte('"')
	for i := 0; i < len(v); i++ {
		if v[i] == '"' || v[i] == '\\' {
			sb.WriteByte('\\')
		}
		sb.WriteByte(v[i])
	}
	sb.WriteByte('"')
	return sb.String()
}

// multiwordCasts are type names that cannot be quoted as a single identifier.
var multiwordCasts = map[string]bool{
	"double precision":            true,
	"character varying":           true,
	"bit varying":                 true,
	"timestamp with time zone":    true,
	"timestamp without time zone": true,
	"time with time zone":         true,
	"time without time zone":      true,
}

// validCast accepts only a plain, optionally schema-qualified type name with an
// optional length specification and array suffix. Anything else risks escaping
// the cast position.
func validCast(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	if strings.HasSuffix(s, ")") {
		open := strings.LastIndexByte(s, '(')
		if open < 0 {
			return false
		}
		if !validLengthSpec(s[open+1 : len(s)-1]) {
			return false
		}
		s = strings.TrimSpace(s[:open])
	}
	if strings.HasSuffix(s, "[]") {
		s = strings.TrimSpace(strings.TrimSuffix(s, "[]"))
	}
	if multiwordCasts[strings.ToLower(s)] {
		return true
	}
	return isTypeIdent(s)
}

func validLengthSpec(s string) bool {
	precision, scale, hasScale := strings.Cut(s, ",")
	if !isDigits(precision) {
		return false
	}
	if !hasScale {
		return true
	}
	return isDigits(scale)
}

func isTypeIdent(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) == 0 || len(parts) > 2 {
		return false
	}
	for _, part := range parts {
		if !isIdent(part) {
			return false
		}
	}
	return true
}

func isIdent(s string) bool {
	if s == "" || !isAlpha(s[0]) && s[0] != '_' {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if !isAlpha(c) && !isDigitsByte(c) && c != '_' {
			return false
		}
	}
	return true
}

func isAlpha(c byte) bool      { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
func isDigitsByte(c byte) bool { return c >= '0' && c <= '9' }
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isDigitsByte(s[i]) {
			return false
		}
	}
	return true
}
