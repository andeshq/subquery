package sqlgen

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/andeshq/subquery/internal/query"
	"github.com/andeshq/subquery/internal/schema"
)

// ReadOptions tunes read generation.
type ReadOptions struct {
	// MaxRows caps returned rows (db-max-rows). Zero means no cap.
	MaxRows int64
	// From overrides the source expression, for example a function call.
	From string
}

// Statement is generated SQL and its bind arguments.
type Statement struct {
	SQL  string
	Args []any
}

// FieldError reports a column that does not exist.
type FieldError struct {
	Column  string
	Message string
}

func (e *FieldError) Error() string { return e.Message }

func fieldError(column string) *FieldError {
	return &FieldError{Column: column, Message: fmt.Sprintf("Could not find column %q", column)}
}

// UnsupportedError reports a feature that is not implemented yet.
type UnsupportedError struct {
	Feature string
}

func (e *UnsupportedError) Error() string { return e.Feature + " is not implemented yet" }

// Read generates a read statement returning one row: the JSON body, the number
// of returned rows, and optionally the filtered total.
func Read(snap *schema.Snapshot, rel *schema.Relation, q *query.Query, opts ReadOptions) (*Statement, error) {
	g := &generator{b: &Builder{}, snap: snap, opts: opts}
	return g.generate(rel, q)
}

type generator struct {
	b     *Builder
	snap  *schema.Snapshot
	opts  ReadOptions
	alias int
}

// scope is one SELECT level: a relation, the alias it is correlated by, and the
// items projected from it.
type scope struct {
	rel   *schema.Relation
	alias string
	items []query.SelectItem
}

func (g *generator) nextAlias(prefix string) string {
	g.alias++
	return fmt.Sprintf("%s%d", prefix, g.alias)
}

func (g *generator) generate(rel *schema.Relation, q *query.Query) (*Statement, error) {
	top := &scope{rel: rel, alias: "pgrst_src", items: q.Select}
	projection, groups, err := g.projection(top)
	if err != nil {
		return nil, err
	}
	where, err := g.where(top, q.Where)
	if err != nil {
		return nil, err
	}
	order, err := g.order(top, q.Order)
	if err != nil {
		return nil, err
	}
	groupBy := groupClause(top.items, groups)
	limit, offset := g.limits(q)

	relation := g.opts.From
	if relation == "" {
		relation = Qualified(rel.ID.Schema, rel.ID.Name)
	}
	// Projecting straight from the relation keeps the row type intact, which
	// computed fields require.
	source := relation + " as " + top.alias

	var sb strings.Builder
	sb.WriteString("with pgrst_source as (\n  select " + projection + " from " + source)
	if where != "" {
		sb.WriteString("\n  where " + where)
	}
	if groupBy != "" {
		sb.WriteString("\n  " + groupBy)
	}
	if order != "" {
		sb.WriteString("\n  order by " + order)
	}
	sb.WriteString(limitOffsetClause("  ", limit, offset))
	sb.WriteString("\n)\nselect coalesce(json_agg(row_to_json(pgrst_source)), '[]'::json)::text as body," +
		"\n       count(*) as returned")

	if q.Count != query.CountNone {
		// The total repeats the same predicate text, so it reuses the same
		// bind parameters.
		sb.WriteString(",\n       (select count(*) from (select 1 from " + source)
		if where != "" {
			sb.WriteString("\n  where " + where)
		}
		if groupBy != "" {
			sb.WriteString("\n  " + groupBy)
		}
		sb.WriteString(") _pgrst_total) as total")
	}
	sb.WriteString("\nfrom pgrst_source")
	return &Statement{SQL: sb.String(), Args: g.b.Args()}, nil
}

// groupClause adds GROUP BY when the projection contains an aggregate.
func groupClause(items []query.SelectItem, groups []string) string {
	if len(groups) == 0 || !hasAggregate(items) {
		return ""
	}
	return "group by " + strings.Join(groups, ", ")
}

