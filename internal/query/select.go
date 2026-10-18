package query

import "strings"

// JSONStep is one -> or ->> traversal in a select or filter.
type JSONStep struct {
	Key string
	// Text reports whether the step uses ->> (text extraction).
	Text bool
	// Numeric reports whether Key is an array index.
	Numeric bool
}

// NullFilter marks an embed referenced by an is.null / not.is.null filter.
type NullFilter int

const (
	NullNone NullFilter = iota
	// NullInner corresponds to `embed=not.is.null`: only parents that have at
	// least one related row.
	NullInner
	// NullAnti corresponds to `embed=is.null`: only parents with no related
	// rows.
	NullAnti
)

// Embed is a related resource requested inside a select list.
type Embed struct {
	// Name is the requested relation or relationship name.
	Name string
	// Alias is the output key, when it differs from Name.
	Alias string
	// Hint is the !<hint> foreign key disambiguator.
	Hint string
	// Inner reports !inner: the embed filters the parent rows.
	Inner bool
	// Left reports an explicit !left, which is already the default.
	Left bool
	// Spread reports the ...spread modifier.
	Spread bool
	// Null is set when the embed is referenced by an is.null filter.
	Null NullFilter

	Select []SelectItem
	Where  []Filter
	Order  []OrderTerm
	Limit  *int64
	Offset int64
}

// Key is the name query parameters refer to the embed by.
func (e *Embed) Key() string {
	if e.Alias != "" {
		return e.Alias
	}
	return e.Name
}

// Aggregate is a count()/sum()/avg()/min()/max() projection.
type Aggregate struct {
	Fn     string
	Column string
	Path   []JSONStep
	Alias  string
}

// SelectItem is one entry of the select list.
type SelectItem struct {
	Star      bool
	Column    string
	Path      []JSONStep
	Cast      string
	Alias     string
	Embed     *Embed
	Aggregate *Aggregate
}

// HasAggregate reports whether any select item is an aggregate.
func (q *Query) HasAggregate() bool {
	for _, item := range q.Select {
		if item.Aggregate != nil {
			return true
		}
	}
	return false
}

// aggregateFunctions are the recognized aggregate names.
var aggregateFunctions = map[string]bool{
	"count": true,
	"sum":   true,
	"avg":   true,
	"min":   true,
	"max":   true,
}

