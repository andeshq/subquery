package sqlgen

import (
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/andeshq/subquery/internal/query"
	"github.com/andeshq/subquery/internal/schema"
)

func column(name, typeName, castType string) schema.CatalogColumn {
	return schema.CatalogColumn{Name: name, TypeName: typeName, CastType: castType}
}

// testFilms builds a snapshot with the relationships embeddings need.
func testFilms(t testing.TB) (*schema.Snapshot, *schema.Relation) {
	t.Helper()
	films := schema.RelID{Schema: "public", Name: "films"}
	directors := schema.RelID{Schema: "public", Name: "directors"}
	actors := schema.RelID{Schema: "public", Name: "actors"}
	roles := schema.RelID{Schema: "public", Name: "roles"}
	specs := schema.RelID{Schema: "public", Name: "technical_specs"}

	snap := schema.Build(1, []string{"public"}, schema.Catalog{
		Relations: []schema.CatalogRelation{
			{Schema: "public", Name: "films", Kind: schema.KindTable, Columns: []schema.CatalogColumn{
				column("id", "integer", `"pg_catalog"."int4"`),
				column("title", "text", `"pg_catalog"."text"`),
				column("year", "integer", `"pg_catalog"."int4"`),
				column("director_id", "integer", `"pg_catalog"."int4"`),
				column("tags", "text[]", `"pg_catalog"."text"[]`),
				column("meta", "jsonb", `"pg_catalog"."jsonb"`),
				column("doc", "tsvector", `"pg_catalog"."tsvector"`),
				column("body", "text", `"pg_catalog"."text"`),
			}},
			{Schema: "public", Name: "directors", Kind: schema.KindTable, Columns: []schema.CatalogColumn{
				column("id", "integer", `"pg_catalog"."int4"`),
				column("last_name", "text", `"pg_catalog"."text"`),
			}},
			{Schema: "public", Name: "actors", Kind: schema.KindTable, Columns: []schema.CatalogColumn{
				column("id", "integer", `"pg_catalog"."int4"`),
				column("first_name", "text", `"pg_catalog"."text"`),
			}},
			{Schema: "public", Name: "roles", Kind: schema.KindTable, Columns: []schema.CatalogColumn{
				column("film_id", "integer", `"pg_catalog"."int4"`),
				column("actor_id", "integer", `"pg_catalog"."int4"`),
				column("character", "text", `"pg_catalog"."text"`),
			}},
			{Schema: "public", Name: "technical_specs", Kind: schema.KindTable, Columns: []schema.CatalogColumn{
				column("film_id", "integer", `"pg_catalog"."int4"`),
				column("camera", "text", `"pg_catalog"."text"`),
			}},
		},
		Constraints: []schema.CatalogConstraint{
			{Name: "films_pkey", Type: 'p', Rel: films, Columns: []string{"id"}},
			{Name: "directors_pkey", Type: 'p', Rel: directors, Columns: []string{"id"}},
			{Name: "actors_pkey", Type: 'p', Rel: actors, Columns: []string{"id"}},
			{Name: "roles_pkey", Type: 'p', Rel: roles, Columns: []string{"film_id", "actor_id"}},
			{Name: "specs_pkey", Type: 'p', Rel: specs, Columns: []string{"film_id"}},
			{Name: "films_director_id_fkey", Type: 'f', Rel: films, Columns: []string{"director_id"}, RefRel: directors, RefCols: []string{"id"}},
			{Name: "roles_film_id_fkey", Type: 'f', Rel: roles, Columns: []string{"film_id"}, RefRel: films, RefCols: []string{"id"}},
			{Name: "roles_actor_id_fkey", Type: 'f', Rel: roles, Columns: []string{"actor_id"}, RefRel: actors, RefCols: []string{"id"}},
			{Name: "specs_film_id_fkey", Type: 'f', Rel: specs, Columns: []string{"film_id"}, RefRel: films, RefCols: []string{"id"}},
		},
		// A scalar function of the films row type is a computed field.
		Functions: []schema.CatalogFunction{{
			Schema: "public", Name: "film_label", Volatility: 's',
			ReturnCastType: `"pg_catalog"."text"`, ReturnTypeName: "text",
			Args: []schema.CatalogFunctionArg{{
				Name: "films", Mode: 'i', Position: 1, CastType: `"public"."films"`, Relation: films,
			}},
		}},
	})
	rel, ok := snap.Relation("films")
	if !ok {
		t.Fatal("films is not in the snapshot")
	}
	return snap, rel
}