func hasAggregate(items []query.SelectItem) bool {
	for _, item := range items {
		if item.Aggregate != nil {
			return true
		}
	}
	return false
}

func (g *generator) limits(q *query.Query) (*int64, int64) {
	limit := q.Limit
	if g.opts.MaxRows > 0 && (limit == nil || *limit > g.opts.MaxRows) {
		limit = new(g.opts.MaxRows)
	}
	return limit, q.Offset
}

func limitOffsetClause(indent string, limit *int64, offset int64) string {
	var sb strings.Builder
	sb.WriteString("\n" + indent)
	if limit != nil {
		sb.WriteString("limit " + strconv.FormatInt(*limit, 10))
	} else {
		sb.WriteString("limit all")
	}
	if offset > 0 {
		sb.WriteString("\n" + indent + "offset " + strconv.FormatInt(offset, 10))
	}
	return sb.String()
}

// projection renders the select list, expanding embeds into correlated
// subqueries. It also returns the expressions an aggregate projection must
// group by.
func (g *generator) projection(sc *scope) (string, []string, error) {
	var projections []string
	var groups []string
	appendAll := func() {
		for _, col := range sc.rel.Columns {
			expr := sc.alias + "." + Ident(col.Name)
			projections = append(projections, expr)
			groups = append(groups, expr)
		}
	}
	if hasAggregate(sc.items) {
		for _, item := range sc.items {
			if item.Embed != nil {
				return "", nil, &UnsupportedError{Feature: "Aggregates combined with embeddings"}
			}
		}
	}
	if len(sc.items) == 0 {
		appendAll()
		return strings.Join(projections, ", "), nil, nil
	}
	for _, item := range sc.items {
		switch {
		case item.Star:
			appendAll()
		case item.Aggregate != nil:
			expr, err := g.aggregateExpr(sc, item.Aggregate)
			if err != nil {
				return "", nil, err
			}
			alias := item.Aggregate.Alias
			if alias == "" {
				alias = item.Aggregate.Fn
			}
			projections = append(projections, expr+" as "+Ident(alias))
		case item.Embed != nil && item.Embed.Spread:
			spread, err := g.spreadExpressions(sc, item.Embed)
			if err != nil {
				return "", nil, err
			}
			projections = append(projections, spread...)
		case item.Embed != nil:
			sql, err := g.embedExpression(sc, item.Embed)
			if err != nil {
				return "", nil, err
			}
			projections = append(projections, sql+" as "+Ident(item.Embed.Key()))
		default:
			expr, err := g.columnExpr(sc, item.Column, item.Path)
			if err != nil {
				return "", nil, err
			}
			if item.Cast != "" {
				if !validCast(item.Cast) {
					return "", nil, &FieldError{Column: item.Column, Message: fmt.Sprintf("Invalid cast %q", item.Cast)}
				}
				expr = "(" + expr + ")::" + item.Cast
			}
			groupExpr := expr
			alias := item.Alias
			if alias == "" && len(item.Path) > 0 {
				alias = item.Path[len(item.Path)-1].Key
			}
			if alias != "" {
				expr += " as " + Ident(alias)
			}
			projections = append(projections, expr)
			groups = append(groups, groupExpr)
		}
	}
	return strings.Join(projections, ", "), groups, nil
}

// aggregateExpr renders one aggregate call. The function name comes from a
// fixed vocabulary.
func (g *generator) aggregateExpr(sc *scope, agg *query.Aggregate) (string, error) {
	if agg.Column == "" {
		if agg.Fn != "count" {
			return "", &FieldError{Column: agg.Fn, Message: fmt.Sprintf("%s() requires a column", agg.Fn)}
		}
		return "count(*)", nil
	}
	expr, err := g.columnExpr(sc, agg.Column, agg.Path)
	if err != nil {
		return "", err
	}
	return agg.Fn + "(" + expr + ")", nil
}

