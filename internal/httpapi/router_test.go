package httpapi

import (
	"encoding/json/v2"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/andeshq/subquery/internal/auth"
	"github.com/andeshq/subquery/internal/config"
	"github.com/andeshq/subquery/internal/schema"
)

func newTestServer(t *testing.T, mutate func(*config.Config)) *Server {
	t.Helper()
	cfg := config.Default()
	if mutate != nil {
		mutate(cfg)
	}
	v, err := auth.NewVerifier(cfg)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	store := schema.NewStore(nil, slog.New(slog.DiscardHandler), schema.Options{
		Schemas: cfg.DBSchemas,
		Exposed: cfg.DBSchemas,
	})
	return New(Options{
		Config:   cfg,
		Logger:   slog.New(slog.DiscardHandler),
		Verifier: v,
		Store:    store,
	})
}

func do(t *testing.T, s *Server, method, target string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	for k, val := range headers {
		req.Header.Set(k, val)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func decodeError(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body %q: %v", rec.Body.String(), err)
	}
	return body
}

func TestHealth(t *testing.T) {
	s := newTestServer(t, nil)
	rec := do(t, s, http.MethodGet, "/_health", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestAnonymousDisabled(t *testing.T) {
	s := newTestServer(t, func(c *config.Config) { c.DBAnonRole = "" })
	rec := do(t, s, http.MethodGet, "/users", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if got := rec.Header().Get("WWW-Authenticate"); got != "Bearer" {
		t.Errorf("WWW-Authenticate = %q, want Bearer", got)
	}
	if code := decodeError(t, rec)["code"]; code != "PGRST302" {
		t.Errorf("code = %v, want PGRST302", code)
	}
}

func TestInvalidTokenUnauthorized(t *testing.T) {
	s := newTestServer(t, func(c *config.Config) {
		c.DBAnonRole = "anon"
		c.JWTSecret = "reallyreallyreallyreallyverysafe!"
	})
	rec := do(t, s, http.MethodGet, "/users", map[string]string{"Authorization": "Bearer not.a.jwt"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if code := decodeError(t, rec)["code"]; code != "PGRST301" {
		t.Errorf("code = %v, want PGRST301", code)
	}
	if got := rec.Header().Get("WWW-Authenticate"); got != `Bearer error="invalid_token"` {
		t.Errorf("WWW-Authenticate = %q", got)
	}
}

func TestRelationNotFound(t *testing.T) {
	s := newTestServer(t, func(c *config.Config) { c.DBAnonRole = "anon" })
	rec := do(t, s, http.MethodGet, "/users", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if code := decodeError(t, rec)["code"]; code != "PGRST205" {
		t.Errorf("code = %v, want PGRST205", code)
	}
}

func TestMinimalErrorVerbosity(t *testing.T) {
	s := newTestServer(t, func(c *config.Config) {
		c.DBAnonRole = "anon"
		c.ClientErrorVerbosity = config.ErrorMinimal
	})
	rec := do(t, s, http.MethodGet, "/users", nil)
	body := decodeError(t, rec)
	if _, present := body["details"]; present {
		t.Error("minimal verbosity must omit details")
	}
	if _, present := body["hint"]; present {
		t.Error("minimal verbosity must omit hint")
	}
}

func TestUnknownPath(t *testing.T) {
	s := newTestServer(t, func(c *config.Config) { c.DBAnonRole = "anon" })
	rec := do(t, s, http.MethodGet, "/a/b/c", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if code := decodeError(t, rec)["code"]; code != "PGRST125" {
		t.Errorf("code = %v, want PGRST125", code)
	}
}

func TestBearerTokenParsing(t *testing.T) {
	tests := []struct {
		header string
		want   string
		ok     bool
	}{
		{"Basic abc", "", false},
		{"", "", false},
		{"Bearer", "", false},
		{"bearer xyz", "xyz", true},
		{"Bearer  spaced  ", "spaced", true},
	}
	for _, tc := range tests {
		got, ok := bearerToken(tc.header)
		if ok != tc.ok || got != tc.want {
			t.Errorf("bearerToken(%q) = (%q, %v), want (%q, %v)", tc.header, got, ok, tc.want, tc.ok)
		}
	}
}

func TestOpenAPIDisabled(t *testing.T) {
	s := newTestServer(t, func(c *config.Config) { c.OpenAPIMode = config.OpenAPIDisabled })
	rec := do(t, s, http.MethodGet, "/", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if code := decodeError(t, rec)["code"]; code != "PGRST126" {
		t.Errorf("code = %v, want PGRST126", code)
	}
}