func generate(t *testing.T, raw string, opts ReadOptions) (*Statement, error) {
	t.Helper()
	snap, rel := testFilms(t)
	values, err := url.ParseQuery(raw)
	if err != nil {
		t.Fatalf("ParseQuery(%q): %v", raw, err)
	}
	q, err := query.Parse(values)
	if err != nil {
		t.Fatalf("query.Parse(%q): %v", raw, err)
	}
	return Read(snap, rel, q, opts)
}

func mustGenerate(t *testing.T, raw string, opts ReadOptions) *Statement {
	t.Helper()
	stmt, err := generate(t, raw, opts)
	if err != nil {
		t.Fatalf("Read(%q): %v", raw, err)
	}
	return stmt
}

func assertContains(t *testing.T, stmt *Statement, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(stmt.SQL, w) {
			t.Errorf("SQL missing %q:\n%s", w, stmt.SQL)
		}
	}
}

func TestReadSelectAll(t *testing.T) {
	stmt := mustGenerate(t, "", ReadOptions{})
	assertContains(t, stmt,
		`select pgrst_src."id", pgrst_src."title", pgrst_src."year", pgrst_src."director_id", pgrst_src."tags", pgrst_src."meta", pgrst_src."doc", pgrst_src."body" from "public"."films" as pgrst_src`,
		"limit all",
	)
	if len(stmt.Args) != 0 {
		t.Errorf("Args = %v, want none", stmt.Args)
	}
}

func TestReadProjection(t *testing.T) {
	stmt := mustGenerate(t, "select=fullTitle:title,id::int8,meta->>rating,meta->tags->0", ReadOptions{})
	assertContains(t, stmt,
		`pgrst_src."title" as "fullTitle"`,
		`(pgrst_src."id")::int8`,
		`pgrst_src."meta" ->> $1::text as "rating"`,
		`pgrst_src."meta" -> $2::text -> $3::int as "0"`,
	)
}

func TestReadToJSONBWrapping(t *testing.T) {
	stmt := mustGenerate(t, "select=title->>key", ReadOptions{})
	assertContains(t, stmt, `to_jsonb(pgrst_src."title") ->> $1::text`)
}

func TestReadScalarComparison(t *testing.T) {
	// Filters are applied in sorted key order, so title precedes year.
	stmt := mustGenerate(t, "year=gte.1980&title=eq.Pulp%20Fiction", ReadOptions{})
	assertContains(t, stmt,
		`pgrst_src."title" = ($1::text)::"pg_catalog"."text"`,
		`pgrst_src."year" >= ($2::text)::"pg_catalog"."int4"`,
	)
	if len(stmt.Args) != 2 || stmt.Args[0] != "Pulp Fiction" || stmt.Args[1] != "1980" {
		t.Errorf("Args = %#v", stmt.Args)
	}
}

func TestReadInList(t *testing.T) {
	stmt := mustGenerate(t, "id=in.(1,2,3)", ReadOptions{})
	assertContains(t, stmt, `pgrst_src."id" = any (($1::text)::"pg_catalog"."int4"[])`)
	if len(stmt.Args) != 1 || stmt.Args[0] != "{1,2,3}" {
		t.Errorf("Args = %#v", stmt.Args)
	}
}

func TestReadPattern(t *testing.T) {
	stmt := mustGenerate(t, "title=like.*Pulp*", ReadOptions{})
	assertContains(t, stmt, `pgrst_src."title" like ($1::text)`)
	if stmt.Args[0] != "%Pulp%" {
		t.Errorf("Args = %#v, want the percent-substituted pattern", stmt.Args)
	}
}

func TestReadPatternAny(t *testing.T) {
	stmt := mustGenerate(t, "title=like(any).{Pulp*,Reservoir*}", ReadOptions{})
	assertContains(t, stmt, `pgrst_src."title" like any ($1::text[])`)
	if stmt.Args[0] != "{Pulp%,Reservoir%}" {
		t.Errorf("Args = %#v", stmt.Args)
	}
}

func TestReadArrayContains(t *testing.T) {
	stmt := mustGenerate(t, "tags=cs.{example,new}", ReadOptions{})
	assertContains(t, stmt, `pgrst_src."tags" @> ($1::text)::"pg_catalog"."text"[]`)
	if stmt.Args[0] != "{example,new}" {
		t.Errorf("Args = %#v", stmt.Args)
	}
}

