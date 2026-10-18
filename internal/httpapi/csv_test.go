package httpapi

import (
	"net/http"
	"testing"

	"github.com/andeshq/subquery/internal/config"
)

// TestLiveCSVRead asserts CSV output matches PostgREST's shape: a header line,
// one line per row, no trailing newline, fields quoted only when needed.
func TestLiveCSVRead(t *testing.T) {
	s := newLiveServer(t)
	accept := map[string]string{"Accept": "text/csv"}

	rec := get(t, s, "/films?select=id,title,yr:year,big:id::int8", accept)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/csv; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	if want := "id,title,yr,big\n1,\"Pulp Fiction\",1994,1"; rec.Body.String() != want {
		t.Errorf("body = %q, want %q", rec.Body.String(), want)
	}

	// An empty result is a bare newline.
	if rec := get(t, s, "/todos?id=eq.999&select=id", accept); rec.Body.String() != "\n" {
		t.Errorf("empty body = %q, want a newline", rec.Body.String())
	}

	// count=exact still reports the total and the partial-content window.
	rec = get(t, s, "/todos?select=id&limit=1", map[string]string{"Accept": "text/csv", "Prefer": "count=exact"})
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("count status = %d, want 206, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Range"); got != "0-0/2" {
		t.Errorf("Content-Range = %q, want 0-0/2", got)
	}
}

// TestLiveCSVRejectsComplexSelection asserts CSV refuses a selection whose
// header would not be a plain column name.
func TestLiveCSVRejectsComplexSelection(t *testing.T) {
	// Aggregates are enabled so that selection reaches the CSV check rather than
	// the aggregates gate.
	s := newLiveServerWith(t, func(c *config.Config) { c.DBAggregatesEnabled = true })
	accept := map[string]string{"Accept": "text/csv"}

	targets := []string{
		"/films?select=id,directors(last_name)",
		"/films?select=year,count()",
		"/films?select=id,meta->>genre",
	}
	for _, target := range targets {
		rec := get(t, s, target, accept)
		if rec.Code != http.StatusNotAcceptable {
			t.Errorf("%s: status = %d, want 406, body = %s", target, rec.Code, rec.Body.String())
			continue
		}
		if code := decodeError(t, rec)["code"]; code != "PGRST107" {
			t.Errorf("%s: code = %v, want PGRST107", target, code)
		}
	}
}
