package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/andeshq/subquery/internal/config"
)

func write(t *testing.T, s *Server, method, target, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, reader)
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

func firstRow(t *testing.T, s *Server, target string) map[string]any {
	t.Helper()
	rec := get(t, s, target, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: status = %d, body = %s", target, rec.Code, rec.Body.String())
	}
	rows := decodeRows(t, rec)
	if len(rows) == 0 {
		t.Fatalf("GET %s returned no rows", target)
	}
	return rows[0]
}

func TestLiveWriteInsertAndReturn(t *testing.T) {
	s := newLiveServer(t)

	// Minimal insert: 201 with no body.
	rec := write(t, s, http.MethodPost, "/notes", `{"title":"minimal"}`, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body = %s", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Errorf("minimal insert should have no body, got %q", rec.Body.String())
	}

	// Representation insert returns the created row.
	rec = write(t, s, http.MethodPost, "/notes", `{"title":"shown","body":"b"}`, map[string]string{
		"Prefer": "return=representation",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	rows := decodeRows(t, rec)
	if len(rows) != 1 || rows[0]["title"] != "shown" {
		t.Fatalf("rows = %#v", rows)
	}
	if rows[0]["score"] != float64(7) {
		t.Errorf("score = %v, want the column default 7", rows[0]["score"])
	}
	createdID := int64(rows[0]["id"].(float64))

	// Clean up.
	if rec := write(t, s, http.MethodDelete, "/notes?id=eq."+itoa(createdID), "", nil); rec.Code != http.StatusNoContent {
		t.Fatalf("cleanup status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if rec := write(t, s, http.MethodDelete, "/notes?title=eq.minimal", "", nil); rec.Code != http.StatusNoContent {
		t.Fatalf("cleanup status = %d", rec.Code)
	}
}

func TestLiveWriteMissingDefault(t *testing.T) {
	s := newLiveServer(t)

	rec := write(t, s, http.MethodPost, "/notes", `{"title":"defaulted"}`, map[string]string{
		"Prefer": "missing=default, return=representation",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	rows := decodeRows(t, rec)
	if rows[0]["score"] != float64(7) {
		t.Errorf("score = %v, want the column default 7", rows[0]["score"])
	}
	write(t, s, http.MethodDelete, "/notes?title=eq.defaulted", "", nil)
}

func TestLiveWriteBulkInsert(t *testing.T) {
	s := newLiveServer(t)
	rec := write(t, s, http.MethodPost, "/notes",
		`[{"title":"bulk-1"},{"title":"bulk-2"}]`, map[string]string{"Prefer": "return=representation"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if rows := decodeRows(t, rec); len(rows) != 2 {
		t.Fatalf("rows = %#v", rows)
	}
	write(t, s, http.MethodDelete, "/notes?title=like.bulk-*", "", nil)
}

func TestLiveWriteHeadersOnlyLocation(t *testing.T) {
	s := newLiveServer(t)
	rec := write(t, s, http.MethodPost, "/notes", `{"title":"located"}`, map[string]string{
		"Prefer": "return=headers-only",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	location := rec.Header().Get("Location")
	if !strings.HasPrefix(location, "/notes?id=eq.") {
		t.Fatalf("Location = %q", location)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("headers-only must not write a body, got %q", rec.Body.String())
	}
	// The Location must resolve.
	rec = get(t, s, location, nil)
	rows := decodeRows(t, rec)
	if len(rows) != 1 || rows[0]["title"] != "located" {
		t.Fatalf("Location did not resolve: %#v", rows)
	}
	write(t, s, http.MethodDelete, "/notes?title=eq.located", "", nil)
}

func TestLiveWriteUpdateAndDelete(t *testing.T) {
	s := newLiveServer(t)
	// Clean up rows left by earlier runs.
	write(t, s, http.MethodDelete, "/notes?title=eq.patched", "", nil)
	write(t, s, http.MethodPost, "/notes", `{"title":"patched","score":1}`, nil)

	rec := write(t, s, http.MethodPatch, "/notes?title=eq.patched", `{"score":42,"body":"updated"}`, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("PATCH status = %d, want 204, body = %s", rec.Code, rec.Body.String())
	}
	row := firstRow(t, s, "/notes?select=score,body&title=eq.patched")
	if row["score"] != float64(42) || row["body"] != "updated" {
		t.Fatalf("row = %#v", row)
	}

	rec = write(t, s, http.MethodPatch, "/notes?title=eq.patched", `{"score":9}`, map[string]string{
		"Prefer": "return=representation",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH representation status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if rows := decodeRows(t, rec); len(rows) != 1 || rows[0]["score"] != float64(9) {
		t.Fatalf("rows = %#v", rows)
	}

	rec = write(t, s, http.MethodDelete, "/notes?title=eq.patched", "", map[string]string{
		"Prefer": "return=representation",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE representation status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if rows := decodeRows(t, rec); len(rows) != 1 {
		t.Fatalf("rows = %#v", rows)
	}
}

func TestLiveWriteUpsert(t *testing.T) {
	s := newLiveServer(t)
	t.Cleanup(func() { write(t, s, http.MethodDelete, "/products?sku=eq.SKU-1", "", nil) })

	rec := write(t, s, http.MethodPost, "/products", `{"sku":"SKU-1","name":"first","price":1}`, map[string]string{
		"Prefer": "resolution=merge-duplicates, return=representation",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("insert status = %d, body = %s", rec.Code, rec.Body.String())
	}

	// A merge that updates an existing row reports 200, not 201.
	rec = write(t, s, http.MethodPost, "/products", `{"sku":"SKU-1","name":"second","price":2}`, map[string]string{
		"Prefer": "resolution=merge-duplicates, return=representation",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("merge status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
	rows := decodeRows(t, rec)
	if rows[0]["name"] != "second" || rows[0]["price"] != float64(2) {
		t.Fatalf("merged row = %#v", rows[0])
	}

	// ignore-duplicates leaves the existing row alone.
	rec = write(t, s, http.MethodPost, "/products", `{"sku":"SKU-1","name":"ignored","price":9}`, map[string]string{
		"Prefer": "resolution=ignore-duplicates, return=representation",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("ignore status = %d, body = %s", rec.Code, rec.Body.String())
	}
	row := firstRow(t, s, "/products?sku=eq.SKU-1")
	if row["name"] != "second" {
		t.Errorf("ignore-duplicates changed the row: %#v", row)
	}
}

func TestLiveWritePut(t *testing.T) {
	s := newLiveServer(t)
	t.Cleanup(func() { write(t, s, http.MethodDelete, "/products?sku=eq.SKU-PUT", "", nil) })

	// A minimal PUT reports 204, as PostgREST does.
	rec := write(t, s, http.MethodPut, "/products?sku=eq.SKU-PUT", `{"sku":"SKU-PUT","name":"created","price":5}`, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("minimal insert status = %d, want 204, body = %s", rec.Code, rec.Body.String())
	}
	row := firstRow(t, s, "/products?sku=eq.SKU-PUT")
	if row["name"] != "created" {
		t.Fatalf("row = %#v", row)
	}

	// With a representation, a PUT that inserts reports 201.
	rec = write(t, s, http.MethodPut, "/products?sku=eq.SKU-OTHER",
		`{"sku":"SKU-OTHER","name":"created","price":5}`,
		map[string]string{"Prefer": "return=representation"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("insert status = %d, want 201, body = %s", rec.Code, rec.Body.String())
	}
	t.Cleanup(func() { write(t, s, http.MethodDelete, "/products?sku=eq.SKU-OTHER", "", nil) })

	// ... and a PUT that replaces reports 200.
	rec = write(t, s, http.MethodPut, "/products?sku=eq.SKU-OTHER",
		`{"sku":"SKU-OTHER","name":"replaced","price":6}`,
		map[string]string{"Prefer": "return=representation"})
	if rec.Code != http.StatusOK {
		t.Fatalf("replace status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
	rows := decodeRows(t, rec)
	if len(rows) != 1 || rows[0]["name"] != "replaced" || rows[0]["price"] != float64(6) {
		t.Fatalf("rows = %#v", rows)
	}
}

func TestLiveWriteEmbedOnInsert(t *testing.T) {
	s := newLiveServer(t)
	rec := write(t, s, http.MethodPost, "/films?select=title,directors(last_name)", `{"director_id":1,"title":"Embedded"}`, map[string]string{
		"Prefer": "return=representation",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	rows := decodeRows(t, rec)
	if len(rows) != 1 || rows[0]["title"] != "Embedded" {
		t.Fatalf("rows = %#v", rows)
	}
	director, ok := rows[0]["directors"].(map[string]any)
	if !ok || director["last_name"] != "Tarantino" {
		t.Fatalf("embed on write = %#v", rows[0]["directors"])
	}
	write(t, s, http.MethodDelete, "/films?title=eq.Embedded", "", nil)
}

func TestLiveWriteRollbackPreference(t *testing.T) {
	// With db-tx-end = commit (the default) the tx preference is ignored, so
	// the row is persisted.
	s := newLiveServer(t)
	rec := write(t, s, http.MethodPost, "/notes", `{"title":"rolled-back"}`, map[string]string{
		"Prefer": "tx=rollback, return=representation",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := get(t, s, "/notes?title=eq.rolled-back", nil); len(decodeRows(t, got)) != 1 {
		t.Fatal("tx=rollback should be ignored unless db-tx-end allows an override")
	}
	write(t, s, http.MethodDelete, "/notes?title=eq.rolled-back", "", nil)

	// With db-tx-end = commit-allow-override it is honored.
	overriding := newLiveServerWith(t, func(c *config.Config) {
		c.DBTxEnd = config.TxCommitAllowOverride
	})
	rec = write(t, overriding, http.MethodPost, "/notes", `{"title":"conf-rollback"}`, map[string]string{
		"Prefer": "tx=rollback, return=representation",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if rows := decodeRows(t, rec); len(rows) != 1 {
		t.Fatalf("rows = %#v", rows)
	}
	got := get(t, overriding, "/notes?title=eq.conf-rollback", nil)
	if rows := decodeRows(t, got); len(rows) != 0 {
		t.Fatalf("tx=rollback should have rolled back, got %#v", rows)
	}
}

func TestLiveWriteErrors(t *testing.T) {
	s := newLiveServer(t)
	cases := []struct {
		name    string
		method  string
		target  string
		body    string
		headers map[string]string
		status  int
		code    string
	}{
		{"empty body", http.MethodPost, "/notes", "", nil, http.StatusBadRequest, "PGRST102"},
		{"malformed", http.MethodPost, "/notes", `{"title":`, nil, http.StatusBadRequest, "PGRST102"},
		{"unknown column", http.MethodPost, "/notes", `{"nope":"x"}`, nil, http.StatusBadRequest, "PGRST204"},
		{"filter on POST", http.MethodPost, "/notes?title=eq.x", `{"title":"x"}`, nil, http.StatusBadRequest, "PGRST100"},
		{"put without key filter", http.MethodPut, "/products", `{"sku":"A","name":"n"}`, nil, http.StatusBadRequest, "PGRST105"},
		{"put key mismatch", http.MethodPut, "/products?sku=eq.A", `{"sku":"B","name":"n"}`, nil, http.StatusBadRequest, "PGRST115"},
		{"strict bad preference", http.MethodPost, "/notes", `{"title":"x"}`, map[string]string{"Prefer": "handling=strict, bogus"}, http.StatusBadRequest, "PGRST122"},
		{"unsupported content type", http.MethodPost, "/notes", `title=x`, map[string]string{"Content-Type": "text/csv"}, http.StatusUnsupportedMediaType, "PGRST107"},
		{"array on PATCH", http.MethodPatch, "/notes?title=eq.x", `[{"title":"a"}]`, nil, http.StatusBadRequest, "PGRST102"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := write(t, s, tc.method, tc.target, tc.body, tc.headers)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.status, rec.Body.String())
			}
			if code := decodeError(t, rec)["code"]; code != tc.code {
				t.Fatalf("code = %v, want %s", code, tc.code)
			}
		})
	}
}

func TestLiveWriteMaxAffected(t *testing.T) {
	s := newLiveServer(t)
	write(t, s, http.MethodDelete, "/notes?title=like.max-*", "", nil)
	write(t, s, http.MethodPost, "/notes", `[{"title":"max-1"},{"title":"max-2"},{"title":"max-3"}]`, nil)
	t.Cleanup(func() { write(t, s, http.MethodDelete, "/notes?title=like.max-*", "", nil) })

	rec := write(t, s, http.MethodDelete, "/notes?title=like.max-*", "", map[string]string{
		"Prefer": "handling=strict, max-affected=1",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	if code := decodeError(t, rec)["code"]; code != "PGRST124" {
		t.Fatalf("code = %v, want PGRST124", code)
	}
	// The rejected delete must not have applied.
	rec = get(t, s, "/notes?title=like.max-*&select=title", nil)
	if rows := decodeRows(t, rec); len(rows) != 3 {
		t.Fatalf("rows = %#v, want the delete rolled back", rows)
	}
}

func itoa(n int64) string {
	return strconv.FormatInt(n, 10)
}