// fieldRef is a name resolved against a scope: a column or a computed field.
// found is false when the name is unknown.
type fieldRef struct {
	expr     string
	castType string
	isJSON   bool
	found    bool
}

// resolveField maps a name to a column or computed-field expression. An unknown
// name is returned quoted so PostgreSQL reports it, which is what PostgREST
// does; whether that is an error is the caller's decision (select rejects it,
// while filter and order let the database report it).
func (g *generator) resolveField(sc *scope, name string) fieldRef {
	if col, ok := sc.rel.Column(name); ok {
		return fieldRef{
			expr:     sc.alias + "." + Ident(col.Name),
			castType: col.CastType,
			isJSON:   col.IsJSON(),
			found:    true,
		}
	}
	if fn, ok := sc.rel.ComputedField(name); ok {
		return fieldRef{
			expr:     Qualified(fn.ID.Schema, fn.ID.Name) + "(" + sc.alias + ")",
			castType: fn.ScalarCast,
			found:    true,
		}
	}
	return fieldRef{expr: sc.alias + "." + Ident(name)}
}

// columnExpr builds a qualified column or computed-field reference with
// optional JSON traversal. Traversal keys are bind parameters. An unknown column
// is a FieldError, because `select` is validated against the schema cache.
func (g *generator) columnExpr(sc *scope, column string, path []query.JSONStep) (string, error) {
	ref := g.resolveField(sc, column)
	if !ref.found {
		return "", fieldError(column)
	}
	if len(path) == 0 {
		return ref.expr, nil
	}
	base := ref.expr
	if !ref.isJSON {
		base = "to_jsonb(" + ref.expr + ")"
	}
	return g.jsonPath(base, path), nil
}

// jsonPath appends each ->/->> traversal step to base. Every key is a bind
// parameter, cast to text (or int for an array index).
func (g *generator) jsonPath(base string, path []query.JSONStep) string {
	for _, step := range path {
		operator := "->"
		if step.Text {
			operator = "->>"
		}
		cast := "text"
		if step.Numeric {
			cast = "int"
		}
		base = base + " " + operator + " " + g.b.Param(step.Key) + "::" + cast
	}
	return base
}

// filterColumnExpr renders a filter operand with its bind cast type. An unknown
// name is passed through quoted, so PostgreSQL reports it.
func (g *generator) filterColumnExpr(sc *scope, column string, path []query.JSONStep) (string, string) {
	ref := g.resolveField(sc, column)
	if len(path) == 0 {
		return ref.expr, ref.castType
	}
	typeName, _ := sc.rel.FieldTypeName(column)
	isJSON := schema.IsJSONTypeName(typeName)
	base := ref.expr
	if !isJSON {
		base = "to_jsonb(" + ref.expr + ")"
	}
	return g.jsonPath(base, path), comparisonCastType("", path, isJSON)
}

// embedExpression renders one embedded resource as a correlated subquery: a
// json object for to-one relationships and a json array for to-many.
func (g *generator) embedExpression(parent *scope, embed *query.Embed) (string, error) {
	inner, relationship, wrapper, err := g.embedSubquery(parent, embed)
	if err != nil {
		return "", err
	}
	if relationship.Cardinality == schema.ToMany {
		return "coalesce((select json_agg(row_to_json(" + wrapper + ")) from (" + inner + ") " + wrapper + "), '[]'::json)", nil
	}
	return "(select row_to_json(" + wrapper + ") from (" + inner + ") " + wrapper + ")", nil
}

