package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// objectMediaType asks for exactly one JSON object instead of an array.
const objectMediaType = "application/vnd.pgrst.object+json"

// TestLiveSingularRead asserts the object media type returns one object and
// rejects any result that is not exactly one row.
func TestLiveSingularRead(t *testing.T) {
	s := newLiveServer(t)
	accept := map[string]string{"Accept": objectMediaType}

	rec := get(t, s, "/todos?id=eq.1&select=id,task", accept)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != objectMediaType+"; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	body := strings.TrimSpace(rec.Body.String())
	if !strings.HasPrefix(body, "{") || !strings.HasSuffix(body, "}") {
		t.Fatalf("body is not a single object: %s", body)
	}
	if !strings.Contains(body, `"id":1`) {
		t.Errorf("body = %s", body)
	}

	// No rows.
	rec = get(t, s, "/todos?id=eq.999", accept)
	if rec.Code != http.StatusNotAcceptable {
		t.Fatalf("empty status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if code := decodeError(t, rec)["code"]; code != "PGRST116" {
		t.Errorf("empty code = %v, want PGRST116", code)
	}

	// More than one row.
	rec = get(t, s, "/todos", accept)
	if rec.Code != http.StatusNotAcceptable {
		t.Fatalf("multi status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if code := decodeError(t, rec)["code"]; code != "PGRST116" {
		t.Errorf("multi code = %v, want PGRST116", code)
	}

	// The default stays an array with the JSON content type.
	rec = get(t, s, "/todos?id=eq.1", nil)
	if rec.Code != http.StatusOK || !strings.HasPrefix(strings.TrimSpace(rec.Body.String()), "[") {
		t.Errorf("default body = %s", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("default Content-Type = %q", ct)
	}

	// A media type Subquery cannot render is still refused.
	if rec := get(t, s, "/todos", map[string]string{"Accept": "text/plain"}); rec.Code != http.StatusNotAcceptable {
		t.Errorf("plain status = %d, want 406", rec.Code)
	}
}

// TestLiveSingularWrite asserts a representation write returns one object.
func TestLiveSingularWrite(t *testing.T) {
	s := newLiveServer(t)

	patch := func(target string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPatch, target, strings.NewReader(`{"done":true}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Prefer", "return=representation")
		req.Header.Set("Accept", objectMediaType)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		return rec
	}

	rec := patch("/todos?id=eq.1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != objectMediaType+"; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	if body := strings.TrimSpace(rec.Body.String()); !strings.HasPrefix(body, "{") {
		t.Fatalf("body is not an object: %s", body)
	}

	// A write that matched no rows cannot be an object.
	if rec := patch("/todos?id=eq.999"); rec.Code != http.StatusNotAcceptable {
		t.Fatalf("empty status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

// TestLiveSingularRPC asserts a table-returning call honors the object media
// type, and that an empty result is refused.
func TestLiveSingularRPC(t *testing.T) {
	s := newLiveServer(t)
	accept := map[string]string{"Accept": objectMediaType}

	// film_titles returns one row.
	rec := get(t, s, "/rpc/film_titles", accept)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != objectMediaType+"; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	if body := strings.TrimSpace(rec.Body.String()); !strings.HasPrefix(body, "{") {
		t.Fatalf("body is not an object: %s", body)
	}

	// A filter that matches nothing cannot be an object.
	rec = get(t, s, "/rpc/film_titles?year=eq.1900", accept)
	if rec.Code != http.StatusNotAcceptable {
		t.Fatalf("empty status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if code := decodeError(t, rec)["code"]; code != "PGRST116" {
		t.Errorf("empty code = %v, want PGRST116", code)
	}
}
