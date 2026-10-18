package sqlgen

import (
	"net/url"
	"testing"

	"github.com/andeshq/subquery/internal/query"
)

func benchmarkQuery(b *testing.B, raw string) *query.Query {
	b.Helper()
	values, err := url.ParseQuery(raw)
	if err != nil {
		b.Fatalf("ParseQuery(%q): %v", raw, err)
	}
	q, err := query.Parse(values)
	if err != nil {
		b.Fatalf("Parse(%q): %v", raw, err)
	}
	return q
}

func BenchmarkRead(b *testing.B) {
	snap, rel := testFilms(b)
	tests := []struct {
		name  string
		query string
	}{
		{"all rows", ""},
		{"projection and order", "select=id,title&order=year.desc&limit=25"},
		{"filters", "year=gte.1900&title=like.*Pulp*"},
		{"embedding", "select=title,directors(last_name),roles(character)"},
		{"aggregate", "select=year,count()"},
		{"computed field", "select=title,film_label&order=film_label"},
	}
	for _, tc := range tests {
		b.Run(tc.name, func(b *testing.B) {
			q := benchmarkQuery(b, tc.query)
			b.ReportAllocs()
			for b.Loop() {
				if _, err := Read(snap, rel, q, ReadOptions{}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkWriteInsert(b *testing.B) {
	snap := writeRelationSnapshot(b)
	rel := notesRelationTB(b, snap)
	payload := []byte(`[{"title":"a","score":1}]`)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Write(snap, rel, &WriteQuery{
			Kind:    WriteInsert,
			Columns: []string{"title", "score"},
			Payload: payload,
		}, WriteOptions{Return: ReturnRepresentation}); err != nil {
			b.Fatal(err)
		}
	}
}
