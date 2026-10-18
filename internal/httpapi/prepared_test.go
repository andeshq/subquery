package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/andeshq/subquery/internal/config"
)

// TestLiveWithoutPreparedStatements asserts the request path works with
// db-prepared-statements disabled, which takes pgx off the prepared-statement
// cache. Both a read and a write must still round-trip.
func TestLiveWithoutPreparedStatements(t *testing.T) {
	s := newLiveServerWith(t, func(c *config.Config) { c.DBPreparedStatements = false })

	if rec := get(t, s, "/todos?limit=1", nil); rec.Code != http.StatusOK {
		t.Fatalf("read status = %d, body = %s", rec.Code, rec.Body.String())
	}

	req := httptest.NewRequest(http.MethodPatch, "/todos?id=eq.1", strings.NewReader(`{"done":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Prefer", "return=minimal")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("write status = %d, body = %s", rec.Code, rec.Body.String())
	}
}
