package query

import "strings"

// OrderTerm is one entry of the order list.
type OrderTerm struct {
	Column string
	Path   []JSONStep
	Desc   bool
	Nulls  string // "", "first", or "last"
}

func parseOrder(s string) ([]OrderTerm, error) {
	parts := splitTop(s, ',')
	terms := make([]OrderTerm, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		segments := splitTop(part, '.')
		column, path, err := parseColumnPath(strings.TrimSpace(segments[0]))
		if err != nil {
			return nil, err
		}
		term := OrderTerm{Column: column, Path: path}
		for _, modifier := range segments[1:] {
			switch strings.ToLower(strings.TrimSpace(modifier)) {
			case "asc":
				term.Desc = false
			case "desc":
				term.Desc = true
			case "nullsfirst":
				term.Nulls = "first"
			case "nullslast":
				term.Nulls = "last"
			default:
				return nil, errf("order", "unknown order modifier %q", modifier)
			}
		}
		terms = append(terms, term)
	}
	return terms, nil
}
