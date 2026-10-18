package sqlgen

import (
	"strings"
	"testing"

	"github.com/andeshq/subquery/internal/query"
	"github.com/andeshq/subquery/internal/schema"
)

func rpcSnapshot(t *testing.T) *schema.Snapshot {
	t.Helper()
	films := schema.RelID{Schema: "public", Name: "films"}
	int4 := `"pg_catalog"."int4"`
	return schema.Build(1, []string{"public"}, schema.Catalog{
		Relations: []schema.CatalogRelation{{
			Schema: "public", Name: "films", Kind: schema.KindTable,
			Columns: []schema.CatalogColumn{
				column("id", "integer", int4),
				column("title", "text", `"pg_catalog"."text"`),
			},
		}},
		Constraints: []schema.CatalogConstraint{
			{Name: "films_pkey", Type: 'p', Rel: films, Columns: []string{"id"}},
		},
		Functions: []schema.CatalogFunction{
			{Schema: "public", Name: "add_them", Volatility: 'i', ReturnCastType: int4,
				Args: []schema.CatalogFunctionArg{
					{Name: "a", Mode: 'i', Position: 1, CastType: int4},
					{Name: "b", Mode: 'i', Position: 2, CastType: int4},
				}},
			{Schema: "public", Name: "greet", Volatility: 's', ReturnCastType: `"pg_catalog"."text"`, DefaultCount: 1,
				Args: []schema.CatalogFunctionArg{{Name: "username", Mode: 'i', Position: 1, CastType: `"pg_catalog"."text"`}}},
			{Schema: "public", Name: "best_films", Volatility: 's', ReturnsSet: true, ReturnRelation: films},
			{Schema: "public", Name: "plus_one", Volatility: 'i', ReturnCastType: `"pg_catalog"."int4"[]`,
				Args: []schema.CatalogFunctionArg{{Name: "v", Mode: 'v', Position: 1, CastType: `"pg_catalog"."int4"[]`}}},
			{Schema: "public", Name: "touch", Volatility: 'v', ReturnsVoid: true,
				Args: []schema.CatalogFunctionArg{{Name: "n", Mode: 'i', Position: 1, CastType: int4}}},
		},
	})
}

func resolveFn(t *testing.T, snap *schema.Snapshot, name string, provided ...string) *schema.Function {
	t.Helper()
	fn, schemaErr := snap.ResolveFunction(name, provided)
	if schemaErr != nil {
		t.Fatalf("ResolveFunction(%s): %v", name, schemaErr)
	}
	return fn
}

func TestRPCScalarSQL(t *testing.T) {
	snap := rpcSnapshot(t)
	fn := resolveFn(t, snap, "add_them", "a", "b")
	stmt, err := RPC(snap, fn, &query.Query{}, []ArgValue{
		{Name: "a", Text: "1"},
		{Name: "b", Text: "2"},
	}, ReadOptions{})
	if err != nil {
		t.Fatalf("RPC: %v", err)
	}
	assertContains(t, stmt,
		`to_jsonb("public"."add_them"("a" => ($1::text)::"pg_catalog"."int4", "b" => ($2::text)::"pg_catalog"."int4"))::text as body`,
	)
	if len(stmt.Args) != 2 || stmt.Args[0] != "1" || stmt.Args[1] != "2" {
		t.Errorf("Args = %#v", stmt.Args)
	}
}

func TestRPCOmitsDefaultedArgument(t *testing.T) {
	snap := rpcSnapshot(t)
	fn := resolveFn(t, snap, "greet")
	stmt, err := RPC(snap, fn, &query.Query{}, nil, ReadOptions{})
	if err != nil {
		t.Fatalf("RPC: %v", err)
	}
	// The defaulted argument is simply absent, so PostgreSQL applies DEFAULT.
	assertContains(t, stmt, `"public"."greet"()`)
}

