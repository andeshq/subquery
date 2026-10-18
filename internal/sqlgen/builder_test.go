package sqlgen

import (
	"strings"
	"testing"

	"github.com/andeshq/subquery/internal/query"
)

func TestArrayLiteralQuoting(t *testing.T) {
	tests := []struct {
		values []string
		want   string
	}{
		{nil, "{}"},
		{[]string{"a", "b"}, "{a,b}"},
		{[]string{""}, `{""}`},
		{[]string{"null"}, `{"null"}`},
		{[]string{"NULL"}, `{"NULL"}`},
		{[]string{"a,b"}, `{"a,b"}`},
		{[]string{"{x}"}, `{"{x}"}`},
		{[]string{"a b"}, `{"a b"}`},
		{[]string{`say "hi"`}, `{"say \"hi\""}`},
		{[]string{`back\slash`}, `{"back\\slash"}`},
		{[]string{"tab\there"}, `{"tab	here"}`},
		{[]string{"plain", "with,comma", "and\"quote"}, `{plain,"with,comma","and\"quote"}`},
	}
	for _, tc := range tests {
		if got := ArrayLiteral(tc.values); got != tc.want {
			t.Errorf("ArrayLiteral(%q) = %q, want %q", tc.values, got, tc.want)
		}
	}
}

func TestComparisonCastType(t *testing.T) {
	text := []query.JSONStep{{Key: "k", Text: true}}
	jsonStep := []query.JSONStep{{Key: "k"}}
	chain := []query.JSONStep{{Key: "a"}, {Key: "b", Text: true}}

	tests := []struct {
		name     string
		base     string
		path     []query.JSONStep
		baseJSON bool
		want     string
	}{
		{"no path returns the base", `"pg_catalog"."int4"`, nil, false, `"pg_catalog"."int4"`},
		{"text extraction yields text", `"pg_catalog"."jsonb"`, text, true, `"pg_catalog"."text"`},
		{"json column keeps json", `"pg_catalog"."json"`, jsonStep, true, `"pg_catalog"."json"`},
		{"non-json base becomes jsonb", `"pg_catalog"."text"`, jsonStep, false, `"pg_catalog"."jsonb"`},
		{"last step decides", `"pg_catalog"."jsonb"`, chain, true, `"pg_catalog"."text"`},
	}
	for _, tc := range tests {
		if got := comparisonCastType(tc.base, tc.path, tc.baseJSON); got != tc.want {
			t.Errorf("%s: comparisonCastType = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestValidCastTable(t *testing.T) {
	valid := []string{
		"text", "int8", "numeric(10,2)", "text[]", "int4[]", "varchar(20)",
		"pg_catalog.int4", "public.mood", "double precision", "character varying",
		"timestamp with time zone", "timestamp without time zone", "time with time zone",
		"bit varying", "numeric", "_x",
	}
	for _, cast := range valid {
		if !validCast(cast) {
			t.Errorf("validCast(%q) = false, want true", cast)
		}
	}
	invalid := []string{
		"", "text;", "text)", "(text", `"text"`, "numeric(1;2)", "numeric(a)",
		"a.b.c", "text drop", "1", "$1", "text'", "text--", "text[]::text", "count(*)",
		"numeric(1,2,3)", "text(1)extra",
	}
	for _, cast := range invalid {
		if validCast(cast) {
			t.Errorf("validCast(%q) = true, want false", cast)
		}
	}
	// Whitespace around the whole value is tolerated, but not inside an identifier.
	if !validCast("  text  ") {
		t.Error("surrounding whitespace should be tolerated")
	}
}

func TestInsertWithNoColumnsUsesDefaults(t *testing.T) {
	snap := writeRelationSnapshot(t)
	rel := notesRelationTB(t, snap)
	stmt, err := Write(snap, rel, &WriteQuery{
		Kind:    WriteInsert,
		Columns: nil,
		Payload: []byte(`[{}]`),
	}, WriteOptions{})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	assertContains(t, &Statement{SQL: stmt.SQL, Args: stmt.Args}, `insert into "public"."notes" default values`)
	if len(stmt.Args) != 0 {
		t.Errorf("a default-values insert should bind nothing, got %#v", stmt.Args)
	}
}

func TestInsertWithoutColumnsIsRejectedForUpsert(t *testing.T) {
	snap := writeRelationSnapshot(t)
	rel := notesRelationTB(t, snap)
	if _, err := Write(snap, rel, &WriteQuery{Kind: WriteUpsert, Payload: []byte(`[{}]`)}, WriteOptions{}); err == nil {
		t.Fatal("an upsert with no columns should be rejected")
	}
}

func TestBuilderWriteAndString(t *testing.T) {
	builder := &Builder{}
	builder.Write("select ")
	first := builder.Param("a")
	builder.Write(first)
	second := builder.Param("b")
	builder.Write("," + second)
	if got := builder.String(); !strings.Contains(got, "$1") || !strings.Contains(got, "$2") {
		t.Errorf("builder output = %q", got)
	}
	if args := builder.Args(); len(args) != 2 || args[0] != "a" || args[1] != "b" {
		t.Errorf("builder args = %#v", args)
	}
}
