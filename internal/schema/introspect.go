package schema

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// querier is satisfied by *pgxpool.Pool and pgx.Tx.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// relationsCTE narrows the catalog to the exposed schemas plus any relation
// they reference through a foreign key or a view, so embeddings and views can
// reach private tables without exposing them as routes.
const relationsCTE = `
with wanted as (
  select c.oid
    from pg_class c
    join pg_namespace n on n.oid = c.relnamespace
   where n.nspname = any($1::name[])
     and c.relkind in ('r','p','v','m','f')
),
refd as (
  select con.confrelid as oid from pg_constraint con
   where con.contype = 'f' and con.conrelid in (select oid from wanted)
  union
  select con.conrelid from pg_constraint con
   where con.contype = 'f' and con.confrelid in (select oid from wanted)
  union
  select d.refobjid from pg_rewrite rw
    join pg_depend d on d.objid = rw.oid and d.classid = 'pg_rewrite'::regclass
   where rw.ev_class in (select oid from wanted)
  union
  select rw.ev_class from pg_rewrite rw
    join pg_depend d on d.objid = rw.oid and d.classid = 'pg_rewrite'::regclass
   where d.refobjid in (select oid from wanted)
),
rels as (
  select c.oid from pg_class c
   where (c.oid in (select oid from wanted) or c.oid in (select oid from refd))
     and c.relkind in ('r','p','v','m','f')
)
`

// Load introspects the catalog for the given schemas. The schemas should be
// the exposed schemas plus db-extra-search-path.
func Load(ctx context.Context, q querier, schemas []string) (Catalog, error) {
	var cat Catalog
	relations, err := loadRelations(ctx, q, schemas)
	if err != nil {
		return Catalog{}, err
	}
	cat.Relations = relations

	if cat.Constraints, err = loadConstraints(ctx, q, schemas); err != nil {
		return Catalog{}, err
	}
	if cat.UniqueIndexes, err = loadUniqueIndexes(ctx, q, schemas); err != nil {
		return Catalog{}, err
	}
	if cat.ViewSources, err = loadViewSources(ctx, q, schemas); err != nil {
		return Catalog{}, err
	}
	if cat.Functions, err = loadFunctions(ctx, q, schemas); err != nil {
		return Catalog{}, err
	}
	return cat, nil
}

