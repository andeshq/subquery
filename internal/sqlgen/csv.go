package sqlgen

import (
	"fmt"
	"strings"

	"github.com/andeshq/subquery/internal/query"
	"github.com/andeshq/subquery/internal/schema"
)

// CSVStatement is generated SQL whose columns are the selected ones rendered as
// text, together with the header row the caller writes.
type CSVStatement struct {
	SQL     string
	Args    []any
	Columns []string
	// CountSQL counts the matching rows before limit/offset. It is set only when
	// the query asked for a count, and reuses the same bind arguments as SQL.
	CountSQL string
}

// ReadCSV generates a statement that renders each selected column as text, so
// the caller can emit CSV. Only plain columns, aliases, and casts are supported;
// an embedding, an aggregate, or a JSON path reports UnsupportedError, because
// the header would not be a plain column name.
func ReadCSV(snap *schema.Snapshot, rel *schema.Relation, q *query.Query, opts ReadOptions) (*CSVStatement, error) {
	g := &generator{b: &Builder{}, snap: snap, opts: opts}
	sc := &scope{rel: rel, alias: "pgrst_src", items: q.Select}

	cols, err := g.csvColumns(sc)
	if err != nil {
		return nil, err
	}
	where, err := g.where(sc, q.Where)
	if err != nil {
		return nil, err
	}
	order, err := g.order(sc, q.Order)
	if err != nil {
		return nil, err
	}
	limit, offset := g.limits(q)

	relation := opts.From
	if relation == "" {
		relation = Qualified(rel.ID.Schema, rel.ID.Name)
	}

	exprs := make([]string, 0, len(cols))
	names := make([]string, 0, len(cols))
	for _, col := range cols {
		exprs = append(exprs, "("+col.expr+")::text as "+Ident(col.name))
		names = append(names, col.name)
	}

	var sb strings.Builder
	sb.WriteString("select " + strings.Join(exprs, ", ") + "\n  from " + relation + " as " + sc.alias)
	if where != "" {
		sb.WriteString("\n  where " + where)
	}
	if order != "" {
		sb.WriteString("\n  order by " + order)
	}
	sb.WriteString(limitOffsetClause("  ", limit, offset))

	statement := &CSVStatement{SQL: sb.String(), Args: g.b.Args(), Columns: names}
	if q.Count != query.CountNone {
		var cb strings.Builder
		cb.WriteString("select count(*)\n  from " + relation + " as " + sc.alias)
		if where != "" {
			cb.WriteString("\n  where " + where)
		}
		statement.CountSQL = cb.String()
	}
	return statement, nil
}

// csvColumn is one CSV output column: its expression and its header name.
type csvColumn struct {
	expr string
	name string
}

// csvColumns lists the CSV projection. Every column is named explicitly, because
// the name becomes the CSV header.
func (g *generator) csvColumns(sc *scope) ([]csvColumn, error) {
	var cols []csvColumn
	appendAll := func() {
		for _, col := range sc.rel.Columns {
			cols = append(cols, csvColumn{expr: sc.alias + "." + Ident(col.Name), name: col.Name})
		}
	}
	if len(sc.items) == 0 {
		appendAll()
		return cols, nil
	}
	for _, item := range sc.items {
		switch {
		case item.Star:
			appendAll()
		case item.Embed != nil || item.Aggregate != nil || len(item.Path) > 0:
			return nil, &UnsupportedError{Feature: "CSV output for embedded, aggregate, or JSON-path selections"}
		default:
			ref := g.resolveField(sc, item.Column)
			if !ref.found {
				return nil, fieldError(item.Column)
			}
			expr := ref.expr
			if item.Cast != "" {
				if !validCast(item.Cast) {
					return nil, &FieldError{Column: item.Column, Message: fmt.Sprintf("Invalid cast %q", item.Cast)}
				}
				expr = "(" + expr + ")::" + item.Cast
			}
			name := item.Column
			if item.Alias != "" {
				name = item.Alias
			}
			cols = append(cols, csvColumn{expr: expr, name: name})
		}
	}
	return cols, nil
}
