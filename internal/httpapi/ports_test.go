package httpapi

import (
	"log/slog"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/andeshq/subquery/internal/auth"
	"github.com/andeshq/subquery/internal/config"
	"github.com/andeshq/subquery/internal/schema"
)

// The production types satisfy the ports the handlers depend on.
var (
	_ snapshotter   = (*schema.Store)(nil)
	_ tokenVerifier = (*auth.Verifier)(nil)
	_ dbPool        = (*pgxpool.Pool)(nil)
)

// fakeStore is a snapshotter with no database behind it.
type fakeStore struct{ snap *schema.Snapshot }

func (f fakeStore) Snapshot() *schema.Snapshot { return f.snap }

// TestHandlerWithFakeStore proves the handlers depend on the ports rather than
// the concrete schema store: a missing snapshot is reported with no database.
func TestHandlerWithFakeStore(t *testing.T) {
	cfg := config.Default()
	cfg.DBAnonRole = "anon"
	verifier, err := auth.NewVerifier(cfg)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	s := New(Options{
		Config:   cfg,
		Logger:   slog.New(slog.DiscardHandler),
		Verifier: verifier,
		Store:    fakeStore{},
	})

	rec := do(t, s, http.MethodGet, "/users", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if code := decodeError(t, rec)["code"]; code != "PGRST205" {
		t.Errorf("code = %v, want PGRST205", code)
	}
}