// spreadExpressions renders a `...spread` embed. A to-one spread lifts each
// embedded column to the parent object; a to-many spread turns each embedded
// column into a correlated array. Each column is one correlated subquery over
// the same inner SELECT, whose bind parameters are reused by placeholder.
func (g *generator) spreadExpressions(parent *scope, embed *query.Embed) ([]string, error) {
	inner, relationship, wrapper, err := g.embedSubquery(parent, embed)
	if err != nil {
		return nil, err
	}
	relationship, target := relationship, g.snap.Relations[relationship.Target]

	keys := spreadKeys(&scope{rel: target, items: embed.Select})
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		var expr string
		if relationship.Cardinality == schema.ToMany {
			expr = "coalesce((select json_agg(" + wrapper + "." + Ident(key) + ") from (" + inner + ") " + wrapper + "), '[]'::json)"
		} else {
			expr = "(select " + wrapper + "." + Ident(key) + " from (" + inner + ") " + wrapper + ")"
		}
		out = append(out, expr+" as "+Ident(key))
	}
	return out, nil
}

// embedSubquery builds the inner SELECT shared by an embed and its spreads.
func (g *generator) embedSubquery(parent *scope, embed *query.Embed) (string, *schema.Relationship, string, error) {
	relationship, schemaErr := parent.rel.ResolveEmbed(embed.Name, embed.Hint)
	if schemaErr != nil {
		return "", nil, "", schemaErr
	}
	target, ok := g.snap.Relations[relationship.Target]
	if !ok {
		return "", nil, "", &FieldError{Column: embed.Name, Message: fmt.Sprintf("Relation %s is missing from the schema cache", relationship.Target)}
	}

	child := &scope{rel: target, alias: g.nextAlias("pgrst_e"), items: embed.Select}
	from, condition, err := g.join(parent.alias, child.alias, relationship)
	if err != nil {
		return "", nil, "", err
	}
	where, err := g.where(child, embed.Where)
	if err != nil {
		return "", nil, "", err
	}
	order, err := g.order(child, embed.Order)
	if err != nil {
		return "", nil, "", err
	}
	selection, groups, err := g.projection(child)
	if err != nil {
		return "", nil, "", err
	}

	var inner strings.Builder
	inner.WriteString("select " + selection + " from " + from + " where " + condition)
	if where != "" {
		inner.WriteString(" and " + where)
	}
	if groupBy := groupClause(child.items, groups); groupBy != "" {
		inner.WriteString(" " + groupBy)
	}
	if order != "" {
		inner.WriteString(" order by " + order)
	}
	inner.WriteString(embedTail(relationship.Cardinality, embed.Limit, embed.Offset))
	return inner.String(), relationship, g.nextAlias("pgrst_r"), nil
}

// spreadKeys lists the output keys a spread contributes, matching the aliases
// projection produces.
func spreadKeys(sc *scope) []string {
	var keys []string
	if len(sc.items) == 0 {
		for _, col := range sc.rel.Columns {
			keys = append(keys, col.Name)
		}
		return keys
	}
	for _, item := range sc.items {
		switch {
		case item.Star:
			for _, col := range sc.rel.Columns {
				keys = append(keys, col.Name)
			}
		case item.Embed != nil:
			keys = append(keys, item.Embed.Key())
		case item.Alias != "":
			keys = append(keys, item.Alias)
		case len(item.Path) > 0:
			keys = append(keys, item.Path[len(item.Path)-1].Key)
		default:
			keys = append(keys, item.Column)
		}
	}
	return keys
}

func embedTail(cardinality schema.Cardinality, limit *int64, offset int64) string {
	if cardinality == schema.ToOne {
		return "\n    limit 1"
	}
	return limitOffsetClause("    ", limit, offset)
}

