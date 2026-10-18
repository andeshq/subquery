package query

import (
	"net/url"
	"testing"
)

// benchQueries covers the shapes a real client sends most often.
var benchQueries = []string{
	"",
	"select=id,title",
	"select=title&order=year.desc&limit=25&offset=50",
	"year=gte.1900&title=like.*Pulp*",
	"or=(year.lt.1900,year.gt.1990)&tags=cs.{crime}",
	"select=title,directors(last_name),roles(character)&roles.order=character",
	"meta->>genre=eq.crime&select=title,genre:meta->>genre",
}

func BenchmarkParse(b *testing.B) {
	parsed := make([]url.Values, 0, len(benchQueries))
	for _, raw := range benchQueries {
		values, err := url.ParseQuery(raw)
		if err != nil {
			b.Fatalf("ParseQuery(%q): %v", raw, err)
		}
		parsed = append(parsed, values)
	}

	b.ReportAllocs()
	for b.Loop() {
		for _, values := range parsed {
			if _, err := Parse(values); err != nil {
				b.Fatal(err)
			}
		}
	}
}

func BenchmarkParseSingleFilter(b *testing.B) {
	values := url.Values{"year": {"gte.1900"}}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Parse(values); err != nil {
			b.Fatal(err)
		}
	}
}