func loadRelations(ctx context.Context, q querier, schemas []string) ([]CatalogRelation, error) {
	rows, err := q.Query(ctx, relationsCTE+`
select n.nspname, c.relname, c.relkind::text
  from pg_class c
  join pg_namespace n on n.oid = c.relnamespace
 where c.oid in (select oid from rels)
 order by n.nspname, c.relname`, schemas)
	if err != nil {
		return nil, fmt.Errorf("load relations: %w", err)
	}
	byName := make(map[RelID]int)
	var out []CatalogRelation
	for rows.Next() {
		var schema, name, kind string
		if err := rows.Scan(&schema, &name, &kind); err != nil {
			return nil, err
		}
		byName[RelID{Schema: schema, Name: name}] = len(out)
		out = append(out, CatalogRelation{Schema: schema, Name: name, Kind: relationKind(kind)})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()

	cols, err := q.Query(ctx, relationsCTE+`
select n.nspname, c.relname, a.attname, a.attnum::int, a.atttypid::bigint,
       format_type(a.atttypid, a.atttypmod), a.attnotnull, a.atthasdef,
       (a.attidentity <> '' or a.attgenerated <> ''),
       case when t.typcategory = 'A'
            then quote_ident(etn.nspname) || '.' || quote_ident(et.typname) || '[]'
            else quote_ident(tn.nspname) || '.' || quote_ident(t.typname)
       end
  from pg_attribute a
  join pg_class c on c.oid = a.attrelid
  join pg_namespace n on n.oid = c.relnamespace
  join pg_type t on t.oid = a.atttypid
  join pg_namespace tn on tn.oid = t.typnamespace
  left join pg_type et on et.oid = t.typelem
  left join pg_namespace etn on etn.oid = et.typnamespace
 where a.attrelid in (select oid from rels)
   and a.attnum > 0 and not a.attisdropped
 order by n.nspname, c.relname, a.attnum`, schemas)
	if err != nil {
		return nil, fmt.Errorf("load columns: %w", err)
	}
	defer cols.Close()
	for cols.Next() {
		var schema, name, col, typeName, castType string
		var pos int
		var typeOID int64
		var notNull, hasDefault, generated bool
		if err := cols.Scan(&schema, &name, &col, &pos, &typeOID, &typeName, &notNull, &hasDefault, &generated, &castType); err != nil {
			return nil, err
		}
		i, ok := byName[RelID{Schema: schema, Name: name}]
		if !ok {
			continue
		}
		out[i].Columns = append(out[i].Columns, CatalogColumn{
			Name:       col,
			Position:   pos,
			TypeOID:    uint32(typeOID),
			TypeName:   typeName,
			CastType:   castType,
			NotNull:    notNull,
			HasDefault: hasDefault,
			Generated:  generated,
		})
	}
	return out, cols.Err()
}

func loadConstraints(ctx context.Context, q querier, schemas []string) ([]CatalogConstraint, error) {
	rows, err := q.Query(ctx, relationsCTE+`
select con.conname, con.contype::text,
       sn.nspname, sc.relname,
       coalesce(array(select att.attname from pg_attribute att
                       where att.attrelid = con.conrelid and att.attnum = any(con.conkey)
                       order by array_position(con.conkey, att.attnum)), '{}'::text[]),
       coalesce(tn.nspname, ''), coalesce(tc.relname, ''),
       coalesce(array(select att.attname from pg_attribute att
                       where att.attrelid = con.confrelid and att.attnum = any(con.confkey)
                       order by array_position(con.confkey, att.attnum)), '{}'::text[])
  from pg_constraint con
  join pg_class sc on sc.oid = con.conrelid
  join pg_namespace sn on sn.oid = sc.relnamespace
  left join pg_class tc on tc.oid = con.confrelid
  left join pg_namespace tn on tn.oid = tc.relnamespace
 where con.contype in ('p','u','f')
   and con.conrelid in (select oid from rels)
 order by con.conname`, schemas)
	if err != nil {
		return nil, fmt.Errorf("load constraints: %w", err)
	}
	defer rows.Close()
	var out []CatalogConstraint
	for rows.Next() {
		var name, contype, srcSchema, srcRel, tgtSchema, tgtRel string
		var cols, refCols []string
		if err := rows.Scan(&name, &contype, &srcSchema, &srcRel, &cols, &tgtSchema, &tgtRel, &refCols); err != nil {
			return nil, err
		}
		out = append(out, CatalogConstraint{
			Name:    name,
			Type:    contype[0],
			Rel:     RelID{Schema: srcSchema, Name: srcRel},
			Columns: cols,
			RefRel:  RelID{Schema: tgtSchema, Name: tgtRel},
			RefCols: refCols,
		})
	}
	return out, rows.Err()
}

func loadUniqueIndexes(ctx context.Context, q querier, schemas []string) ([]CatalogUniqueIndex, error) {
	rows, err := q.Query(ctx, relationsCTE+`
select n.nspname, c.relname, ic.relname,
       array(select att.attname
               from unnest(i.indkey) with ordinality as k(attnum, ord)
               join pg_attribute att on att.attrelid = i.indrelid and att.attnum = k.attnum
              order by k.ord)
  from pg_index i
  join pg_class c on c.oid = i.indrelid
  join pg_namespace n on n.oid = c.relnamespace
  join pg_class ic on ic.oid = i.indexrelid
 where i.indisunique and i.indisvalid
   and i.indpred is null and i.indexprs is null
   and i.indnkeyatts = i.indnatts
   and i.indrelid in (select oid from rels)`, schemas)
	if err != nil {
		return nil, fmt.Errorf("load unique indexes: %w", err)
	}
	defer rows.Close()
	var out []CatalogUniqueIndex
	for rows.Next() {
		var schema, rel, name string
		var cols []string
		if err := rows.Scan(&schema, &rel, &name, &cols); err != nil {
			return nil, err
		}
		out = append(out, CatalogUniqueIndex{Name: name, Rel: RelID{Schema: schema, Name: rel}, Columns: cols})
	}
	return out, rows.Err()
}

func loadViewSources(ctx context.Context, q querier, schemas []string) ([]CatalogViewSource, error) {
	rows, err := q.Query(ctx, relationsCTE+`
select vn.nspname, vc.relname, bn.nspname, bc.relname
  from pg_rewrite rw
  join pg_class vc on vc.oid = rw.ev_class
  join pg_namespace vn on vn.oid = vc.relnamespace
  join pg_depend d on d.objid = rw.oid
                  and d.classid = 'pg_rewrite'::regclass
                  and d.refclassid = 'pg_class'::regclass
  join pg_class bc on bc.oid = d.refobjid
  join pg_namespace bn on bn.oid = bc.relnamespace
 where vc.relkind in ('v','m')
   and bc.oid <> vc.oid
   and bc.relkind in ('r','p','v','m','f')
   and vc.oid in (select oid from rels)
 group by vn.nspname, vc.relname, bn.nspname, bc.relname`, schemas)
	if err != nil {
		return nil, fmt.Errorf("load view sources: %w", err)
	}
	defer rows.Close()
	var out []CatalogViewSource
	for rows.Next() {
		var vs CatalogViewSource
		if err := rows.Scan(&vs.View.Schema, &vs.View.Name, &vs.Base.Schema, &vs.Base.Name); err != nil {
			return nil, err
		}
		out = append(out, vs)
	}
	return out, rows.Err()
}

func relationKind(relkind string) RelationKind {
	switch relkind {
	case "r":
		return KindTable
	case "p":
		return KindPartitioned
	case "v":
		return KindView
	case "m":
		return KindMaterialized
	case "f":
		return KindForeign
	default:
		return RelationKind(relkind)
	}
}