func TestReadIsNull(t *testing.T) {
	stmt := mustGenerate(t, "title=is.null", ReadOptions{})
	assertContains(t, stmt, `pgrst_src."title" is NULL`)

	stmt = mustGenerate(t, "title=not.is.null", ReadOptions{})
	assertContains(t, stmt, `pgrst_src."title" is not NULL`)
	if strings.Contains(stmt.SQL, "not (") {
		t.Errorf("is/isdistinct must not add a wrapping not: %s", stmt.SQL)
	}
}

func TestReadFullTextSearch(t *testing.T) {
	stmt := mustGenerate(t, "doc=fts(french).amusant", ReadOptions{})
	assertContains(t, stmt, `@@ (to_tsquery(($2::text)::"pg_catalog"."regconfig"`)
	if stmt.Args[0] != "amusant" || stmt.Args[1] != "french" {
		t.Errorf("Args = %#v", stmt.Args)
	}
}

func TestReadLogicalGroup(t *testing.T) {
	stmt := mustGenerate(t, "or=(year.lt.1950,year.gt.2000)", ReadOptions{})
	assertContains(t, stmt, `(pgrst_src."year" < ($1::text)::"pg_catalog"."int4" or pgrst_src."year" > ($2::text)::"pg_catalog"."int4")`)
}

func TestReadNestedNegatedLogical(t *testing.T) {
	stmt := mustGenerate(t, "or=(year.eq.1994,not.and(year.gte.1990,year.lte.2000))", ReadOptions{})
	assertContains(t, stmt, `or not (pgrst_src."year" >= `)
}

func TestReadOrderLimitOffset(t *testing.T) {
	stmt := mustGenerate(t, "order=year.desc.nullslast&limit=10&offset=5", ReadOptions{})
	assertContains(t, stmt, `order by pgrst_src."year" desc nulls last`, "limit 10", "offset 5")
}

func TestReadMaxRowsCap(t *testing.T) {
	assertContains(t, mustGenerate(t, "", ReadOptions{MaxRows: 100}), "limit 100")
	assertContains(t, mustGenerate(t, "limit=5000", ReadOptions{MaxRows: 100}), "limit 100")
}

func TestReadCountAddsTotal(t *testing.T) {
	snap, rel := testFilms(t)
	values, _ := url.ParseQuery("")
	q, _ := query.Parse(values)
	q.Count = query.CountExact
	stmt, err := Read(snap, rel, q, ReadOptions{})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	assertContains(t, stmt, "_pgrst_total", "as total", "count(*) as returned")
}

func TestReadOrderByUnselectedColumn(t *testing.T) {
	stmt := mustGenerate(t, "select=title&order=year&limit=1", ReadOptions{})
	assertContains(t, stmt, `select pgrst_src."title" from "public"."films" as pgrst_src`, `order by pgrst_src."year" asc`)
}

// --- computed fields and aggregates ---

func TestReadComputedField(t *testing.T) {
	stmt := mustGenerate(t, "select=title,film_label&film_label=like.*Pulp*&order=film_label", ReadOptions{})
	assertContains(t, stmt,
		`"public"."film_label"(pgrst_src)`,
		`"public"."film_label"(pgrst_src) like ($1::text)`,
		`order by "public"."film_label"(pgrst_src) asc`,
	)
}

func TestReadAggregateOnly(t *testing.T) {
	stmt := mustGenerate(t, "select=count()", ReadOptions{})
	assertContains(t, stmt, `count(*) as "count"`)
	if strings.Contains(stmt.SQL, "group by") {
		t.Errorf("a bare aggregate must not group:\n%s", stmt.SQL)
	}
}

func TestReadAggregateWithGrouping(t *testing.T) {
	stmt := mustGenerate(t, "select=year,count(),max(id)", ReadOptions{})
	assertContains(t, stmt,
		`count(*) as "count"`,
		`max(pgrst_src."id") as "max"`,
		`group by pgrst_src."year"`,
	)
}

func TestReadAggregateOnColumn(t *testing.T) {
	stmt := mustGenerate(t, "select=sum(year)", ReadOptions{})
	assertContains(t, stmt, `sum(pgrst_src."year") as "sum"`)
}

func TestReadAggregateWithEmbedRejected(t *testing.T) {
	_, err := generate(t, "select=count(),roles(character)", ReadOptions{})
	var unsupported *UnsupportedError
	if !errors.As(err, &unsupported) {
		t.Fatalf("error = %v, want *UnsupportedError", err)
	}
}