func parseSelect(s string) ([]SelectItem, error) {
	parts := splitTop(s, ',')
	items := make([]SelectItem, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if part == "*" {
			items = append(items, SelectItem{Star: true})
			continue
		}
		item, err := parseSelectItem(part)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

func parseSelectItem(s string) (SelectItem, error) {
	spread := false
	if rest, ok := strings.CutPrefix(s, "..."); ok {
		spread = true
		s = strings.TrimSpace(rest)
	}

	alias := ""
	if a, rest, ok := splitAlias(s); ok {
		alias = a
		s = rest
	}

	if open := indexTopParen(s); open >= 0 {
		return parseEmbedItem(s, open, alias, spread)
	}
	if spread {
		return SelectItem{}, errf("select", "spread requires an embedded resource")
	}

	item := SelectItem{Alias: alias}
	if i := indexUnquoted(s, "::"); i >= 0 {
		item.Cast = strings.TrimSpace(s[i+2:])
		s = s[:i]
	}
	column, path, err := parseColumnPath(s)
	if err != nil {
		return SelectItem{}, err
	}
	item.Column = column
	item.Path = path
	return item, nil
}

func parseEmbedItem(s string, open int, alias string, spread bool) (SelectItem, error) {
	if !strings.HasSuffix(s, ")") {
		return SelectItem{}, errf("select", "unbalanced parenthesis in %q", s)
	}
	head := strings.TrimSpace(s[:open])
	inner := s[open+1 : len(s)-1]
	if head == "" {
		return SelectItem{}, errf("select", "missing embedded resource name in %q", s)
	}

	// A bare aggregate name in call position is an aggregate, not an embed.
	if !spread && !strings.Contains(head, "!") && aggregateFunctions[strings.ToLower(head)] {
		return parseAggregateItem(head, inner, alias)
	}

	embed := &Embed{Alias: alias, Spread: spread}
	parts := strings.Split(head, "!")
	embed.Name = strings.TrimSpace(parts[0])
	if embed.Name == "" {
		return SelectItem{}, errf("select", "missing embedded resource name in %q", s)
	}
	for _, modifier := range parts[1:] {
		switch strings.ToLower(strings.TrimSpace(modifier)) {
		case "inner":
			embed.Inner = true
		case "left":
			embed.Left = true
		case "":
			return SelectItem{}, errf("select", "empty modifier in %q", s)
		default:
			embed.Hint = strings.TrimSpace(modifier)
		}
	}
	if strings.TrimSpace(inner) != "" {
		children, err := parseSelect(inner)
		if err != nil {
			return SelectItem{}, err
		}
		embed.Select = children
	}
	return SelectItem{Embed: embed}, nil
}

// parseAggregateItem parses `count()`, `count(col)`, or `sum(col)`.
func parseAggregateItem(fnName, inner, alias string) (SelectItem, error) {
	agg := &Aggregate{Fn: strings.ToLower(fnName), Alias: alias}
	switch inner = strings.TrimSpace(inner); inner {
	case "", "*":
		if agg.Fn != "count" {
			return SelectItem{}, errf("select", "%s() requires a column argument", agg.Fn)
		}
	default:
		column, path, err := parseColumnPath(inner)
		if err != nil {
			return SelectItem{}, err
		}
		agg.Column = column
		agg.Path = path
	}
	return SelectItem{Aggregate: agg}, nil
}

// indexTopParen returns the index of the first parenthesis outside quotes.
func indexTopParen(s string) int {
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
		case c == '(':
			return i
		}
	}
	return -1
}

// splitAlias splits `alias:expression`, ignoring the "::" used by casts and
// colons nested inside an embedded resource's parentheses.
func splitAlias(s string) (string, string, bool) {
	quote := byte(0)
	depth := 0
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
		case c == ':' && depth == 0:
			if i+1 < len(s) && s[i+1] == ':' {
				i++
				continue
			}
			alias := unquoteValue(s[:i])
			if alias == "" {
				return "", "", false
			}
			return alias, s[i+1:], true
		}
	}
	return "", "", false
}

// indexUnquoted returns the first index of sub outside quotes.
func indexUnquoted(s, sub string) int {
	quote := byte(0)
	for i := 0; i+len(sub) <= len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"':
			quote = c
		case strings.HasPrefix(s[i:], sub):
			return i
		}
	}
	return -1
}

// parseColumnPath splits a column reference from its ->/->> traversals.
func parseColumnPath(s string) (string, []JSONStep, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil, errf("", "empty column reference")
	}
	idx := strings.Index(s, "->")
	if idx < 0 {
		return unquoteValue(s), nil, nil
	}
	column := unquoteValue(s[:idx])
	rest := s[idx:]
	var steps []JSONStep

	for rest != "" {
		var text bool
		switch {
		case strings.HasPrefix(rest, "->>"):
			text = true
			rest = rest[3:]
		case strings.HasPrefix(rest, "->"):
			rest = rest[2:]
		default:
			return "", nil, errf(column, "malformed JSON path in %q", s)
		}
		next := strings.Index(rest, "->")
		key := rest
		if next >= 0 {
			key = rest[:next]
			rest = rest[next:]
		} else {
			rest = ""
		}
		key = unquoteValue(strings.TrimSpace(key))
		if key == "" {
			return "", nil, errf(column, "empty JSON path key in %q", s)
		}
		steps = append(steps, JSONStep{Key: key, Text: text, Numeric: isDigits(key)})
	}
	return column, steps, nil
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
