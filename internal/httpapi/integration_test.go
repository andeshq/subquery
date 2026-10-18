package httpapi

import (
	"encoding/json/v2"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/andeshq/subquery/internal/auth"
	"github.com/andeshq/subquery/internal/config"
	"github.com/andeshq/subquery/internal/pg"
	"github.com/andeshq/subquery/internal/schema"
	"github.com/andeshq/subquery/test/dbtest"
)

// TestMain provisions the package's own fixture database.
func TestMain(m *testing.M) { os.Exit(dbtest.Main(m)) }

// newLiveServer builds a server backed by the package's fixture database.
func newLiveServer(t testing.TB) *Server { return newLiveServerWith(t, nil) }

func newLiveServerWith(t testing.TB, mutate func(*config.Config)) *Server {
	return newLiveServerLogging(t, mutate, slog.New(slog.DiscardHandler))
}

// newLiveServerLogging builds a live server whose access log is written to log.
func newLiveServerLogging(t testing.TB, mutate func(*config.Config), log *slog.Logger) *Server {
	t.Helper()
	cfg := config.Default()
	cfg.DBURI = dbtest.DSN(t)
	cfg.DBAnonRole = "anon"
	// A secret lets the RLS tests present real tokens.
	cfg.JWTSecret = testJWTSecret
	if mutate != nil {
		mutate(cfg)
	}

	pool, err := pg.NewPool(t.Context(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(pool.Close)

	store := schema.NewStore(pool, log, schema.Options{
		Schemas: cfg.DBSchemas,
		Exposed: cfg.DBSchemas,
	})
	if err := store.Reload(t.Context()); err != nil {
		t.Fatalf("schema Reload: %v", err)
	}

	verifier, err := auth.NewVerifier(cfg)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	return New(Options{
		Config:   cfg,
		Logger:   log,
		Pool:     pool,
		Verifier: verifier,
		Store:    store,
	})
}

// newLiveServerNoSchema builds a live server whose schema cache has never
// loaded, so readiness can be exercised without a loaded snapshot.
func newLiveServerNoSchema(t testing.TB) *Server {
	t.Helper()
	cfg := config.Default()
	cfg.DBURI = dbtest.DSN(t)
	cfg.DBAnonRole = "anon"

	pool, err := pg.NewPool(t.Context(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(pool.Close)

	store := schema.NewStore(pool, slog.New(slog.DiscardHandler), schema.Options{
		Schemas: cfg.DBSchemas,
		Exposed: cfg.DBSchemas,
	})
	verifier, err := auth.NewVerifier(cfg)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	return New(Options{
		Config:   cfg,
		Logger:   slog.New(slog.DiscardHandler),
		Pool:     pool,
		Verifier: verifier,
		Store:    store,
	})
}

func get(t *testing.T, s *Server, target string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func decodeRows(t *testing.T, rec *httptest.ResponseRecorder) []map[string]any {
	t.Helper()
	var rows []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode body %q: %v", rec.Body.String(), err)
	}
	return rows
}

func TestLiveReadProjectionAndOrder(t *testing.T) {
	s := newLiveServer(t)
	rec := get(t, s, "/films?select=title,year&order=year.desc", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Range"); got != "0-0/*" {
		t.Errorf("Content-Range = %q, want 0-0/*", got)
	}
	rows := decodeRows(t, rec)
	if len(rows) != 1 || rows[0]["title"] != "Pulp Fiction" {
		t.Fatalf("rows = %#v", rows)
	}
	if _, present := rows[0]["id"]; present {
		t.Errorf("projection should omit id: %#v", rows[0])
	}
}

func TestLiveReadFilterAndCount(t *testing.T) {
	s := newLiveServer(t)
	rec := get(t, s, "/todos?limit=1", map[string]string{"Prefer": "count=exact"})
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Range"); got != "0-0/2" {
		t.Errorf("Content-Range = %q, want 0-0/2", got)
	}
	if got := rec.Header().Get("Preference-Applied"); got != "count=exact" {
		t.Errorf("Preference-Applied = %q", got)
	}
}

func TestLiveReadFullCountIsOK(t *testing.T) {
	s := newLiveServer(t)
	rec := get(t, s, "/todos", map[string]string{"Prefer": "count=exact"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Range"); got != "0-1/2" {
		t.Errorf("Content-Range = %q, want 0-1/2", got)
	}
}

func TestLiveReadPartialContent(t *testing.T) {
	s := newLiveServer(t)
	rec := get(t, s, "/todos?limit=1", map[string]string{
		"Prefer": "count=exact",
		"Range":  "0-0",
	})
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Range"); got != "0-0/2" {
		t.Errorf("Content-Range = %q", got)
	}
}

func TestLiveReadFilters(t *testing.T) {
	s := newLiveServer(t)
	cases := []struct {
		target string
		rows   int
	}{
		{"/films?title=like.*Pulp*", 1},
		{"/films?title=like.*Nope*", 0},
		{"/films?year=gte.2000", 0},
		{"/films?year=gte.1900&title=eq.Pulp%20Fiction", 1},
		{"/films?id=in.(1,999)", 1},
		{"/films?or=(year.lt.1900,year.gt.1990)", 1},
		{"/films?year=not.eq.2000", 1},
		{"/films?select=title&title=not.is.null", 1},
		{"/films?tags=cs.{crime}", 1},
		{"/films?tags=cs.{horror}", 0},
		{"/films?meta->>genre=eq.crime", 1},
		{"/films?meta->>genre=eq.horror", 0},
		{"/films?select=meta->>genre&meta->>genre=eq.crime", 1},
	}
	for _, tc := range cases {
		rec := get(t, s, tc.target, nil)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status = %d, body = %s", tc.target, rec.Code, rec.Body.String())
			continue
		}
		if rows := decodeRows(t, rec); len(rows) != tc.rows {
			t.Errorf("%s: %d rows, want %d", tc.target, len(rows), tc.rows)
		}
	}
}

func TestLiveReadPaginationWindow(t *testing.T) {
	s := newLiveServer(t)
	rec := get(t, s, "/todos?order=id.asc&limit=1&offset=1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Range"); got != "1-1/*" {
		t.Errorf("Content-Range = %q, want 1-1/*", got)
	}
}

func TestLiveReadErrors(t *testing.T) {
	s := newLiveServer(t)
	cases := []struct {
		target string
		status int
		code   string
	}{
		// An unknown filter column is passed through, so PostgreSQL names it,
		// matching PostgREST.
		{"/films?bogus=eq.1", http.StatusBadRequest, "42703"},
		{"/films?select=bogus", http.StatusBadRequest, "PGRST100"},
		{"/films?year=bogus.1", http.StatusBadRequest, "PGRST100"},
		{"/films?select=title,unicorns(name)", http.StatusBadRequest, "PGRST200"},
		{"/films?select=title&unicorns.name=eq.x", http.StatusBadRequest, "PGRST108"},
		{"/nope", http.StatusNotFound, "PGRST205"},
	}
	for _, tc := range cases {
		rec := get(t, s, tc.target, nil)
		if rec.Code != tc.status {
			t.Errorf("%s: status = %d, want %d", tc.target, rec.Code, tc.status)
			continue
		}
		if code := decodeError(t, rec)["code"]; code != tc.code {
			t.Errorf("%s: code = %v, want %s", tc.target, code, tc.code)
		}
	}
}

func TestLiveReadRejectsUnsupportedMediaType(t *testing.T) {
	s := newLiveServer(t)
	rec := get(t, s, "/films", map[string]string{"Accept": "text/plain"})
	if rec.Code != http.StatusNotAcceptable {
		t.Fatalf("status = %d, want 406", rec.Code)
	}
	if code := decodeError(t, rec)["code"]; code != "PGRST107" {
		t.Errorf("code = %v, want PGRST107", code)
	}
}

func TestLiveReadRange(t *testing.T) {
	s := newLiveServer(t)

	// A range that cannot be parsed is ignored, as PostgREST ignores it.
	rec := get(t, s, "/todos", map[string]string{"Range": "not-a-range"})
	if rec.Code != http.StatusOK {
		t.Fatalf("malformed range status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Range"); got != "0-1/*" {
		t.Errorf("Content-Range = %q, want the full window", got)
	}

	// A unit other than items is rejected.
	rec = get(t, s, "/todos", map[string]string{"Range": "0-0", "Range-Unit": "bytes"})
	if rec.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("status = %d, want 416", rec.Code)
	}
	if code := decodeError(t, rec)["code"]; code != "PGRST103" {
		t.Errorf("code = %v, want PGRST103", code)
	}
}

func TestLiveReadJSONProjectionAlias(t *testing.T) {
	s := newLiveServer(t)
	rec := get(t, s, "/films?select=jsonYear:year,title", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	rows := decodeRows(t, rec)
	if len(rows) != 1 {
		t.Fatalf("rows = %#v", rows)
	}
	if _, ok := rows[0]["jsonYear"]; !ok {
		t.Errorf("alias missing: %#v (first row keys %s)", rows[0], strings.Join(keys(rows[0]), ","))
	}
}

func TestLiveReadEmbeddings(t *testing.T) {
	s := newLiveServer(t)

	// Many-to-one: films -> directors, a single object.
	rec := get(t, s, "/films?select=title,directors(last_name)", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("to-one: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	rows := decodeRows(t, rec)
	director, ok := rows[0]["directors"].(map[string]any)
	if !ok || director["last_name"] != "Tarantino" {
		t.Fatalf("to-one embed = %#v", rows[0]["directors"])
	}

	// One-to-many: films -> roles, an array.
	rec = get(t, s, "/films?select=title,roles(character)", nil)
	rows = decodeRows(t, rec)
	roles, ok := rows[0]["roles"].([]any)
	if !ok || len(roles) != 1 {
		t.Fatalf("to-many embed = %#v", rows[0]["roles"])
	}

	// Many-to-many through the roles junction: films -> actors.
	rec = get(t, s, "/films?select=title,actors(first_name,last_name)", nil)
	rows = decodeRows(t, rec)
	actors, ok := rows[0]["actors"].([]any)
	if !ok || len(actors) != 1 {
		t.Fatalf("m2m embed = %#v", rows[0]["actors"])
	}
	first := actors[0].(map[string]any)
	if first["first_name"] != "John" {
		t.Errorf("m2m actor = %#v", first)
	}

	// One-to-one: technical_specs shares films' key.
	rec = get(t, s, "/films?select=title,technical_specs(camera)", nil)
	rows = decodeRows(t, rec)
	specs, ok := rows[0]["technical_specs"].(map[string]any)
	if !ok || specs["camera"] != "Arriflex" {
		t.Fatalf("one-to-one embed = %#v", rows[0]["technical_specs"])
	}

	// Alias.
	rec = get(t, s, "/films?select=title,director:directors(last_name)", nil)
	rows = decodeRows(t, rec)
	if _, ok := rows[0]["director"].(map[string]any); !ok {
		t.Fatalf("aliased embed = %#v", rows[0])
	}
}

func TestLiveReadEmbedInnerJoin(t *testing.T) {
	s := newLiveServer(t)
	// Only films with an actor named John survive !inner.
	rec := get(t, s, "/films?select=title,actors!inner(first_name)&actors.first_name=eq.John", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if rows := decodeRows(t, rec); len(rows) != 1 {
		t.Fatalf("inner join returned %d rows, want 1", len(rows))
	}
	rec = get(t, s, "/films?select=title,actors!inner(first_name)&actors.first_name=eq.Nobody", nil)
	if rows := decodeRows(t, rec); len(rows) != 0 {
		t.Fatalf("inner join returned %d rows, want 0", len(rows))
	}
}

func TestLiveReadEmbedAntiJoin(t *testing.T) {
	s := newLiveServer(t)
	rec := get(t, s, "/films?select=title,technical_specs(*)&technical_specs=is.null", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if rows := decodeRows(t, rec); len(rows) != 0 {
		t.Fatalf("anti-join returned %d rows, want 0", len(rows))
	}
}

func TestLiveReadNestedEmbed(t *testing.T) {
	s := newLiveServer(t)
	rec := get(t, s, "/films?select=title,roles(character,actors(first_name))", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	rows := decodeRows(t, rec)
	roles, ok := rows[0]["roles"].([]any)
	if !ok || len(roles) != 1 {
		t.Fatalf("roles = %#v", rows[0]["roles"])
	}
	role := roles[0].(map[string]any)
	// roles -> actors is many-to-one, so the nested embed is an object.
	actor, ok := role["actors"].(map[string]any)
	if !ok {
		t.Fatalf("nested actors = %#v, want a single object", role["actors"])
	}
	if actor["first_name"] != "John" {
		t.Errorf("nested actor = %#v", actor)
	}
}

func TestLiveReadEmbeddedOrderAndLimit(t *testing.T) {
	s := newLiveServer(t)
	rec := get(t, s, "/films?select=title,roles(character)&roles.order=character.desc&roles.limit=1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	rows := decodeRows(t, rec)
	roles := rows[0]["roles"].([]any)
	if len(roles) != 1 {
		t.Fatalf("roles = %#v", roles)
	}
}

func TestLiveSpreadEmbeds(t *testing.T) {
	s := newLiveServer(t)

	// To-one spread lifts columns to the parent object.
	rec := get(t, s, "/films?select=title,...directors(last_name)", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("to-one spread: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	rows := decodeRows(t, rec)
	if rows[0]["last_name"] != "Tarantino" {
		t.Errorf("to-one spread = %#v", rows[0])
	}
	if _, wrapped := rows[0]["directors"]; wrapped {
		t.Errorf("spread must not wrap in an object: %#v", rows[0])
	}

	// To-many spread turns columns into correlated arrays.
	rec = get(t, s, "/films?select=title,...roles(character)", nil)
	rows = decodeRows(t, rec)
	characters, ok := rows[0]["character"].([]any)
	if !ok || len(characters) != 1 || characters[0] != "Vincent Vega" {
		t.Fatalf("to-many spread = %#v", rows[0])
	}
}

func TestLiveAllowFromPrivileges(t *testing.T) {
	s := newLiveServer(t)

	rec := do(t, s, http.MethodOptions, "/films", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != "OPTIONS,GET,HEAD,POST,PUT,PATCH,DELETE" {
		t.Errorf("Allow = %q", got)
	}

	// The viewer role holds SELECT on films and nothing else.
	snap := s.store.Snapshot()
	films, ok := snap.Relation("films")
	if !ok {
		t.Fatal("films is missing from the snapshot")
	}
	allow, err := s.relationAllow(t.Context(), films, "viewer")
	if err != nil {
		t.Fatalf("relationAllow: %v", err)
	}
	if allow != "OPTIONS,GET,HEAD" {
		t.Errorf("viewer Allow = %q, want OPTIONS,GET,HEAD", allow)
	}

	// Unsupported verbs report the same Allow.
	rec = do(t, s, "BREW", "/films", nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got == "" {
		t.Error("405 responses should carry Allow")
	}
}

func TestLiveReadAggregates(t *testing.T) {
	s := newLiveServerWith(t, func(c *config.Config) { c.DBAggregatesEnabled = true })

	// Aggregates only: a single row.
	rec := get(t, s, "/todos?select=count()", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	rows := decodeRows(t, rec)
	if len(rows) != 1 || rows[0]["count"] != float64(2) {
		t.Fatalf("rows = %#v", rows)
	}

	// Mixed projection implies GROUP BY the plain columns.
	rec = get(t, s, "/todos?select=done,count()", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	rows = decodeRows(t, rec)
	if len(rows) != 2 {
		t.Fatalf("grouped rows = %#v", rows)
	}

	rec = get(t, s, "/films?select=count(),max(year)", nil)
	rows = decodeRows(t, rec)
	if len(rows) != 1 || rows[0]["max"] != float64(1994) {
		t.Fatalf("films aggregates = %#v", rows)
	}
}

func TestLiveReadAggregatesDisabled(t *testing.T) {
	s := newLiveServer(t)
	rec := get(t, s, "/todos?select=count()", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if code := decodeError(t, rec)["code"]; code != "PGRST123" {
		t.Errorf("code = %v, want PGRST123", code)
	}
}

func TestLiveReadComputedField(t *testing.T) {
	s := newLiveServer(t)

	rec := get(t, s, "/films?select=title,film_label&title=eq.Pulp%20Fiction", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	rows := decodeRows(t, rec)
	if len(rows) != 1 || rows[0]["film_label"] != "Pulp Fiction (1994)" {
		t.Fatalf("computed field = %#v", rows)
	}

	// A computed field can be filtered and ordered like a column.
	rec = get(t, s, "/films?select=title&film_label=like.*1994*", nil)
	if rows := decodeRows(t, rec); len(rows) != 1 {
		t.Fatalf("filter on a computed field = %#v", rows)
	}
	rec = get(t, s, "/films?select=title&order=film_label.desc", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("order by a computed field: status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

// TestLiveReadFullTextSearchOnText verifies fts against a non-tsvector column,
// which PostgreSQL converts with to_tsvector.
func TestLiveReadFullTextSearchOnText(t *testing.T) {
	s := newLiveServer(t)

	rec := get(t, s, "/films?select=title&title=fts(english).fiction", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if rows := decodeRows(t, rec); len(rows) != 1 {
		t.Fatalf("fts rows = %#v", rows)
	}
	rec = get(t, s, "/films?select=title&title=fts(english).unicorn", nil)
	if rows := decodeRows(t, rec); len(rows) != 0 {
		t.Fatalf("fts should not match: %#v", rows)
	}
	// plainto_tsquery takes a phrase rather than a query expression.
	rec = get(t, s, "/films?select=title&title=plfts.pulp%20fiction", nil)
	if rows := decodeRows(t, rec); len(rows) != 1 {
		t.Fatalf("plfts rows = %#v", rows)
	}
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
