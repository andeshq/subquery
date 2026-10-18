package pg

import (
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/andeshq/subquery/internal/config"
)

// TestPoolConfigHonorsPreparedStatements proves db-prepared-statements reaches
// pgx: the default prepares and caches statements, and disabling it avoids
// named prepared statements.
func TestPoolConfigHonorsPreparedStatements(t *testing.T) {
	cfg := config.Default()
	cfg.DBURI = "postgres://user:pass@example.com:5432/app"

	pc, err := poolConfig(cfg)
	if err != nil {
		t.Fatalf("poolConfig: %v", err)
	}
	if pc.ConnConfig.DefaultQueryExecMode != pgx.QueryExecModeCacheStatement {
		t.Errorf("prepared statements on: exec mode = %v, want CacheStatement", pc.ConnConfig.DefaultQueryExecMode)
	}
	if pc.MaxConns != int32(cfg.DBPool) {
		t.Errorf("MaxConns = %d, want %d", pc.MaxConns, cfg.DBPool)
	}

	cfg.DBPreparedStatements = false
	pc, err = poolConfig(cfg)
	if err != nil {
		t.Fatalf("poolConfig: %v", err)
	}
	if pc.ConnConfig.DefaultQueryExecMode != pgx.QueryExecModeExec {
		t.Errorf("prepared statements off: exec mode = %v, want Exec", pc.ConnConfig.DefaultQueryExecMode)
	}
}

// TestPoolConfigRejectsBadURI asserts a malformed db-uri is reported instead of
// panicking.
func TestPoolConfigRejectsBadURI(t *testing.T) {
	cfg := config.Default()
	cfg.DBURI = "://not a uri"
	if _, err := poolConfig(cfg); err == nil {
		t.Fatal("poolConfig accepted a malformed db-uri")
	}
}
