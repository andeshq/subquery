package sqlgen

import (
	"fmt"
	"strings"

	"github.com/andeshq/subquery/internal/query"
	"github.com/andeshq/subquery/internal/schema"
)

// WriteKind is the statement being generated.
type WriteKind int

const (
	WriteInsert WriteKind = iota
	WriteUpdate
	WriteUpsert // PUT: INSERT ... ON CONFLICT DO UPDATE
	WriteDelete
)

// Resolution is the Prefer: resolution choice.
type Resolution int

const (
	ResolutionNone Resolution = iota
	ResolutionMerge
	ResolutionIgnore
)

// ReturnMode is the Prefer: return choice.
type ReturnMode int

const (
	ReturnMinimal ReturnMode = iota
	ReturnHeadersOnly
	ReturnRepresentation
)

// ColumnError reports a column that does not exist. Writes report PGRST204.
type ColumnError struct {
	Column  string
	Message string
}

func (e *ColumnError) Error() string { return e.Message }

// WriteQuery describes a write.
type WriteQuery struct {
	Kind       WriteKind
	Columns    []string
	Conflict   []string
	Resolution Resolution
	// Payload is JSON: a JSON array of objects for inserts and upserts, or a
	// single object for updates.
	Payload []byte
	Where   []query.Filter
	Select  []query.SelectItem
}

// WriteOptions tunes the generated statement.
type WriteOptions struct {
	Return ReturnMode
}

// WriteStatement is generated write SQL plus how to consume its result.
type WriteStatement struct {
	SQL  string
	Args []any
	// Scan is true when the statement returns rows to read.
	Scan bool
	// Representation is true when a scanned row is (body, returned[, inserted]).
	Representation bool
	// ReturnedColumns names a headers-only RETURNING projection, in order.
	ReturnedColumns []string
	// Inserted is true when a scanned row carries a trailing boolean flag that
	// distinguishes an inserted row from an updated one.
	Inserted bool
}

// Write generates a write statement.
func Write(snap *schema.Snapshot, rel *schema.Relation, w *WriteQuery, opts WriteOptions) (*WriteStatement, error) {
	g := &generator{b: &Builder{}, snap: snap}
	columns, err := resolveColumns(rel, w.Columns)
	if err != nil {
		return nil, err
	}
	conflict, err := resolveColumns(rel, w.Conflict)
	if err != nil {
		return nil, err
	}
	if opts.Return == ReturnRepresentation {
		return g.writeRepresentation(rel, w, columns, conflict)
	}
	return g.writePlain(rel, w, columns, conflict, opts)
}

func resolveColumns(rel *schema.Relation, names []string) ([]string, error) {
	out := make([]string, 0, len(names))
	for _, name := range names {
		if _, ok := rel.Column(name); !ok {
			return nil, &ColumnError{
				Column:  name,
				Message: fmt.Sprintf("Could not find the '%s' column of '%s' in the schema cache", name, rel.ID.Name),
			}
		}
		out = append(out, name)
	}
	return out, nil
}

func quoteAll(names []string) []string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		out = append(out, Ident(name))
	}
	return out
}

// coreWrite builds the data-modifying statement without a RETURNING clause.
// Parameters are allocated in textual order.
func (g *generator) coreWrite(rel *schema.Relation, w *WriteQuery, columns, conflict []string) (string, error) {
	qualified := Qualified(rel.ID.Schema, rel.ID.Name)
	scope := &scope{rel: rel, alias: "pgrst_src"}

	switch w.Kind {
	case WriteInsert, WriteUpsert:
		if len(columns) == 0 {
			// An empty payload inserts a row of column defaults, so PostgreSQL
			// reports the not-null violation rather than the API rejecting it.
			if w.Kind == WriteUpsert {
				return "", &ColumnError{Message: "no columns to insert"}
			}
			return "insert into " + qualified + " default values", nil
		}
		payload := g.b.Param(string(w.Payload))
		quoted := strings.Join(quoteAll(columns), ", ")
		var sb strings.Builder
		sb.WriteString("insert into " + qualified + " (" + quoted + ")\n")
		sb.WriteString("  select " + quoted + " from json_populate_recordset(null::" + qualified + ", " + payload + "::json)")

		resolution := w.Resolution
		target := conflict
		if w.Kind == WriteUpsert {
			resolution = ResolutionMerge
		}
		if resolution != ResolutionNone {
			if len(target) == 0 {
				target = rel.PrimaryKey
			}
			if len(target) == 0 {
				return "", &ColumnError{Message: "ON CONFLICT requires a primary key or the on_conflict parameter"}
			}
			sb.WriteString("\n  on conflict (" + strings.Join(quoteAll(target), ", ") + ")")
			if resolution == ResolutionIgnore {
				sb.WriteString(" do nothing")
			} else {
				sets := make([]string, 0, len(columns))
				for _, column := range columns {
					sets = append(sets, Ident(column)+" = excluded."+Ident(column))
				}
				sb.WriteString(" do update set " + strings.Join(sets, ", "))
			}
		}
		return sb.String(), nil

	case WriteUpdate:
		if len(columns) == 0 {
			return "", &ColumnError{Message: "no columns to update"}
		}
		payload := g.b.Param(string(w.Payload))
		sets := make([]string, 0, len(columns))
		for _, column := range columns {
			sets = append(sets, Ident(column)+" = pgrst_body."+Ident(column))
		}
		var sb strings.Builder
		sb.WriteString("update " + qualified + " as pgrst_src\n  set " + strings.Join(sets, ", ") + "\n")
		sb.WriteString("  from (select * from json_populate_record(null::" + qualified + ", " + payload + "::json)) as pgrst_body")
		where, err := g.where(scope, w.Where)
		if err != nil {
			return "", err
		}
		if where != "" {
			sb.WriteString("\n  where " + where)
		}
		return sb.String(), nil

	case WriteDelete:
		sql := "delete from " + qualified + " as pgrst_src"
		where, err := g.where(scope, w.Where)
		if err != nil {
			return "", err
		}
		if where != "" {
			sql += "\n  where " + where
		}
		return sql, nil
	}
	return "", fmt.Errorf("unsupported write kind")
}

