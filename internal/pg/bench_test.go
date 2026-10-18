package pg

import (
	"context"
	"testing"

	"github.com/andeshq/subquery/test/dbtest"
)

// BenchmarkBeginRollback measures the per-request transaction setup: BEGIN, the
// role/search_path/GUC settings, and the rollback. This is the fixed cost every
// request pays before its own statement runs.
func BenchmarkBeginRollback(b *testing.B) {
	cfg := testConfig(dbtest.DSN(b))
	pool, err := NewPool(b.Context(), cfg)
	if err != nil {
		b.Fatalf("NewPool: %v", err)
	}
	b.Cleanup(pool.Close)

	settings := RequestSettings{
		Role:       "anon",
		SearchPath: []string{"public"},
		GUCs: map[string]string{
			"request.method":     "GET",
			"request.path":       "/todos",
			"request.headers":    `{"accept":"application/json"}`,
			"request.jwt.claims": `{"role":"anon"}`,
		},
		ReadOnly: true,
	}
	ctx := context.Background()

	b.ReportAllocs()
	for b.Loop() {
		tx, err := Begin(ctx, pool, settings)
		if err != nil {
			b.Fatal(err)
		}
		if err := tx.Rollback(ctx); err != nil {
			b.Fatal(err)
		}
	}
}