func TestRPCTableValuedUsesReadPipeline(t *testing.T) {
	snap := rpcSnapshot(t)
	fn := resolveFn(t, snap, "best_films")
	stmt, err := RPC(snap, fn, &query.Query{
		Select: []query.SelectItem{{Column: "title"}},
	}, nil, ReadOptions{})
	if err != nil {
		t.Fatalf("RPC: %v", err)
	}
	assertContains(t, stmt,
		`select pgrst_src."title" from "public"."best_films"() as pgrst_src`,
		`json_agg(row_to_json(pgrst_source))`,
	)
}

func TestRPCVariadicSQL(t *testing.T) {
	snap := rpcSnapshot(t)
	fn := resolveFn(t, snap, "plus_one", "v")
	stmt, err := RPC(snap, fn, &query.Query{}, []ArgValue{{Name: "v", Text: "{1,2,3}"}}, ReadOptions{})
	if err != nil {
		t.Fatalf("RPC: %v", err)
	}
	assertContains(t, stmt, `variadic ($1::text)::"pg_catalog"."int4"[]`)
}

func TestRPCVoidSQL(t *testing.T) {
	snap := rpcSnapshot(t)
	fn := resolveFn(t, snap, "touch", "n")
	stmt, err := RPC(snap, fn, &query.Query{}, []ArgValue{{Name: "n", Text: "1"}}, ReadOptions{})
	if err != nil {
		t.Fatalf("RPC: %v", err)
	}
	assertContains(t, stmt, `select "public"."touch"("n" => ($1::text)::"pg_catalog"."int4")`)
}

func TestRPCUnknownFunction(t *testing.T) {
	snap := rpcSnapshot(t)
	_, schemaErr := snap.ResolveFunction("nope", nil)
	if schemaErr == nil || schemaErr.Code != "PGRST202" {
		t.Fatalf("error = %v, want PGRST202", schemaErr)
	}
}

func TestRPCAmbiguousOverload(t *testing.T) {
	int4 := `"pg_catalog"."int4"`
	snap := schema.Build(1, []string{"public"}, schema.Catalog{
		Functions: []schema.CatalogFunction{
			{Schema: "public", Name: "f", Volatility: 'i', ReturnCastType: int4,
				Args: []schema.CatalogFunctionArg{{Name: "x", Mode: 'i', Position: 1, CastType: int4}}},
			{Schema: "public", Name: "f", Volatility: 'i', ReturnCastType: int4,
				Args: []schema.CatalogFunctionArg{{Name: "x", Mode: 'i', Position: 1, CastType: `"pg_catalog"."int8"`}}},
		},
	})
	_, schemaErr := snap.ResolveFunction("f", []string{"x"})
	if schemaErr == nil || schemaErr.Code != "PGRST203" {
		t.Fatalf("error = %v, want PGRST203", schemaErr)
	}
}

func TestRPCRequiresNamedArguments(t *testing.T) {
	snap := rpcSnapshot(t)
	_, schemaErr := snap.ResolveFunction("add_them", []string{"a"})
	if schemaErr == nil || schemaErr.Code != "PGRST202" {
		t.Fatalf("error = %v, want PGRST202", schemaErr)
	}
}

// TestRPCArgumentIsBound asserts argument values never reach the SQL text.
func TestRPCArgumentIsBound(t *testing.T) {
	snap := rpcSnapshot(t)
	fn := resolveFn(t, snap, "add_them", "a", "b")
	payload := `1); drop table films; --`
	stmt, err := RPC(snap, fn, &query.Query{}, []ArgValue{
		{Name: "a", Text: payload},
		{Name: "b", Text: "0"},
	}, ReadOptions{})
	if err != nil {
		t.Fatalf("RPC: %v", err)
	}
	if strings.Contains(strings.ToLower(stmt.SQL), "drop table") || strings.Contains(stmt.SQL, ";") {
		t.Fatalf("payload leaked into SQL:\n%s", stmt.SQL)
	}
	if len(stmt.Args) != 2 || stmt.Args[0] != payload {
		t.Fatalf("Args = %#v", stmt.Args)
	}
}
