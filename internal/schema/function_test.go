package schema

import (
	"testing"
)

// functionSnapshot builds a snapshot exercising overloads, defaults, variadic
// arguments, and a computed field.
func functionSnapshot(t *testing.T) *Snapshot {
	t.Helper()
	films := RelID{Schema: "public", Name: "films"}
	int4 := `"pg_catalog"."int4"`
	return Build(1, []string{"public"}, Catalog{
		Relations: []CatalogRelation{{
			Schema: "public", Name: "films", Kind: KindTable,
			Columns: []CatalogColumn{
				{Name: "id", TypeName: "integer", CastType: int4},
				{Name: "title", TypeName: "text", CastType: `"pg_catalog"."text"`},
			},
		}},
		Constraints: []CatalogConstraint{
			{Name: "films_pkey", Type: 'p', Rel: films, Columns: []string{"id"}},
		},
		Functions: []CatalogFunction{
			{Schema: "public", Name: "add_them", Volatility: 'i', ReturnCastType: int4, ReturnTypeName: "integer",
				Args: []CatalogFunctionArg{
					{Name: "a", Mode: 'i', Position: 1, CastType: int4},
					{Name: "b", Mode: 'i', Position: 2, CastType: int4},
				}},
			{Schema: "public", Name: "with_defaults", Volatility: 'i', ReturnCastType: int4, ReturnTypeName: "integer", DefaultCount: 2,
				Args: []CatalogFunctionArg{
					{Name: "a", Mode: 'i', Position: 1, CastType: int4},
					{Name: "b", Mode: 'i', Position: 2, CastType: int4},
					{Name: "c", Mode: 'i', Position: 3, CastType: `"pg_catalog"."text"`},
				}},
			{Schema: "public", Name: "plus_one", Volatility: 'i', ReturnCastType: `"pg_catalog"."int4"[]`, ReturnTypeName: "integer[]",
				Args: []CatalogFunctionArg{{Name: "v", Mode: 'v', Position: 1, CastType: `"pg_catalog"."int4"[]`}}},
			{Schema: "public", Name: "best_films", Volatility: 's', ReturnsSet: true, ReturnRelation: films},
			{Schema: "public", Name: "touch", Volatility: 'v', ReturnsVoid: true, ReturnTypeName: "void",
				Args: []CatalogFunctionArg{{Name: "n", Mode: 'i', Position: 1, CastType: int4}}},
			{Schema: "public", Name: "mult_them", Volatility: 'i', ReturnCastType: int4, ReturnTypeName: "integer",
				Args: []CatalogFunctionArg{{Name: "", Mode: 'i', Position: 1, CastType: `"pg_catalog"."jsonb"`}}},
			{Schema: "public", Name: "film_label", Volatility: 's', ReturnCastType: `"pg_catalog"."text"`, ReturnTypeName: "text",
				Args: []CatalogFunctionArg{{Name: "films", Mode: 'i', Position: 1, CastType: `"public"."films"`, Relation: films}}},
		},
	})
}

func TestResolveFunctionByName(t *testing.T) {
	snap := functionSnapshot(t)

	fn, schemaErr := snap.ResolveFunction("add_them", []string{"a", "b"})
	if schemaErr != nil {
		t.Fatalf("ResolveFunction: %v", schemaErr)
	}
	if fn.ID.Name != "add_them" || len(fn.InputArgs()) != 2 {
		t.Fatalf("resolved %+v", fn)
	}
	if fn.Mutable() {
		t.Error("an immutable function must not be mutable")
	}
	if _, ok := fn.Arg("a"); !ok {
		t.Error("Arg(a) not found")
	}
	if _, ok := fn.Arg("z"); ok {
		t.Error("Arg(z) should not be found")
	}
	if signature := fn.Signature(); signature == "" {
		t.Error("Signature is empty")
	}
}

func TestResolveFunctionWithDefaults(t *testing.T) {
	snap := functionSnapshot(t)

	// Omitting defaulted arguments resolves.
	fn, schemaErr := snap.ResolveFunction("with_defaults", []string{"a"})
	if schemaErr != nil {
		t.Fatalf("ResolveFunction: %v", schemaErr)
	}
	inputs := fn.InputArgs()
	if len(inputs) != 3 || inputs[0].HasDefault || !inputs[1].HasDefault || !inputs[2].HasDefault {
		t.Fatalf("default marking = %+v", inputs)
	}

	// Omitting a required argument does not.
	if _, schemaErr := snap.ResolveFunction("with_defaults", nil); schemaErr == nil {
		t.Fatal("omitting a required argument should not resolve")
	}
}

