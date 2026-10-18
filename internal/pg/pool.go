// Package pg owns the connection pool and the per-request transaction setup
// that impersonates a Postgres role.
package pg

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/andeshq/subquery/internal/config"
)

// NewPool builds the pgx pool from db-uri and pool settings.
func NewPool(ctx context.Context, cfg *config.Config) (*pgxpool.Pool, error) {
	pc, err := poolConfig(cfg)
	if err != nil {
		return nil, err
	}
	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, cfg.DBPoolAcquisitionAfter)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect to database: %w", err)
	}
	return pool, nil
}

// poolConfig derives the pgx pool configuration from the Subquery settings.
//
// db-prepared-statements controls pgx's statement handling. The default,
// CacheStatement, prepares and caches one statement per distinct SQL text, which
// is what makes the generator's deterministic SQL pay off. Disabling it switches
// to the extended protocol without named prepared statements, which
// transaction-pooling proxies and some RLS setups require. Values stay bind
// parameters in both modes; only the caching differs.
func poolConfig(cfg *config.Config) (*pgxpool.Config, error) {
	pc, err := pgxpool.ParseConfig(cfg.DBURI)
	if err != nil {
		return nil, fmt.Errorf("parse db-uri: %w", err)
	}
	pc.MaxConns = int32(cfg.DBPool)
	pc.MaxConnIdleTime = cfg.DBPoolMaxIdleTime
	pc.MaxConnLifetime = cfg.DBPoolMaxLifetime
	if !cfg.DBPreparedStatements {
		pc.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeExec
	}
	return pc, nil
}

// RequestSettings describes the transaction-scoped state for one HTTP request.
type RequestSettings struct {
	// Role is the impersonated Postgres role. Empty leaves the connection's
	// own role in place.
	Role string
	// SearchPath is applied with SET LOCAL search_path.
	SearchPath []string
	// GUCs are applied with set_config(name, value, true). PostgREST uses
	// request.jwt.claims, request.headers, request.cookies, request.method,
	// request.path, and app.settings.*.
	GUCs map[string]string
	// PreRequest is a schema-qualified function called before the request
	// query, matching db-pre-request.
	PreRequest string
	// ReadOnly begins a read-only transaction, as PostgREST does for GET/HEAD.
	ReadOnly bool
}

// Beginner begins transactions. *pgxpool.Pool satisfies it.
type Beginner interface {
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

// Begin starts a transaction and applies the request settings. The caller owns
// Commit/Rollback.
func Begin(ctx context.Context, pool Beginner, s RequestSettings) (pgx.Tx, error) {
	opts := pgx.TxOptions{}
	if s.ReadOnly {
		opts.AccessMode = pgx.ReadOnly
	}
	tx, err := pool.BeginTx(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	if err := apply(ctx, tx, s); err != nil {
		_ = tx.Rollback(ctx)
		return nil, err
	}
	return tx, nil
}

// apply applies the transaction setup: search_path, the impersonated role, the
// request GUCs, and db-pre-request, in that order.
//
// The first three are sent as one batch, so they cost a single round trip. The
// pre-request is not batched: pgx prepares a batch's statements before any of
// them run, and a pre-request is analysed with the caller's privileges, which
// SET LOCAL role has not granted yet at that point. Running it after the batch
// means it executes as the impersonated role, exactly as PostgREST does.
func apply(ctx context.Context, tx pgx.Tx, s RequestSettings) error {
	batch := &pgx.Batch{}
	var labels []string

	if len(s.SearchPath) > 0 {
		// search_path is a setting, not an identifier list, so each schema is
		// quoted as an identifier.
		quoted := make([]string, 0, len(s.SearchPath))
		for _, schema := range s.SearchPath {
			quoted = append(quoted, pgx.Identifier{schema}.Sanitize())
		}
		batch.Queue("set local search_path to " + strings.Join(quoted, ", "))
		labels = append(labels, "set search_path")
	}
	if s.Role != "" {
		batch.Queue("set local role " + pgx.Identifier{s.Role}.Sanitize())
		labels = append(labels, fmt.Sprintf("set role %q", s.Role))
	}
	if len(s.GUCs) > 0 {
		// One set_config call per setting, in one statement.
		names := slices.Sorted(maps.Keys(s.GUCs))
		parts := make([]string, 0, len(names))
		args := make([]any, 0, len(names)*2)
		for i, name := range names {
			parts = append(parts, fmt.Sprintf("set_config($%d, $%d, true)", i*2+1, i*2+2))
			args = append(args, name, s.GUCs[name])
		}
		batch.Queue("select "+strings.Join(parts, ", "), args...)
		labels = append(labels, "set request settings")
	}
	if batch.Len() > 0 {
		results := tx.SendBatch(ctx, batch)
		for i := range labels {
			if _, err := results.Exec(); err != nil {
				_ = results.Close()
				return fmt.Errorf("%s: %w", labels[i], err)
			}
		}
		if err := results.Close(); err != nil {
			return fmt.Errorf("apply request settings: %w", err)
		}
	}

	if s.PreRequest != "" {
		fn, err := qualifiedIdentifier(s.PreRequest)
		if err != nil {
			return fmt.Errorf("db-pre-request: %w", err)
		}
		if _, err := tx.Exec(ctx, "select "+fn+"()"); err != nil {
			return fmt.Errorf("call db-pre-request %s: %w", s.PreRequest, err)
		}
	}
	return nil
}

// qualifiedIdentifier quotes a possibly schema-qualified name.
func qualifiedIdentifier(name string) (string, error) {
	parts := strings.Split(name, ".")
	if len(parts) > 2 {
		return "", fmt.Errorf("%q is not a schema-qualified name", name)
	}
	for i, part := range parts {
		if part = strings.TrimSpace(part); part == "" {
			return "", fmt.Errorf("%q is not a schema-qualified name", name)
		}
		parts[i] = pgx.Identifier{part}.Sanitize()
	}
	return strings.Join(parts, "."), nil
}
