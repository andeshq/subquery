package query

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/andeshq/subquery/internal/pgrst"
)

// findEmbed returns the embed an embed chain segment refers to. Both the alias
// and the relation name are accepted.
func findEmbed(items []SelectItem, key string) *Embed {
	for i := range items {
		embed := items[i].Embed
		if embed == nil {
			continue
		}
		if embed.Key() == key || embed.Name == key {
			return embed
		}
	}
	return nil
}

// applyNullFilter recognizes `is.null` / `not.is.null` on an embed.
func applyNullFilter(embed *Embed, raw string) bool {
	value := strings.TrimSpace(raw)
	negated := false
	if rest, ok := trimPrefixFold(value, "not."); ok {
		negated = true
		value = rest
	}
	if !strings.EqualFold(value, "is.null") {
		return false
	}
	if negated {
		embed.Null = NullInner
	} else {
		embed.Null = NullAnti
	}
	return true
}

// distribute routes a dotted query parameter such as
// element_name.order or actors.first_name into the embed tree.
func distribute(items []SelectItem, key, raw string) error {
	parts := strings.Split(key, ".")
	var walk func(items []SelectItem, parts []string) error
	walk = func(items []SelectItem, parts []string) error {
		embed := findEmbed(items, parts[0])
		if embed == nil {
			return &Error{
				Code:    pgrst.EmbedNotFound,
				Param:   parts[0],
				Message: fmt.Sprintf("Could not find the embedded resource %q in the select list", parts[0]),
			}
		}
		if len(parts) == 2 {
			return applyEmbedParam(embed, parts[1], raw)
		}
		return walk(embed.Select, parts[1:])
	}
	return walk(items, parts)
}

func applyEmbedParam(embed *Embed, param, raw string) error {
	switch param {
	case "order":
		terms, err := parseOrder(raw)
		if err != nil {
			return err
		}
		embed.Order = terms
	case "limit":
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 0 {
			return errf("limit", "failed to parse embedded limit %q", raw)
		}
		embed.Limit = new(n)
	case "offset":
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 0 {
			return errf("offset", "failed to parse embedded offset %q", raw)
		}
		embed.Offset = n
	case "or":
		f, err := parseLogicalList("or", raw, false)
		if err != nil {
			return err
		}
		embed.Where = append(embed.Where, f)
	case "and":
		f, err := parseLogicalList("and", raw, false)
		if err != nil {
			return err
		}
		embed.Where = append(embed.Where, f)
	case "not.or":
		f, err := parseLogicalList("or", raw, true)
		if err != nil {
			return err
		}
		embed.Where = append(embed.Where, f)
	case "not.and":
		f, err := parseLogicalList("and", raw, true)
		if err != nil {
			return err
		}
		embed.Where = append(embed.Where, f)
	default:
		f, err := parseComparison(param, raw)
		if err != nil {
			return err
		}
		embed.Where = append(embed.Where, f)
	}
	return nil
}