// join returns the FROM clause and join condition connecting a parent scope to
// a child scope through a relationship.
func (g *generator) join(parentAlias, childAlias string, r *schema.Relationship) (string, string, error) {
	if r.M2M {
		junction, ok := g.snap.Relations[r.Junction]
		if !ok {
			return "", "", fmt.Errorf("junction %s is missing from the schema cache", r.Junction)
		}
		junctionAlias := g.nextAlias("pgrst_j")
		conditions := make([]string, 0, len(r.SourceFK.Columns)+len(r.FK.Columns))
		for i, column := range r.SourceFK.Columns {
			conditions = append(conditions, junctionAlias+"."+Ident(column)+" = "+parentAlias+"."+Ident(r.SourceFK.RefCols[i]))
		}
		for i, column := range r.FK.Columns {
			conditions = append(conditions, childAlias+"."+Ident(r.FK.RefCols[i])+" = "+junctionAlias+"."+Ident(column))
		}
		from := Qualified(junction.ID.Schema, junction.ID.Name) + " as " + junctionAlias +
			", " + Qualified(r.Target.Schema, r.Target.Name) + " as " + childAlias
		return from, strings.Join(conditions, " and "), nil
	}

	conditions := make([]string, 0, len(r.FK.Columns))
	for i := range r.FK.Columns {
		if r.Forward {
			conditions = append(conditions, childAlias+"."+Ident(r.FK.RefCols[i])+" = "+parentAlias+"."+Ident(r.FK.Columns[i]))
		} else {
			conditions = append(conditions, childAlias+"."+Ident(r.FK.Columns[i])+" = "+parentAlias+"."+Ident(r.FK.RefCols[i]))
		}
	}
	from := Qualified(r.Target.Schema, r.Target.Name) + " as " + childAlias
	return from, strings.Join(conditions, " and "), nil
}

// where renders filters for a scope plus the existence conditions imposed by
// the scope's !inner and is.null embeds.
func (g *generator) where(sc *scope, filters []query.Filter) (string, error) {
	parts := make([]string, 0, len(filters)+1)
	if len(filters) > 0 {
		sql, err := g.filterList(sc, filters)
		if err != nil {
			return "", err
		}
		parts = append(parts, sql)
	}
	for _, item := range sc.items {
		if item.Embed == nil || (!item.Embed.Inner && item.Embed.Null == query.NullNone) {
			continue
		}
		sql, err := g.existence(sc, item.Embed)
		if err != nil {
			return "", err
		}
		parts = append(parts, sql)
	}
	return strings.Join(parts, " and "), nil
}

// existence renders the `exists` / `not exists` a !inner or is.null embed
// imposes on its parent.
func (g *generator) existence(parent *scope, embed *query.Embed) (string, error) {
	relationship, schemaErr := parent.rel.ResolveEmbed(embed.Name, embed.Hint)
	if schemaErr != nil {
		return "", schemaErr
	}
	target, ok := g.snap.Relations[relationship.Target]
	if !ok {
		return "", &FieldError{Column: embed.Name, Message: fmt.Sprintf("Relation %s is missing from the schema cache", relationship.Target)}
	}
	child := &scope{rel: target, alias: g.nextAlias("pgrst_i"), items: embed.Select}
	from, condition, err := g.join(parent.alias, child.alias, relationship)
	if err != nil {
		return "", err
	}
	where, err := g.where(child, embed.Where)
	if err != nil {
		return "", err
	}
	sql := "select 1 from " + from + " where " + condition
	if where != "" {
		sql += " and " + where
	}
	if embed.Null == query.NullAnti {
		return "not exists (" + sql + ")", nil
	}
	return "exists (" + sql + ")", nil
}

func (g *generator) filterList(sc *scope, filters []query.Filter) (string, error) {
	parts := make([]string, 0, len(filters))
	for _, filter := range filters {
		sql, err := g.filterSQL(sc, filter)
		if err != nil {
			return "", err
		}
		parts = append(parts, sql)
	}
	return strings.Join(parts, " and "), nil
}

// filterSQL renders one node of a filter tree. query.Filter is a closed sum
// type, so this type switch is the dispatch: a new node kind is added in
// internal/query and handled here.
func (g *generator) filterSQL(sc *scope, filter query.Filter) (string, error) {
	switch f := filter.(type) {
	case *query.Logical:
		return g.logicalSQL(sc, f)
	case *query.Comparison:
		return g.comparisonSQL(sc, f)
	default:
		return "", fmt.Errorf("unsupported filter %T", filter)
	}
}