func TestReadAggregateInEmbed(t *testing.T) {
	stmt := mustGenerate(t, "select=title,roles(count())", ReadOptions{})
	assertContains(t, stmt, `count(*) as "count"`)
}

func TestReadUnknownColumn(t *testing.T) {
	// `select` is validated against the schema cache.
	var fieldErr *FieldError
	if _, err := generate(t, "select=bogus", ReadOptions{}); !errors.As(err, &fieldErr) {
		t.Fatalf("unknown select column error = %v", err)
	}
	// Filters and ordering are passed through quoted so PostgreSQL reports the
	// error, matching PostgREST.
	stmt, err := generate(t, "bogus=eq.1", ReadOptions{})
	if err != nil {
		t.Fatalf("an unknown filter column should be passed through: %v", err)
	}
	if !strings.Contains(stmt.SQL, `pgrst_src."bogus" = (`) {
		t.Errorf("SQL = %s", stmt.SQL)
	}
	if _, err := generate(t, "order=bogus", ReadOptions{}); err != nil {
		t.Fatalf("an unknown order column should be passed through: %v", err)
	}
}

func TestReadRejectsMaliciousCast(t *testing.T) {
	for _, cast := range []string{"text);drop table films;--", `"text"`, "text)", "int; select 1"} {
		raw := "select=id::" + url.QueryEscape(cast)
		if _, err := generate(t, raw, ReadOptions{}); err == nil {
			t.Errorf("cast %q was accepted", cast)
		}
	}
}

// TestNoRequestTextReachesSQL asserts the core safety invariant: request values
// appear only as bind arguments, never inside the statement text.
func TestNoRequestTextReachesSQL(t *testing.T) {
	snap, rel := testFilms(t)
	injections := []string{
		`'; drop table films; --`,
		`1) or (1=1`,
		`x"); DROP TABLE films; --`,
	}
	for _, payload := range injections {
		values := url.Values{
			"title":  {"eq." + payload},
			"select": {"id," + payload + ":title"},
			"order":  {"title." + payload},
		}
		q, err := query.Parse(values)
		if err != nil {
			continue
		}
		stmt, err := Read(snap, rel, q, ReadOptions{})
		if err != nil {
			continue
		}
		if strings.Contains(strings.ToUpper(stmt.SQL), "DROP TABLE") {
			t.Errorf("payload %q leaked into SQL:\n%s", payload, stmt.SQL)
		}
		if strings.Contains(stmt.SQL, ";") {
			t.Errorf("payload %q introduced a statement separator:\n%s", payload, stmt.SQL)
		}
		found := false
		for _, arg := range stmt.Args {
			if s, ok := arg.(string); ok && strings.Contains(s, payload) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("payload %q should be present as a bind argument", payload)
		}
	}
}

func TestValidCast(t *testing.T) {
	valid := []string{"text", "int8", "numeric(10,2)", "text[]", "varchar(20)", "pg_catalog.int4", "double precision", "timestamp with time zone"}
	for _, c := range valid {
		if !validCast(c) {
			t.Errorf("validCast(%q) = false, want true", c)
		}
	}
	invalid := []string{"", "text;", "text)", "count(*)", `"text"`, "numeric(1;2)", "a.b.c", "text drop"}
	for _, c := range invalid {
		if validCast(c) {
			t.Errorf("validCast(%q) = true, want false", c)
		}
	}
}

// --- embeddings ---

func TestReadEmbedToOne(t *testing.T) {
	stmt := mustGenerate(t, "select=title,directors(last_name)", ReadOptions{})
	assertContains(t, stmt,
		`(select row_to_json(pgrst_r`,
		`= pgrst_src."director_id"`,
		`pgrst_src."title"`,
		`as "directors"`,
		"limit 1",
	)
}

func TestReadEmbedToMany(t *testing.T) {
	stmt := mustGenerate(t, "select=title,roles(character)", ReadOptions{})
	assertContains(t, stmt,
		`coalesce((select json_agg(row_to_json(pgrst_r`,
		`= pgrst_src."id"`,
		`as "roles"`,
	)
}

func TestReadEmbedManyToMany(t *testing.T) {
	stmt := mustGenerate(t, "select=title,actors(first_name)", ReadOptions{})
	assertContains(t, stmt,
		`coalesce((select json_agg(row_to_json(pgrst_r`,
		`"public"."roles" as pgrst_j`,
		`= pgrst_src."id"`,
		`as "actors"`,
	)
}

