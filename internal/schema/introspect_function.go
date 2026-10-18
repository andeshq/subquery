package schema

import (
	"context"
	"fmt"
)

const functionsSQL = `
select n.nspname, p.proname,
       p.provolatile::text,
       p.proretset,
       p.pronargdefaults::int,
       (p.prorettype = 'pg_catalog.void'::regtype) as returns_void,
       format_type(p.prorettype, null) as return_type_name,
       case when rc.oid is not null and rc.relkind in ('r','p','v','m','f') then rn.nspname else '' end,
       case when rc.oid is not null and rc.relkind in ('r','p','v','m','f') then rc.relname else '' end,
       case when rc.oid is not null then ''
            when ret.typcategory = 'A' then quote_ident(en.nspname) || '.' || quote_ident(et.typname) || '[]'
            else quote_ident(tn.nspname) || '.' || quote_ident(ret.typname)
       end
  from pg_proc p
  join pg_namespace n on n.oid = p.pronamespace
  join pg_type ret on ret.oid = p.prorettype
  join pg_namespace tn on tn.oid = ret.typnamespace
  left join pg_type et on et.oid = ret.typelem
  left join pg_namespace en on en.oid = et.typnamespace
  left join pg_class rc on rc.oid = ret.typrelid
  left join pg_namespace rn on rn.oid = rc.relnamespace
 where n.nspname = any($1::name[])
   and p.prokind = 'f'
 order by n.nspname, p.proname, p.oid`

// Multi-argument unnest pads the shorter arrays with nulls, so a null
// proargnames or proargmodes needs no explicit padding.
const functionArgsSQL = `
select n.nspname, p.proname,
       a.ordinality::int,
       coalesce(a.mode, 'i'),
       coalesce(a.name, ''),
       a.argtype::bigint,
       case when at.typcategory = 'A' then quote_ident(aen.nspname) || '.' || quote_ident(aet.typname) || '[]'
            else quote_ident(an.nspname) || '.' || quote_ident(at.typname)
       end,
       case when ac.oid is not null and ac.relkind in ('r','p','v','m','f') then acn.nspname else '' end,
       case when ac.oid is not null and ac.relkind in ('r','p','v','m','f') then ac.relname else '' end
  from pg_proc p
  join pg_namespace n on n.oid = p.pronamespace
  cross join lateral unnest(
      coalesce(p.proallargtypes, p.proargtypes::oid[]),
      coalesce(p.proargmodes::text[], '{}'::text[]),
      coalesce(p.proargnames, '{}'::text[])
  ) with ordinality as a(argtype, mode, name, ordinality)
  join pg_type at on at.oid = a.argtype
  join pg_namespace an on an.oid = at.typnamespace
  left join pg_type aet on aet.oid = at.typelem
  left join pg_namespace aen on aen.oid = aet.typnamespace
  left join pg_class ac on ac.oid = at.typrelid
  left join pg_namespace acn on acn.oid = ac.relnamespace
 where n.nspname = any($1::name[])
   and p.prokind = 'f'
 order by n.nspname, p.proname, a.ordinality`

func loadFunctions(ctx context.Context, q querier, schemas []string) ([]CatalogFunction, error) {
	rows, err := q.Query(ctx, functionsSQL, schemas)
	if err != nil {
		return nil, fmt.Errorf("load functions: %w", err)
	}
	defer rows.Close()

	byID := make(map[RelID]int)
	var out []CatalogFunction
	for rows.Next() {
		var schema, name, volatility, returnType, retSchema, retRel, retCast string
		var returnsSet, returnsVoid bool
		var defaultCount int
		if err := rows.Scan(&schema, &name, &volatility, &returnsSet, &defaultCount,
			&returnsVoid, &returnType, &retSchema, &retRel, &retCast); err != nil {
			return nil, err
		}
		fn := CatalogFunction{
			Schema:         schema,
			Name:           name,
			Volatility:     volatility[0],
			ReturnsSet:     returnsSet,
			ReturnsVoid:    returnsVoid,
			DefaultCount:   defaultCount,
			ReturnTypeName: returnType,
			ReturnCastType: retCast,
		}
		if retSchema != "" && retRel != "" {
			fn.ReturnRelation = RelID{Schema: retSchema, Name: retRel}
		}
		byID[RelID{Schema: schema, Name: name}] = len(out)
		out = append(out, fn)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	argRows, err := q.Query(ctx, functionArgsSQL, schemas)
	if err != nil {
		return nil, fmt.Errorf("load function arguments: %w", err)
	}
	defer argRows.Close()
	for argRows.Next() {
		var schema, name, mode, argName, castType, relSchema, relName string
		var position int
		var typeOID int64
		if err := argRows.Scan(&schema, &name, &position, &mode, &argName, &typeOID, &castType, &relSchema, &relName); err != nil {
			return nil, err
		}
		i, ok := byID[RelID{Schema: schema, Name: name}]
		if !ok {
			continue
		}
		arg := CatalogFunctionArg{
			Name:     argName,
			Mode:     mode[0],
			Position: position,
			TypeOID:  uint32(typeOID),
			CastType: castType,
		}
		if relSchema != "" && relName != "" {
			arg.Relation = RelID{Schema: relSchema, Name: relName}
		}
		out[i].Args = append(out[i].Args, arg)
	}
	if err := argRows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
