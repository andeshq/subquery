package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/andeshq/subquery/internal/pg"
)

// errRollbackRequested makes inTx roll back and report success, which is how
// Prefer: tx=rollback terminates a request.
var errRollbackRequested = errors.New("transaction rollback requested")

// inTx runs fn inside the per-request transaction, applying the role,
// search_path, request GUCs, and db-pre-request, then commits. An error from fn
// rolls the transaction back; errRollbackRequested rolls back and reports
// success. The whole transaction is bounded by db-request-timeout.
func (s *Server) inTx(r *http.Request, principal Principal, readOnly bool, fn func(context.Context, pgx.Tx) error) error {
	ctx := r.Context()
	if s.cfg.DBRequestTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.cfg.DBRequestTimeout)
		defer cancel()
	}
	tx, err := pg.Begin(ctx, s.pool, pg.RequestSettings{
		Role:       principal.Role,
		SearchPath: s.cfg.SearchPath(),
		GUCs:       s.requestGUCs(r, principal),
		PreRequest: s.cfg.DBPreRequest,
		ReadOnly:   readOnly,
	})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := fn(ctx, tx); err != nil {
		if errors.Is(err, errRollbackRequested) {
			return tx.Rollback(ctx)
		}
		return err
	}
	return tx.Commit(ctx)
}