// logicalSQL renders an AND/OR group, optionally negated.
func (g *generator) logicalSQL(sc *scope, f *query.Logical) (string, error) {
	parts := make([]string, 0, len(f.Children))
	for _, child := range f.Children {
		sql, err := g.filterSQL(sc, child)
		if err != nil {
			return "", err
		}
		parts = append(parts, sql)
	}
	joiner := " and "
	if f.Op == query.LogicalOr {
		joiner = " or "
	}
	sql := "(" + strings.Join(parts, joiner) + ")"
	if f.Negated {
		sql = "not " + sql
	}
	return sql, nil
}

func (g *generator) comparisonSQL(sc *scope, c *query.Comparison) (string, error) {
	expr, castType := g.filterColumnExpr(sc, c.Column, c.Path)
	typeName, _ := sc.rel.FieldTypeName(c.Column)

	if castType == "" {
		// The operand could not be resolved, so the cast is left to PostgreSQL,
		// which reports the unknown column.
		castType = `"pg_catalog"."text"`
	}

	var sql string
	var err error
	switch {
	case c.Op == query.OpIs || c.Op == query.OpIsDistinct:
		sql, err = g.isSQL(expr, c)
	case isPatternOp(c.Op) || c.Modifier != query.ModNone || c.Op == query.OpIn:
		sql, err = g.arrayOrPatternSQL(expr, castType, c)
	case isFTSSearchOp(c.Op):
		sql, err = g.ftsSQL(expr, typeName, c)
	default:
		symbol := scalarSymbol(c.Op)
		if symbol == "" {
			return "", &FieldError{Column: c.Column, Message: fmt.Sprintf("Unsupported operator on %q", c.Column)}
		}
		placeholder := g.b.Param(c.Values[0])
		sql = expr + " " + symbol + " (" + placeholder + "::text)::" + castType
	}
	if err != nil {
		return "", err
	}
	if c.Negated && c.Op != query.OpIs && c.Op != query.OpIsDistinct {
		sql = "not (" + sql + ")"
	}
	return sql, nil
}

// opInfo describes how one comparison operator is rendered.
type opInfo struct {
	// symbol is the scalar/array SQL operator; empty for pattern and full-text
	// operators, which have dedicated renderings.
	symbol string
	// patternSymbol is the LIKE/ILIKE/regex operator; empty when the operator is
	// not a pattern match.
	patternSymbol string
	// fts marks the full-text search operators.
	fts bool
}

// operators is the SQL vocabulary for every comparison operator. An operator
// absent from the table is unsupported and renders as empty.
var operators = map[query.Operator]opInfo{
	query.OpEq:     {symbol: "="},
	query.OpIn:     {symbol: "="},
	query.OpNeq:    {symbol: "<>"},
	query.OpGt:     {symbol: ">"},
	query.OpGte:    {symbol: ">="},
	query.OpLt:     {symbol: "<"},
	query.OpLte:    {symbol: "<="},
	query.OpCs:     {symbol: "@>"},
	query.OpCd:     {symbol: "<@"},
	query.OpOv:     {symbol: "&&"},
	query.OpSl:     {symbol: "<<"},
	query.OpSr:     {symbol: ">>"},
	query.OpNxr:    {symbol: "&<"},
	query.OpNxl:    {symbol: "&>"},
	query.OpAdj:    {symbol: "-|-"},
	query.OpLike:   {patternSymbol: "like"},
	query.OpILike:  {patternSymbol: "ilike"},
	query.OpMatch:  {patternSymbol: "~"},
	query.OpIMatch: {patternSymbol: "~*"},
	query.OpFts:    {fts: true},
	query.OpPlfts:  {fts: true},
	query.OpPhfts:  {fts: true},
	query.OpWfts:   {fts: true},
}

func scalarSymbol(op query.Operator) string { return operators[op].symbol }

func isPatternOp(op query.Operator) bool { return operators[op].patternSymbol != "" }