func TestResolveFunctionRejectsUnknownArguments(t *testing.T) {
	snap := functionSnapshot(t)
	_, schemaErr := snap.ResolveFunction("add_them", []string{"a", "zzz"})
	if schemaErr == nil || schemaErr.Code != "PGRST202" {
		t.Fatalf("error = %v, want PGRST202", schemaErr)
	}
	_, schemaErr = snap.ResolveFunction("nope", nil)
	if schemaErr == nil || schemaErr.Code != "PGRST202" {
		t.Fatalf("error = %v, want PGRST202", schemaErr)
	}
}

func TestResolveFunctionAmbiguousOverloads(t *testing.T) {
	int4 := `"pg_catalog"."int4"`
	snap := Build(1, []string{"public"}, Catalog{
		Functions: []CatalogFunction{
			{Schema: "public", Name: "f", Volatility: 'i', ReturnCastType: int4,
				Args: []CatalogFunctionArg{{Name: "x", Mode: 'i', Position: 1, CastType: int4}}},
			{Schema: "public", Name: "f", Volatility: 'i', ReturnCastType: int4,
				Args: []CatalogFunctionArg{{Name: "x", Mode: 'i', Position: 1, CastType: `"pg_catalog"."int8"`}}},
		},
	})
	_, schemaErr := snap.ResolveFunction("f", []string{"x"})
	if schemaErr == nil || schemaErr.Code != "PGRST203" {
		t.Fatalf("error = %v, want PGRST203", schemaErr)
	}
	if schemaErr.Hint == nil {
		t.Error("an ambiguous overload should carry a hint")
	}
}

func TestResolveUnnamedSingleArgument(t *testing.T) {
	snap := functionSnapshot(t)
	fn, schemaErr := snap.ResolveFunction("mult_them", []string{"x", "y"})
	if schemaErr != nil {
		t.Fatalf("ResolveFunction: %v", schemaErr)
	}
	if arg, ok := unnamedSingleArgFor(fn); !ok || arg.CastType != `"pg_catalog"."jsonb"` {
		t.Fatalf("unnamed single argument = %+v, %v", arg, ok)
	}
}

func TestFunctionReturnClassification(t *testing.T) {
	snap := functionSnapshot(t)

	scalar, _ := snap.ResolveFunction("add_them", []string{"a", "b"})
	if scalar.Kind != ReturnScalar || scalar.ScalarCast == "" || scalar.ResultTypeName != "integer" {
		t.Errorf("scalar = %+v", scalar)
	}

	table, _ := snap.ResolveFunction("best_films", nil)
	if table.Kind != ReturnTable || table.Returns == nil || table.Returns.ID.Name != "films" {
		t.Errorf("table-valued = %+v", table)
	}

	void, _ := snap.ResolveFunction("touch", []string{"n"})
	if void.Kind != ReturnVoid || !void.Mutable() {
		t.Errorf("void = %+v", void)
	}
}

func TestComputedFieldRegistration(t *testing.T) {
	snap := functionSnapshot(t)
	films, ok := snap.Relation("films")
	if !ok {
		t.Fatal("films is not exposed")
	}

	fn, ok := films.ComputedField("film_label")
	if !ok || fn.ScalarCast != `"pg_catalog"."text"` {
		t.Fatalf("ComputedField = %+v, %v", fn, ok)
	}
	if _, ok := films.ComputedField("add_them"); ok {
		t.Error("a function that does not take the row type must not be a computed field")
	}
	if cast, ok := films.FieldCastType("film_label"); !ok || cast != `"pg_catalog"."text"` {
		t.Errorf("FieldCastType(film_label) = %q, %v", cast, ok)
	}
	if cast, ok := films.FieldCastType("title"); !ok || cast != `"pg_catalog"."text"` {
		t.Errorf("FieldCastType(title) = %q, %v", cast, ok)
	}
	if _, ok := films.FieldCastType("nope"); ok {
		t.Error("FieldCastType(nope) should not resolve")
	}
	if name, ok := films.FieldTypeName("id"); !ok || name != "integer" {
		t.Errorf("FieldTypeName(id) = %q, %v", name, ok)
	}
	if names := films.ComputedFields(); len(names) != 1 {
		t.Errorf("ComputedFields = %v", names)
	}
}

func TestFunctionOrderIsDeterministic(t *testing.T) {
	snap := functionSnapshot(t)
	first := snap.FunctionOrder()
	second := snap.FunctionOrder()
	if len(first) == 0 || len(first) != len(second) {
		t.Fatalf("orders = %v, %v", first, second)
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("order is not stable: %v vs %v", first, second)
		}
	}
	for i := 1; i < len(first); i++ {
		if first[i-1].Name > first[i].Name {
			t.Errorf("order is not sorted: %v", first)
			break
		}
	}
}

// unnamedSingleArgFor reaches the unexported helper through its package.
func unnamedSingleArgFor(fn *Function) (FunctionArg, bool) { return fn.unnamedSingleArg() }
