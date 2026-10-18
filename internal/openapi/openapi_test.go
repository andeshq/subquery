package openapi

import (
	"encoding/json/v2"
	"testing"

	"github.com/andeshq/subquery/internal/schema"
)

func testDoc(t *testing.T, opts Options) *Document {
	t.Helper()
	films := schema.RelID{Schema: "public", Name: "films"}
	snap := schema.Build(1, []string{"public"}, schema.Catalog{
		Relations: []schema.CatalogRelation{{
			Schema: "public", Name: "films", Kind: schema.KindTable,
			Columns: []schema.CatalogColumn{
				{Name: "id", TypeName: "integer", CastType: `"pg_catalog"."int4"`, NotNull: true, Generated: true},
				{Name: "title", TypeName: "character varying(200)", CastType: `"pg_catalog"."varchar"`, NotNull: true},
				{Name: "year", TypeName: "integer", CastType: `"pg_catalog"."int4"`},
				{Name: "tags", TypeName: "text[]", CastType: `"pg_catalog"."text"[]`},
				{Name: "meta", TypeName: "jsonb", CastType: `"pg_catalog"."jsonb"`},
			},
		}},
		Constraints: []schema.CatalogConstraint{{
			Name: "films_pkey", Type: 'p', Rel: films, Columns: []string{"id"},
		}},
		Functions: []schema.CatalogFunction{
			{Schema: "public", Name: "film_label", Volatility: 's',
				ReturnCastType: `"pg_catalog"."text"`, ReturnTypeName: "text",
				Args: []schema.CatalogFunctionArg{{Name: "films", Mode: 'i', Position: 1, CastType: `"public"."films"`, Relation: films}}},
			{Schema: "public", Name: "add_them", Volatility: 'i',
				ReturnCastType: `"pg_catalog"."int4"`, ReturnTypeName: "integer",
				Args: []schema.CatalogFunctionArg{
					{Name: "a", Mode: 'i', Position: 1, CastType: `"pg_catalog"."int4"`},
					{Name: "b", Mode: 'i', Position: 2, CastType: `"pg_catalog"."int4"`},
				}},
			{Schema: "public", Name: "bump", Volatility: 'v', ReturnsVoid: true,
				Args: []schema.CatalogFunctionArg{{Name: "n", Mode: 'i', Position: 1, CastType: `"pg_catalog"."int4"`}}},
		},
	})
	if opts.Version == "" {
		opts.Version = "test"
	}
	if opts.Host == "" {
		opts.Host = "api.example"
	}
	return Build(snap, opts)
}

func TestBuildPathsAndDefinitions(t *testing.T) {
	doc := testDoc(t, Options{})

	if doc.Swagger != "2.0" || doc.BasePath != "/" || doc.Host != "api.example" {
		t.Fatalf("document header = %+v", doc)
	}

	films, ok := doc.Paths["/films"]
	if !ok {
		t.Fatal("/films path is missing")
	}
	if films.Get == nil || films.Post == nil || films.Patch == nil || films.Delete == nil {
		t.Errorf("films should answer all read and write verbs: %+v", films)
	}
	if films.Put == nil {
		t.Error("films has a primary key, so PUT should be offered")
	}
	if films.Get.Responses["200"].Schema.Items.Ref != "#/definitions/films" {
		t.Errorf("get response = %+v", films.Get.Responses["200"])
	}

	definition, ok := doc.Definitions["films"]
	if !ok {
		t.Fatal("films definition is missing")
	}
	if definition.Properties["title"].Type != "string" {
		t.Errorf("title = %+v", definition.Properties["title"])
	}
	if definition.Properties["year"].Type != "integer" {
		t.Errorf("year = %+v", definition.Properties["year"])
	}
	if definition.Properties["tags"].Type != "array" || definition.Properties["tags"].Items.Type != "string" {
		t.Errorf("tags = %+v", definition.Properties["tags"])
	}
	if definition.Properties["meta"].Type != "object" {
		t.Errorf("meta = %+v", definition.Properties["meta"])
	}
	// A computed field appears as a property.
	if _, ok := definition.Properties["film_label"]; !ok {
		t.Errorf("computed field missing from the definition: %+v", definition.Properties)
	}
	// id is generated and title is the only required non-defaulted column.
	if len(definition.Required) != 1 || definition.Required[0] != "title" {
		t.Errorf("required = %v, want [title]", definition.Required)
	}

	// The query parameter list carries the shared parameters and one per column.
	var hasSelect, hasTitle bool
	for _, param := range films.Get.Parameters {
		switch {
		case param.Ref == "#/parameters/select":
			hasSelect = true
		case param.Name == "title":
			hasTitle = true
		}
	}
	if !hasSelect || !hasTitle {
		t.Errorf("get parameters = %+v", films.Get.Parameters)
	}
}