func TestReadEmbedAlias(t *testing.T) {
	stmt := mustGenerate(t, "select=title,director:directors(last_name)", ReadOptions{})
	assertContains(t, stmt, `as "director"`)
}

func TestReadEmbedInnerFiltersParent(t *testing.T) {
	stmt := mustGenerate(t, "select=title,actors!inner(first_name)&actors.first_name=eq.John", ReadOptions{})
	assertContains(t, stmt,
		`exists (select 1 from`,
		`"first_name" = (`,
		`as "actors"`,
	)
}

func TestReadEmbedNullAntiJoin(t *testing.T) {
	stmt := mustGenerate(t, "select=title,technical_specs(*)&technical_specs=is.null", ReadOptions{})
	assertContains(t, stmt, `not exists (select 1 from`)
}

func TestReadEmbedInnerViaNotNull(t *testing.T) {
	stmt := mustGenerate(t, "select=title,technical_specs(*)&technical_specs=not.is.null", ReadOptions{})
	assertContains(t, stmt, `exists (select 1 from`)
	if strings.Contains(stmt.SQL, "not exists (select 1 from") {
		t.Errorf("not.is.null must not produce an anti-join:\n%s", stmt.SQL)
	}
}

func TestReadEmbeddedOrderAndLimit(t *testing.T) {
	stmt := mustGenerate(t, "select=title,roles(character)&roles.order=character.desc&roles.limit=2", ReadOptions{})
	assertContains(t, stmt, `order by pgrst_e`, `."character" desc`, "limit 2")
}

func TestReadNestedEmbed(t *testing.T) {
	// films -> roles -> actors
	stmt := mustGenerate(t, "select=title,roles(character,actors(first_name))", ReadOptions{})
	assertContains(t, stmt, `as "roles"`, `as "actors"`)
	if strings.Count(stmt.SQL, "json_agg") != 2 {
		t.Errorf("expected two aggregations:\n%s", stmt.SQL)
	}
}

func TestReadEmbedUnknownRelation(t *testing.T) {
	_, err := generate(t, "select=title,unicorns(*)", ReadOptions{})
	var schemaErr *schema.SchemaError
	if !errors.As(err, &schemaErr) {
		t.Fatalf("error = %v, want *schema.SchemaError", err)
	}
	if schemaErr.Code != "PGRST200" {
		t.Errorf("code = %q, want PGRST200", schemaErr.Code)
	}
}

func TestReadEmbedUnknownParamTarget(t *testing.T) {
	values, err := url.ParseQuery("select=title&unicorns.order=id")
	if err != nil {
		t.Fatalf("ParseQuery: %v", err)
	}
	_, err = query.Parse(values)
	var queryErr *query.Error
	if !errors.As(err, &queryErr) {
		t.Fatalf("error = %v, want *query.Error", err)
	}
	if queryErr.Code != "PGRST108" {
		t.Errorf("code = %q, want PGRST108", queryErr.Code)
	}
}

func TestReadSpreadToOne(t *testing.T) {
	stmt := mustGenerate(t, "select=title,...directors(lastName:last_name)", ReadOptions{})
	assertContains(t, stmt,
		`(select pgrst_r`,
		`."lastName" from (`,
		`as "lastName"`,
		// A spread lifts the columns and adds no wrapper object.
	)
	if strings.Contains(stmt.SQL, `as "directors"`) {
		t.Errorf("spread must not wrap in an object:\n%s", stmt.SQL)
	}
}

func TestReadSpreadToMany(t *testing.T) {
	stmt := mustGenerate(t, "select=title,...roles(character)", ReadOptions{})
	assertContains(t, stmt,
		`coalesce((select json_agg(pgrst_r`,
		`."character") from (`,
		`as "character"`,
	)
	if strings.Contains(stmt.SQL, `as "roles"`) {
		t.Errorf("spread must not wrap in an object:\n%s", stmt.SQL)
	}
}

func TestReadSpreadInsideEmbed(t *testing.T) {
	// roles -> actors is to-one, so the spread lifts it into each role object.
	stmt := mustGenerate(t, "select=title,roles(character,...actors(first_name))", ReadOptions{})
	assertContains(t, stmt, `."character"`, `as "first_name"`, `as "roles"`)
	// The lifted column is not wrapped in an actors object.
	if strings.Contains(stmt.SQL, `as "actors"`) {
		t.Errorf("spread must not wrap in an object:\n%s", stmt.SQL)
	}
}