// returningNames renders a RETURNING projection. UPDATE and DELETE are
// qualified because their FROM clause can contribute identically named
// columns; INSERT and UPSERT are not, since the target has no alias.
func returningNames(kind WriteKind, names []string) []string {
	qualified := kind == WriteUpdate || kind == WriteDelete
	out := make([]string, 0, len(names))
	for _, name := range names {
		if qualified {
			out = append(out, "pgrst_src."+Ident(name))
		} else {
			out = append(out, Ident(name))
		}
	}
	return out
}

// allReturning is the full-column RETURNING projection used by the
// representation path.
func allReturning(rel *schema.Relation, kind WriteKind, extra string) []string {
	names := make([]string, 0, len(rel.Columns)+1)
	for _, column := range rel.Columns {
		names = append(names, column.Name)
	}
	out := returningNames(kind, names)
	if extra != "" {
		out = append(out, extra)
	}
	return out
}

// writePlain handles the minimal and headers-only return modes.
func (g *generator) writePlain(rel *schema.Relation, w *WriteQuery, columns, conflict []string, opts WriteOptions) (*WriteStatement, error) {
	core, err := g.coreWrite(rel, w, columns, conflict)
	if err != nil {
		return nil, err
	}
	upsert := w.Kind == WriteUpsert
	needsFlag := upsert || w.Resolution != ResolutionNone
	const insertedExpr = "(xmax = 0) as pgrst_inserted"

	switch opts.Return {
	case ReturnHeadersOnly:
		if len(rel.PrimaryKey) == 0 {
			// Without a key there is no Location to build.
			if needsFlag {
				return &WriteStatement{SQL: core + "\n  returning " + insertedExpr, Args: g.b.Args(), Scan: true, Inserted: true}, nil
			}
			return &WriteStatement{SQL: core, Args: g.b.Args()}, nil
		}
		returns := returningNames(w.Kind, rel.PrimaryKey)
		if needsFlag {
			returns = append(returns, insertedExpr)
		}
		return &WriteStatement{
			SQL:             core + "\n  returning " + strings.Join(returns, ", "),
			Args:            g.b.Args(),
			Scan:            true,
			ReturnedColumns: rel.PrimaryKey,
			Inserted:        needsFlag,
		}, nil

	case ReturnMinimal:
		// A minimal PUT reports 204, so its create-or-replace outcome is
		// unobservable and no flag is needed.
		if !upsert && needsFlag {
			return &WriteStatement{SQL: core + "\n  returning " + insertedExpr, Args: g.b.Args(), Scan: true, Inserted: true}, nil
		}
		return &WriteStatement{SQL: core, Args: g.b.Args()}, nil
	}
	return nil, fmt.Errorf("unsupported return mode")
}

// writeRepresentation wraps the write in a CTE and projects the affected rows,
// so embedded resources and vertical filtering work exactly as they do for
// reads.
func (g *generator) writeRepresentation(rel *schema.Relation, w *WriteQuery, columns, conflict []string) (*WriteStatement, error) {
	core, err := g.coreWrite(rel, w, columns, conflict)
	if err != nil {
		return nil, err
	}
	upsert := w.Kind == WriteUpsert
	// The inserted flag distinguishes a create from a replace, which the
	// status code reports for upserts and merges.
	needsFlag := upsert || w.Resolution != ResolutionNone
	returning := strings.Join(allReturning(rel, w.Kind, ""), ", ")
	if needsFlag {
		returning += ", (xmax = 0) as pgrst_inserted"
	}

	top := &scope{rel: rel, alias: "pgrst_src", items: w.Select}
	projection, groups, err := g.projection(top)
	if err != nil {
		return nil, err
	}
	embedWhere, err := g.where(top, nil)
	if err != nil {
		return nil, err
	}

	inner := "select " + projection + " from pgrst_write as pgrst_src"
	if embedWhere != "" {
		inner += " where " + embedWhere
	}
	if groupBy := groupClause(top.items, groups); groupBy != "" {
		inner += " " + groupBy
	}

	var sb strings.Builder
	sb.WriteString("with pgrst_write as (\n  " + core + "\n  returning " + returning + "\n)\n")
	sb.WriteString("select coalesce(json_agg(row_to_json(pgrst_result)), '[]'::json)::text as body,\n       count(*) as returned")
	if needsFlag {
		sb.WriteString(",\n       (select coalesce(bool_and(pgrst_write.pgrst_inserted), false) from pgrst_write) as inserted")
	}
	sb.WriteString("\nfrom (" + inner + ") pgrst_result")

	return &WriteStatement{SQL: sb.String(), Args: g.b.Args(), Scan: true, Representation: true, Inserted: needsFlag}, nil
}
