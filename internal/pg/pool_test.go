package pg

import (
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/andeshq/subquery/internal/config"
	"github.com/andeshq/subquery/test/dbtest"
)

// TestMain provisions the package's own fixture database.
func TestMain(m *testing.M) { os.Exit(dbtest.Main(m)) }

// TestBeginImpersonatesRole exercises the transaction setup against a real
// database.
func TestBeginImpersonatesRole(t *testing.T) {
	cfg := testConfig(dbtest.DSN(t))

	pool, err := NewPool(t.Context(), cfg)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()

	tx, err := Begin(t.Context(), pool, RequestSettings{
		Role:       "anon",
		SearchPath: []string{"public"},
		GUCs: map[string]string{
			"request.jwt.claims": `{"role":"anon"}`,
			"app.settings.env":   "test",
		},
		ReadOnly: true,
	})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = tx.Rollback(t.Context()) }()

	var currentRole, sessionRole, claims, setting string
	if err := tx.QueryRow(t.Context(), "select current_user, session_user").Scan(&currentRole, &sessionRole); err != nil {
		t.Fatalf("select roles: %v", err)
	}
	if currentRole != "anon" {
		t.Errorf("current_user = %q, want anon", currentRole)
	}
	if sessionRole == "anon" {
		t.Errorf("session_user = %q, want the authenticator, not the impersonated role", sessionRole)
	}
	if err := tx.QueryRow(t.Context(), "select current_setting('request.jwt.claims')").Scan(&claims); err != nil {
		t.Fatalf("read request.jwt.claims: %v", err)
	}
	if claims != `{"role":"anon"}` {
		t.Errorf("request.jwt.claims = %q", claims)
	}
	if err := tx.QueryRow(t.Context(), "select current_setting('app.settings.env')").Scan(&setting); err != nil {
		t.Fatalf("read app.settings.env: %v", err)
	}
	if setting != "test" {
		t.Errorf("app.settings.env = %q, want test", setting)
	}

	var count int
	if err := tx.QueryRow(t.Context(), "select count(*) from todos").Scan(&count); err != nil {
		t.Fatalf("count todos: %v", err)
	}

	// The transaction is read-only, so a write must be rejected by PostgreSQL.
	_, err = tx.Exec(t.Context(), "insert into todos (task) values ('nope')")
	if err == nil {
		t.Fatal("expected a read-only transaction to reject an insert")
	}
	pgErr, ok := errors.AsType[*pgconn.PgError](err)
	if !ok {
		t.Fatalf("error type = %T, want *pgconn.PgError", err)
	}
	if pgErr.Code != "25006" {
		t.Errorf("sqlstate = %s, want 25006 (read_only_sql_transaction)", pgErr.Code)
	}
}

// testDatabaseURI returns the package's fixture connection string.
func testDatabaseURI(t *testing.T) string {
	t.Helper()
	return dbtest.DSN(t)
}

// testConfig is a pool configuration for unit tests.
func testConfig(uri string) *config.Config {
	cfg := config.Default()
	cfg.DBURI = uri
	cfg.DBPool = 2
	return cfg
}
