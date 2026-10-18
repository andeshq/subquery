package pg

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// Queryer is satisfied by *pgxpool.Pool and pgx.Tx.
type Queryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Privileges is the set of operations a role may perform on a relation.
type Privileges struct {
	Select bool
	Insert bool
	Update bool
	Delete bool
}

// relationPrivilegesSQL reports the grants a role holds on a relation and, for
// a relation whose kind is not a plain table, which writes PostgreSQL can route
// through it.
const relationPrivilegesSQL = `
select
  has_table_privilege($1, $2, 'SELECT'),
  has_table_privilege($1, $2, 'INSERT'),
  has_table_privilege($1, $2, 'UPDATE'),
  has_table_privilege($1, $2, 'DELETE'),
  case when $3 then pg_relation_is_updatable($2::regclass, true) else 0 end`

// pg_relation_is_updatable bit flags.
const (
	updatableInsert = 4
	updatableUpdate = 8
	updatableDelete = 16
)

// RelationPrivileges reports what role may do with a relation. When notTable is
// true the relation's kind is not a plain table, so the write verbs additionally
// require pg_relation_is_updatable to permit them.
func RelationPrivileges(ctx context.Context, q Queryer, role, qualifiedName string, notTable bool) (Privileges, error) {
	var (
		p         Privileges
		updatable int
	)
	if err := q.QueryRow(ctx, relationPrivilegesSQL, role, qualifiedName, notTable).
		Scan(&p.Select, &p.Insert, &p.Update, &p.Delete, &updatable); err != nil {
		return Privileges{}, err
	}
	if notTable {
		p.Insert = p.Insert && updatable&updatableInsert != 0
		p.Update = p.Update && updatable&updatableUpdate != 0
		p.Delete = p.Delete && updatable&updatableDelete != 0
	}
	return p, nil
}

// AllowedRelations reports, for each quoted relation name, whether role may
// SELECT it.
func AllowedRelations(ctx context.Context, q Queryer, role string, names []string) (map[string]bool, error) {
	allowed := make(map[string]bool, len(names))
	if len(names) == 0 {
		return allowed, nil
	}
	rows, err := q.Query(ctx,
		`select name, has_table_privilege($1, name, 'SELECT') from unnest($2::text[]) as name`,
		role, names)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var ok bool
		if err := rows.Scan(&name, &ok); err != nil {
			return nil, err
		}
		allowed[name] = ok
	}
	return allowed, rows.Err()
}

// AllowedSchemas reports, for each schema name, whether role has USAGE.
func AllowedSchemas(ctx context.Context, q Queryer, role string, names []string) (map[string]bool, error) {
	allowed := make(map[string]bool, len(names))
	if len(names) == 0 {
		return allowed, nil
	}
	rows, err := q.Query(ctx,
		`select nspname, has_schema_privilege($1, oid, 'USAGE') from pg_namespace where nspname = any($2::text[])`,
		role, names)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var ok bool
		if err := rows.Scan(&name, &ok); err != nil {
			return nil, err
		}
		allowed[name] = ok
	}
	return allowed, rows.Err()
}
