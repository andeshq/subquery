package sqlgen

import (
	"net/url"
	"strings"
	"testing"

	"github.com/andeshq/subquery/internal/query"
	"github.com/andeshq/subquery/internal/schema"
)

// FuzzRead asserts three invariants for every query string that parses:
//
//  1. generation never panics;
//  2. the generated SQL contains no statement separator and no single quote,
//     because every value is a bind parameter and every literal comes from a
//     fixed vocabulary;
//  3. double quotes are balanced, because identifiers are always quoted; and
//  4. generation is deterministic, so the prepared-statement cache is effective.
func FuzzRead(f *testing.F) {
	for _, seed := range []string{
		"",
		"select=*",
		"select=title,year",
		"select=fullTitle:title,id::int8,meta->>rating",
		"select=id::text);drop table films;--",
		"select=title,directors(last_name)",
		"select=title,roles(character,actors(first_name))",
		"select=title,...directors(last_name)",
		"select=count()",
		"select=year,count(),max(id)",
		"select=title,film_label",
		"title=eq.Pulp Fiction",
		"title=eq.'; drop table films; --",
		"title=like.*a*b*",
		"title=ilike.%22quoted%22",
		"id=in.(1,2,%22a,b%22)",
		"tags=cs.{a,b}",
		"year=gte.1900",
		"rating=is.null",
		"rating=not.is.null",
		"or=(year.lt.1900,year.gt.1990)",
		"not.and=(year.gte.1900,year.lte.2000)",
		"meta->>genre=eq.crime",
		"meta->phones->0->>number=eq.1",
		"order=year.desc.nullslast,title.asc",
		"limit=10&offset=5",
		"select=title&roles.order=character.desc&roles.limit=1",
		"title=fts(english).fiction",
		"not.a=eq.1",
		"a=like(any).{x*,y*}",
		"a=eq(all).{1,2}",
	} {
		f.Add(seed)
	}

	snap, rel := testFilms(f)

	f.Fuzz(func(t *testing.T, raw string) {
		values, err := url.ParseQuery(raw)
		if err != nil {
			return
		}
		q, err := query.Parse(values)
		if err != nil {
			return
		}
		first, err := Read(snap, rel, q, ReadOptions{})
		if err != nil {
			return
		}
		assertSafeSQL(t, first.SQL)

		// Re-parse and re-generate: the result must be byte-identical.
		again, err := url.ParseQuery(raw)
		if err != nil {
			return
		}
		q2, err := query.Parse(again)
		if err != nil {
			return
		}
		second, err := Read(snap, rel, q2, ReadOptions{})
		if err != nil {
			return
		}
		if first.SQL != second.SQL {
			t.Fatalf("generation is not deterministic for %q:\n%s\n%s", raw, first.SQL, second.SQL)
		}
	})
}

// assertSafeSQL checks the structural invariants the generator promises.
//
// Identifiers are removed first, because a quoted identifier may legitimately
// contain anything. What remains is the template text, which may contain bind
// placeholders and exactly one kind of literal. Any other quoting there would
// mean request text reached the statement.
func assertSafeSQL(t *testing.T, sql string) {
	t.Helper()
	if strings.Count(sql, `"`)%2 != 0 {
		t.Fatalf("unbalanced identifier quoting:\n%s", sql)
	}

	rest := stripQuotedIdentifiers(sql)
	if strings.Contains(rest, ";") {
		t.Fatalf("statement separator in generated SQL:\n%s", sql)
	}
	if strings.Contains(rest, "\\") {
		t.Fatalf("escape character in generated SQL:\n%s", sql)
	}
	// '[]'::json is the only literal the generator emits.
	if withoutLiterals := strings.ReplaceAll(rest, `'[]'::json`, "<empty-array>"); strings.Contains(withoutLiterals, "'") {
		t.Fatalf("quoting outside identifiers and known literals:\n%s", sql)
	}
}

// stripQuotedIdentifiers removes double-quoted identifiers, honouring the ""
// escape that quoting uses for an embedded quote.
func stripQuotedIdentifiers(sql string) string {
	var b strings.Builder
	inQuote := false
	for i := 0; i < len(sql); i++ {
		if sql[i] == '"' {
			if inQuote && i+1 < len(sql) && sql[i+1] == '"' {
				i++
				continue
			}
			inQuote = !inQuote
			continue
		}
		if !inQuote {
			b.WriteByte(sql[i])
		}
	}
	return b.String()
}

// FuzzValidCast asserts that nothing accepted as a cast can escape the cast
// position.
func FuzzValidCast(f *testing.F) {
	for _, seed := range []string{
		"text", "int8", "numeric(10,2)", "text[]", "varchar(20)",
		"pg_catalog.int4", "double precision", "timestamp with time zone",
		`"text"`, "text;", "text)", "int; select 1", "count(*)", "a.b.c",
		"text drop", "", "text[]::text", "numeric(1;2)", "1", "_x", "$1",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, cast string) {
		if !validCast(cast) {
			return
		}
		for _, forbidden := range []string{";", "'", `"`, "\\", "--", "/*", "$"} {
			if strings.Contains(cast, forbidden) {
				t.Fatalf("validCast accepted %q containing %q", cast, forbidden)
			}
		}
		if strings.Count(cast, "(") != strings.Count(cast, ")") {
			t.Fatalf("validCast accepted %q with unbalanced parentheses", cast)
		}
		if strings.Count(cast, "(") > 1 {
			t.Fatalf("validCast accepted %q with more than one length specification", cast)
		}
	})
}

// FuzzWrite asserts the same invariants for generated write statements.
func FuzzWrite(f *testing.F) {
	for _, seed := range []string{
		"title", "title,score", "nope", `"title"`, "title);drop table notes;--",
		"title,title", "", "*",
	} {
		f.Add(seed)
	}

	snap := writeRelationSnapshot(f)
	rel := notesRelationTB(f, snap)

	f.Fuzz(func(t *testing.T, columns string) {
		statement, err := Write(snap, rel, &WriteQuery{
			Kind:    WriteInsert,
			Columns: strings.Split(columns, ","),
			Payload: []byte(`[{"title":"x"}]`),
		}, WriteOptions{Return: ReturnRepresentation})
		if err != nil {
			return
		}
		assertSafeSQL(t, statement.SQL)
	})
}

func notesRelationTB(t testing.TB, snap *schema.Snapshot) *schema.Relation {
	t.Helper()
	rel, ok := snap.Relation("notes")
	if !ok {
		t.Fatal("notes is not in the snapshot")
	}
	return rel
}
