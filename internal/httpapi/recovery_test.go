package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/andeshq/subquery/internal/config"
)

// TestRecoveryReturnsAPIError asserts a panicking handler yields the API's
// error shape rather than a dropped connection, without leaking the panic.
func TestRecoveryReturnsAPIError(t *testing.T) {
	s := newTestServer(t, func(c *config.Config) { c.DBAnonRole = "anon" })
	panicking := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") })
	handler := s.withLogging(s.withRecovery(panicking))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/users", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if code := decodeError(t, rec)["code"]; code != "PGRSTX00" {
		t.Errorf("code = %v, want PGRSTX00", code)
	}
	if strings.Contains(rec.Body.String(), "boom") {
		t.Errorf("body leaked the panic value: %s", rec.Body.String())
	}
}

// TestRecoveryAfterResponseStarted asserts an already-started response is left
// alone; only the log records the panic.
func TestRecoveryAfterResponseStarted(t *testing.T) {
	s := newTestServer(t, func(c *config.Config) { c.DBAnonRole = "anon" })
	panicking := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		panic("late")
	})
	handler := s.withLogging(s.withRecovery(panicking))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/users", nil))

	if rec.Code != http.StatusCreated {
		t.Errorf("status = %d, want the handler's 201", rec.Code)
	}
}
