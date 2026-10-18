package query

import (
	"strings"
)

// LogicalOp is AND or OR.
type LogicalOp int

const (
	LogicalAnd LogicalOp = iota
	LogicalOr
)

// Filter is a node in the filter tree.
type Filter interface{ filterNode() }

// Logical groups child filters with AND or OR.
type Logical struct {
	Op       LogicalOp
	Negated  bool
	Children []Filter
}

func (*Logical) filterNode() {}

// Operator is a PostgREST comparison operator.
type Operator int

const (
	OpEq Operator = iota
	OpNeq
	OpGt
	OpGte
	OpLt
	OpLte
	OpLike
	OpILike
	OpMatch
	OpIMatch
	OpIn
	OpIs
	OpIsDistinct
	OpFts
	OpPlfts
	OpPhfts
	OpWfts
	OpCs
	OpCd
	OpOv
	OpSl
	OpSr
	OpNxr
	OpNxl
	OpAdj
)

// Modifier is the any/all suffix on a comparison operator.
type Modifier int

const (
	ModNone Modifier = iota
	ModAny
	ModAll
)

// Comparison is a column or JSON-path comparison.
type Comparison struct {
	Column   string
	Path     []JSONStep
	Op       Operator
	Negated  bool
	Modifier Modifier
	Lang     string
	Values   []string
}

func (*Comparison) filterNode() {}

var operatorByName = map[string]Operator{
	"eq":         OpEq,
	"neq":        OpNeq,
	"gt":         OpGt,
	"gte":        OpGte,
	"lt":         OpLt,
	"lte":        OpLte,
	"like":       OpLike,
	"ilike":      OpILike,
	"match":      OpMatch,
	"imatch":     OpIMatch,
	"in":         OpIn,
	"is":         OpIs,
	"isdistinct": OpIsDistinct,
	"fts":        OpFts,
	"plfts":      OpPlfts,
	"phfts":      OpPhfts,
	"wfts":       OpWfts,
	"cs":         OpCs,
	"cd":         OpCd,
	"ov":         OpOv,
	"sl":         OpSl,
	"sr":         OpSr,
	"nxr":        OpNxr,
	"nxl":        OpNxl,
	"adj":        OpAdj,
}

func isFTSSearch(op Operator) bool {
	switch op {
	case OpFts, OpPlfts, OpPhfts, OpWfts:
		return true
	}
	return false
}

// parseComparison parses a top-level `column=value` parameter.
func parseComparison(columnKey, raw string) (Filter, error) {
	col, path, err := parseColumnPath(columnKey)
	if err != nil {
		return nil, err
	}
	return parseComparisonTail(col, path, raw)
}

// parseComparisonTail parses `op[.param].value` possibly prefixed with "not.".
func parseComparisonTail(col string, path []JSONStep, raw string) (Filter, error) {
	raw = strings.TrimSpace(raw)
	negated := false
	if rest, ok := trimPrefixFold(raw, "not."); ok {
		negated = true
		raw = rest
	}

	token, value, err := splitOperator(raw)
	if err != nil {
		return nil, err
	}
	op, ok := operatorByName[strings.ToLower(token.name)]
	if !ok {
		return nil, errf(col, "unknown operator %q", token.name)
	}

	comparison := &Comparison{Column: col, Path: path, Op: op, Negated: negated}
	switch {
	case isFTSSearch(op):
		comparison.Lang = token.param
	case token.param != "":
		switch strings.ToLower(token.param) {
		case "any":
			comparison.Modifier = ModAny
		case "all":
			comparison.Modifier = ModAll
		default:
			return nil, errf(col, "unknown operator modifier %q", token.param)
		}
	}

	switch {
	case op == OpIn:
		list, ok := splitList(value)
		if !ok {
			return nil, errf(col, "failed to parse in list")
		}
		comparison.Values = list
	case comparison.Modifier != ModNone:
		list, ok := splitList(value)
		if !ok {
			return nil, errf(col, "failed to parse %s list", token.name)
		}
		comparison.Values = list
	case op == OpIs || op == OpIsDistinct:
		keyword := strings.ToLower(unquoteValue(value))
		switch keyword {
		case "null", "true", "false", "unknown":
			comparison.Values = []string{keyword}
		default:
			return nil, errf(col, "failed to parse %s value %q", token.name, value)
		}
	default:
		comparison.Values = []string{unquoteValue(value)}
	}

	// Pattern operators accept * as an alias of %, including with any/all.
	if op == OpLike || op == OpILike || op == OpMatch || op == OpIMatch {
		for i, v := range comparison.Values {
			comparison.Values[i] = strings.ReplaceAll(v, "*", "%")
		}
	}
	return comparison, nil
}