func TestBuildFunctionPaths(t *testing.T) {
	doc := testDoc(t, Options{})

	// A computed field is not described as an RPC path; its function is.
	add, ok := doc.Paths["/rpc/add_them"]
	if !ok {
		t.Fatal("/rpc/add_them is missing")
	}
	if add.Get == nil || add.Post == nil {
		t.Errorf("an immutable function should answer GET and POST: %+v", add)
	}
	if add.Post.Responses["200"].Schema.Type != "integer" {
		t.Errorf("scalar response = %+v", add.Post.Responses["200"].Schema)
	}

	bump, ok := doc.Paths["/rpc/bump"]
	if !ok {
		t.Fatal("/rpc/bump is missing")
	}
	if bump.Get != nil {
		t.Errorf("a volatile function must not offer GET: %+v", bump.Get)
	}
	if bump.Post.Responses["200"].Description != "No content" {
		t.Errorf("void response = %+v", bump.Post.Responses["200"])
	}
}

func TestBuildPrivilegeFiltering(t *testing.T) {
	doc := testDoc(t, Options{
		IncludeRel:  func(rel *schema.Relation) bool { return rel.ID.Name == "films" },
		IncludeFunc: func(fn *schema.Function) bool { return fn.ID.Name == "add_them" },
	})
	if _, ok := doc.Paths["/films"]; !ok {
		t.Error("/films should be included")
	}
	if _, ok := doc.Paths["/rpc/add_them"]; !ok {
		t.Error("/rpc/add_them should be included")
	}
	if _, ok := doc.Paths["/rpc/bump"]; ok {
		t.Error("/rpc/bump should be filtered out")
	}
}

func TestBuildProxyURI(t *testing.T) {
	doc := testDoc(t, Options{ProxyURI: "https://api.example.com/v1"})
	if doc.Host != "api.example.com" || doc.BasePath != "/v1" {
		t.Errorf("host = %q, basePath = %q", doc.Host, doc.BasePath)
	}
	if len(doc.Schemes) != 1 || doc.Schemes[0] != "https" {
		t.Errorf("schemes = %v", doc.Schemes)
	}
}

// TestDocumentIsValidJSON asserts the document marshals cleanly, since a bad
// schema would only surface in a client.
func TestDocumentIsValidJSON(t *testing.T) {
	payload, err := json.Marshal(testDoc(t, Options{}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded["swagger"] != "2.0" {
		t.Errorf("swagger = %v", decoded["swagger"])
	}
}

func TestSwaggerType(t *testing.T) {
	cases := []struct {
		typeName string
		want     string
		format   string
	}{
		{"integer", "integer", "int32"},
		{"bigint", "integer", "int64"},
		{"numeric(3,1)", "number", ""},
		{"double precision", "number", "double"},
		{"boolean", "boolean", ""},
		{"timestamp with time zone", "string", "date-time"},
		{"uuid", "string", "uuid"},
		{"jsonb", "object", ""},
		{"character varying(20)", "string", ""},
		{"text[]", "array", ""},
		{"tsvector", "string", ""},
	}
	for _, tc := range cases {
		got, format, _ := swaggerType(tc.typeName)
		if got != tc.want || format != tc.format {
			t.Errorf("swaggerType(%q) = (%q, %q), want (%q, %q)", tc.typeName, got, format, tc.want, tc.format)
		}
	}
}