func patternSymbol(op query.Operator) string { return operators[op].patternSymbol }

func isFTSSearchOp(op query.Operator) bool { return operators[op].fts }

func (g *generator) isSQL(expr string, c *query.Comparison) (string, error) {
	keyword := strings.ToUpper(c.Values[0])
	if c.Op == query.OpIsDistinct {
		if c.Negated {
			return expr + " is not distinct from " + keyword, nil
		}
		return expr + " is distinct from " + keyword, nil
	}
	if c.Negated {
		return expr + " is not " + keyword, nil
	}
	return expr + " is " + keyword, nil
}

func (g *generator) arrayOrPatternSQL(expr, castType string, c *query.Comparison) (string, error) {
	if isPatternOp(c.Op) && c.Modifier == query.ModNone {
		pattern := g.b.Param(c.Values[0])
		return expr + " " + patternSymbol(c.Op) + " (" + pattern + "::text)", nil
	}

	quantifier := "any"
	if c.Modifier == query.ModAll {
		quantifier = "all"
	}
	literal := g.b.Param(arrayLiteral(c.Values))

	if isPatternOp(c.Op) {
		return expr + " " + patternSymbol(c.Op) + " " + quantifier + " (" + literal + "::text[])", nil
	}
	symbol := scalarSymbol(c.Op)
	if symbol == "" {
		return "", &FieldError{Column: c.Column, Message: fmt.Sprintf("Unsupported operator on %q", c.Column)}
	}
	return expr + " " + symbol + " " + quantifier + " ((" + literal + "::text)::" + arrayCastType(castType) + ")", nil
}

func arrayCastType(castType string) string {
	if strings.HasSuffix(castType, "[]") {
		return castType
	}
	return castType + "[]"
}

// comparisonCastType is the type a bind parameter is cast to, derived from the
// expression a JSON path produced rather than from the base field.
func comparisonCastType(base string, path []query.JSONStep, baseIsJSON bool) string {
	if len(path) == 0 {
		return base
	}
	if path[len(path)-1].Text {
		return `"pg_catalog"."text"`
	}
	if baseIsJSON {
		return base
	}
	return `"pg_catalog"."jsonb"`
}

func (g *generator) ftsSQL(expr, typeName string, c *query.Comparison) (string, error) {
	left := expr
	if !schema.IsTextSearchTypeName(typeName) {
		if c.Lang != "" {
			lang := g.b.Param(c.Lang)
			left = "to_tsvector((" + lang + "::text)::" + regconfig + ", " + expr + ")"
		} else {
			left = "to_tsvector(" + expr + ")"
		}
	}
	queryValue := g.b.Param(c.Values[0])
	fn := "to_tsquery"
	switch c.Op {
	case query.OpPlfts:
		fn = "plainto_tsquery"
	case query.OpPhfts:
		fn = "phraseto_tsquery"
	case query.OpWfts:
		fn = "websearch_to_tsquery"
	}
	var right string
	if c.Lang != "" {
		lang := g.b.Param(c.Lang)
		right = fn + "((" + lang + "::text)::" + regconfig + ", " + queryValue + "::text)"
	} else {
		right = fn + "(" + queryValue + "::text)"
	}
	return "(" + left + ") @@ (" + right + ")", nil
}

const regconfig = `"pg_catalog"."regconfig"`

func (g *generator) order(sc *scope, terms []query.OrderTerm) (string, error) {
	if len(terms) == 0 {
		return "", nil
	}
	parts := make([]string, 0, len(terms))
	for _, term := range terms {
		expr, _ := g.filterColumnExpr(sc, term.Column, term.Path)
		if term.Desc {
			expr += " desc"
		} else {
			expr += " asc"
		}
		switch term.Nulls {
		case "first":
			expr += " nulls first"
		case "last":
			expr += " nulls last"
		}
		parts = append(parts, expr)
	}
	return strings.Join(parts, ", "), nil
}
