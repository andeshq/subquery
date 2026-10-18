package httpapi

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func rpc(t *testing.T, s *Server, method, target, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func decodeValue(t *testing.T, rec *httptest.ResponseRecorder) any {
	t.Helper()
	var value any
	if err := json.Unmarshal(rec.Body.Bytes(), &value); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return value
}

func TestLiveRPCScalar(t *testing.T) {
	s := newLiveServer(t)

	rec := rpc(t, s, http.MethodGet, "/rpc/add_them?a=1&b=2", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := decodeValue(t, rec); got != float64(3) {
		t.Errorf("GET /rpc/add_them = %v, want 3", got)
	}

	rec = rpc(t, s, http.MethodPost, "/rpc/add_them", `{"a":10,"b":32}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := decodeValue(t, rec); got != float64(42) {
		t.Errorf("POST /rpc/add_them = %v, want 42", got)
	}
}

func TestLiveRPCDefaultArguments(t *testing.T) {
	s := newLiveServer(t)

	rec := rpc(t, s, http.MethodGet, "/rpc/greet_user", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := decodeValue(t, rec); got != "Hello guest!" {
		t.Errorf("default argument = %v", got)
	}

	// with_defaults(a, b default 5, c default 'x') called with only a.
	rec = rpc(t, s, http.MethodGet, "/rpc/with_defaults?a=1", "", nil)
	if got := decodeValue(t, rec); got != float64(6) {
		t.Errorf("with_defaults(a=1) = %v, want 6", got)
	}
	rec = rpc(t, s, http.MethodPost, "/rpc/with_defaults", `{"a":1,"b":41}`, nil)
	if got := decodeValue(t, rec); got != float64(42) {
		t.Errorf("with_defaults(a=1,b=41) = %v, want 42", got)
	}
}

func TestLiveRPCUnnamedJSONArgument(t *testing.T) {
	s := newLiveServer(t)
	rec := rpc(t, s, http.MethodPost, "/rpc/mult_them", `{"x":4,"y":2}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := decodeValue(t, rec); got != float64(8) {
		t.Errorf("mult_them = %v, want 8", got)
	}
}

func TestLiveRPCVariadic(t *testing.T) {
	s := newLiveServer(t)

	rec := rpc(t, s, http.MethodPost, "/rpc/plus_one", `{"v":[1,2,3]}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := decodeValue(t, rec); !equalNumbers(got, []float64{2, 3, 4}) {
		t.Errorf("POST plus_one = %v, want [2 3 4]", got)
	}

	rec = rpc(t, s, http.MethodGet, "/rpc/plus_one?v=1&v=2&v=3", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := decodeValue(t, rec); !equalNumbers(got, []float64{2, 3, 4}) {
		t.Errorf("GET plus_one = %v, want [2 3 4]", got)
	}
}

func TestLiveRPCTableValued(t *testing.T) {
	s := newLiveServer(t)

	// SETOF a table: filters and projection apply.
	rec := rpc(t, s, http.MethodGet, "/rpc/best_films?select=title,year&title=eq.Pulp%20Fiction", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	rows := decodeRows(t, rec)
	if len(rows) != 1 || rows[0]["title"] != "Pulp Fiction" {
		t.Fatalf("rows = %#v", rows)
	}
	if _, present := rows[0]["id"]; present {
		t.Errorf("projection should omit id: %#v", rows[0])
	}

	// RETURNS TABLE(...): the synthesized relation drives the projection.
	rec = rpc(t, s, http.MethodGet, "/rpc/film_titles?select=title&order=year.desc", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if rows := decodeRows(t, rec); len(rows) != 1 {
		t.Fatalf("rows = %#v", rows)
	}
}

func TestLiveRPCTableValuedCountAndEmbed(t *testing.T) {
	s := newLiveServer(t)

	rec := rpc(t, s, http.MethodGet, "/rpc/best_films?limit=1", "", map[string]string{"Prefer": "count=exact"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Range"); got != "0-0/1" {
		t.Errorf("Content-Range = %q, want 0-0/1", got)
	}

	// A table-valued function embeds like a relation.
	rec = rpc(t, s, http.MethodGet, "/rpc/best_films?select=title,directors(last_name)", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	rows := decodeRows(t, rec)
	director, ok := rows[0]["directors"].(map[string]any)
	if !ok || director["last_name"] != "Tarantino" {
		t.Fatalf("embed through RPC = %#v", rows[0])
	}
}

func TestLiveRPCVolatileRequiresPost(t *testing.T) {
	s := newLiveServer(t)

	// GET runs in a read-only transaction, so PostgreSQL refuses the write and
	// the SQLSTATE maps to 405, exactly as it does under PostgREST.
	rec := rpc(t, s, http.MethodGet, "/rpc/bump_notes?n=1", "", nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405, body = %s", rec.Code, rec.Body.String())
	}
	if code := decodeError(t, rec)["code"]; code != "25006" {
		t.Errorf("code = %v, want the read-only transaction SQLSTATE", code)
	}

	rec = rpc(t, s, http.MethodPost, "/rpc/bump_notes", `{"n":1}`, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("void status = %d, want 204, body = %s", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Errorf("a void function must not return a body, got %q", rec.Body.String())
	}
}

func TestLiveRPCAllow(t *testing.T) {
	s := newLiveServer(t)

	// A stable function callable with no arguments answers OPTIONS.
	rec := rpc(t, s, http.MethodOptions, "/rpc/best_films", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("stable OPTIONS status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != "OPTIONS,GET,HEAD,POST" {
		t.Errorf("stable Allow = %q", got)
	}

	// A volatile function offers POST only.
	rec = rpc(t, s, http.MethodOptions, "/rpc/bump_notes", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("volatile OPTIONS status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != "OPTIONS,POST" {
		t.Errorf("volatile Allow = %q, want OPTIONS,POST", got)
	}

	// OPTIONS behaves like a parameterless call, so a function requiring
	// arguments cannot be resolved. PostgREST does the same.
	rec = rpc(t, s, http.MethodOptions, "/rpc/add_them", "", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("OPTIONS on add_them status = %d, want 404", rec.Code)
	}
	rec = rpc(t, s, http.MethodOptions, "/rpc/nope", "", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown function OPTIONS status = %d, want 404", rec.Code)
	}
}

func TestLiveRPCErrors(t *testing.T) {
	s := newLiveServer(t)

	rec := rpc(t, s, http.MethodPost, "/rpc/does_not_exist", `{}`, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", rec.Code, rec.Body.String())
	}
	if code := decodeError(t, rec)["code"]; code != "PGRST202" {
		t.Errorf("code = %v, want PGRST202", code)
	}

	// A missing required argument cannot resolve to an overload.
	rec = rpc(t, s, http.MethodPost, "/rpc/add_them", `{"a":1}`, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", rec.Code, rec.Body.String())
	}
	if code := decodeError(t, rec)["code"]; code != "PGRST202" {
		t.Errorf("code = %v, want PGRST202", code)
	}

	// max-affected is enforced for RPC as PGRST128.
	rec = rpc(t, s, http.MethodGet, "/rpc/best_films", "", map[string]string{
		"Prefer": "handling=strict, max-affected=0",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	if code := decodeError(t, rec)["code"]; code != "PGRST128" {
		t.Errorf("code = %v, want PGRST128", code)
	}
}

func equalNumbers(value any, want []float64) bool {
	list, ok := value.([]any)
	if !ok || len(list) != len(want) {
		return false
	}
	for i, item := range list {
		if item != want[i] {
			return false
		}
	}
	return true
}