// parseLogicalList parses the grouped form `(a.op.v,not.and(...),...)`.
func parseLogicalList(op string, raw string, negated bool) (Filter, error) {
	items, ok := splitList(strings.TrimSpace(raw))
	if !ok {
		return nil, errf(op, "failed to parse %s filter", op)
	}
	logical := &Logical{Op: LogicalAnd, Negated: negated}
	if strings.EqualFold(op, "or") {
		logical.Op = LogicalOr
	}
	for _, item := range items {
		child, err := parseFilterElement(strings.TrimSpace(item))
		if err != nil {
			return nil, err
		}
		logical.Children = append(logical.Children, child)
	}
	return logical, nil
}

// parseFilterElement parses one element inside a logical group.
func parseFilterElement(s string) (Filter, error) {
	if s == "" {
		return nil, errf("", "empty filter")
	}
	negated := false
	if rest, ok := trimPrefixFold(s, "not."); ok {
		negated = true
		s = rest
	}
	if inner, ok := trimPrefixFold(s, "and("); ok {
		return parseLogicalList("and", "("+strings.TrimSuffix(inner, ")")+")", negated)
	}
	if inner, ok := trimPrefixFold(s, "or("); ok {
		return parseLogicalList("or", "("+strings.TrimSuffix(inner, ")")+")", negated)
	}

	colPart, rest, err := splitColumn(s)
	if err != nil {
		return nil, err
	}
	col, path, err := parseColumnPath(colPart)
	if err != nil {
		return nil, err
	}
	filter, err := parseComparisonTail(col, path, rest)
	if err != nil {
		return nil, err
	}
	if negated {
		if comparison, ok := filter.(*Comparison); ok {
			comparison.Negated = !comparison.Negated
		}
	}
	return filter, nil
}

type opToken struct {
	name  string
	param string
}

// splitOperator reads `op`, an optional `(param)`, and the trailing value.
func splitOperator(raw string) (opToken, string, error) {
	i := 0
	for i < len(raw) && raw[i] != '.' && raw[i] != '(' {
		i++
	}
	if i == 0 {
		return opToken{}, "", errf("", "missing operator in %q", raw)
	}
	token := opToken{name: raw[:i]}
	rest := raw[i:]
	if strings.HasPrefix(rest, "(") {
		end := strings.IndexByte(rest, ')')
		if end < 0 {
			return opToken{}, "", errf(token.name, "unbalanced parenthesis in %q", raw)
		}
		token.param = rest[1:end]
		rest = rest[end+1:]
	}
	if !strings.HasPrefix(rest, ".") {
		return opToken{}, "", errf(token.name, "expected '.' before the value in %q", raw)
	}
	return token, rest[1:], nil
}

// splitColumn separates the leading column (or JSON path) from the operator.
func splitColumn(s string) (string, string, error) {
	depth := 0
	quote := byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"':
			quote = c
		case c == '(' || c == '{' || c == '[':
			depth++
		case c == ')' || c == '}' || c == ']':
			depth--
		case c == '.' && depth == 0:
			return s[:i], s[i+1:], nil
		}
	}
	return "", "", errf("", "failed to parse filter %q", s)
}
