package httpapi

import (
	"net/http"
	"testing"
	"time"

	"github.com/andeshq/subquery/internal/config"
)

// TestLiveRequestTimeout asserts db-request-timeout bounds the request: a
// deadline that cannot be met fails the request instead of hanging.
func TestLiveRequestTimeout(t *testing.T) {
	s := newLiveServerWith(t, func(c *config.Config) { c.DBRequestTimeout = time.Nanosecond })

	rec := get(t, s, "/todos", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500, body = %s", rec.Code, rec.Body.String())
	}
}

// TestLiveNoRequestTimeout asserts a zero timeout leaves the request unbounded.
func TestLiveNoRequestTimeout(t *testing.T) {
	s := newLiveServerWith(t, func(c *config.Config) { c.DBRequestTimeout = 0 })

	if rec := get(t, s, "/todos", nil); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
}
