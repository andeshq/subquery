package httpapi

import (
	"context"
	"io"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/andeshq/subquery/internal/pgrst"
	"github.com/andeshq/subquery/internal/query"
	"github.com/andeshq/subquery/internal/schema"
	"github.com/andeshq/subquery/internal/sqlgen"
)

// writeRelation serves POST, PATCH, PUT, and DELETE.
func (s *Server) writeRelation(w http.ResponseWriter, r *http.Request, snap *schema.Snapshot, rel *schema.Relation) {
	prefs, apiErr := parsePreferences(r.Header.Get("Prefer"), s.cfg.DBTxEnd)
	if apiErr != nil {
		s.writeError(w, r, apiErr)
		return
	}

	shape, apiErr := negotiateAccept(r.Header.Get("Accept"))
	if apiErr != nil {
		s.writeError(w, r, apiErr)
		return
	}
	// CSV output is implemented for relation reads only.
	if shape == shapeCSV {
		shape = shapeArray
	}

	values := r.URL.Query()
	q, err := query.ParseWith(values, query.ParseOptions{Write: true})
	if err != nil {
		s.writeError(w, r, queryAPIError(err))
		return
	}

	kind, apiErr := writeKindFor(r.Method)
	if apiErr != nil {
		s.writeError(w, r, apiErr)
		return
	}
	if kind != sqlgen.WriteInsert && (q.Limit != nil || q.Offset != 0 || len(q.Order) > 0) {
		s.writeError(w, r, &APIError{
			Status:  http.StatusBadRequest,
			Code:    pgrst.NotImplemented,
			Message: "limit, offset, and order on writes are not implemented yet",
		})
		return
	}
	if kind == sqlgen.WriteInsert && len(q.Where) > 0 {
		s.writeError(w, r, &APIError{
			Status:  http.StatusBadRequest,
			Code:    pgrst.ParseError,
			Message: "Filters are not supported on POST; use PATCH or DELETE",
		})
		return
	}

	write := &sqlgen.WriteQuery{Kind: kind, Where: q.Where, Select: q.Select, Resolution: prefs.Resolution}
	if kind == sqlgen.WriteDelete {
		if apiErr := s.executeWrite(w, r, snap, rel, write, prefs, shape); apiErr != nil {
			s.writeError(w, r, apiErr)
		}
		return
	}

	raw, apiErr := readWriteBody(r)
	if apiErr != nil {
		s.writeError(w, r, apiErr)
		return
	}
	body, apiErr := parsePayload(raw, kind != sqlgen.WriteUpdate)
	if apiErr != nil {
		s.writeError(w, r, apiErr)
		return
	}

	switch kind {
	case sqlgen.WriteInsert:
		if len(q.Columns) == 0 {
			if apiErr := validateKeys(rel, body.keys); apiErr != nil {
				s.writeError(w, r, apiErr)
				return
			}
		}
		write.Columns = insertColumns(body.keys, q.Columns)
		write.Conflict = q.OnConflict
		write.Payload = body.asArray()
	case sqlgen.WriteUpdate:
		if apiErr := validateKeys(rel, body.keys); apiErr != nil {
			s.writeError(w, r, apiErr)
			return
		}
		write.Columns = insertColumns(body.keys, q.Columns)
		write.Payload = body.raw
	case sqlgen.WriteUpsert:
		if apiErr := validateKeys(rel, body.keys); apiErr != nil {
			s.writeError(w, r, apiErr)
			return
		}
		if apiErr := validatePut(rel, body, q.Where); apiErr != nil {
			s.writeError(w, r, apiErr)
			return
		}
		write.Columns = insertColumns(body.keys, nil)
		write.Payload = body.asArray()
	}

	if apiErr := s.executeWrite(w, r, snap, rel, write, prefs, shape); apiErr != nil {
		s.writeError(w, r, apiErr)
	}
}

// executeWrite runs the statement and renders the response.
func (s *Server) executeWrite(w http.ResponseWriter, r *http.Request, snap *schema.Snapshot, rel *schema.Relation, write *sqlgen.WriteQuery, prefs preferences, shape responseShape) *APIError {
	statement, err := sqlgen.Write(snap, rel, write, sqlgen.WriteOptions{Return: prefs.Return})
	if err != nil {
		return s.generationError(err)
	}
	s.logQuery(r.Context(), statement.SQL)

	principal, _ := PrincipalFrom(r.Context())
	var (
		returned int64
		inserted bool
		respBody string
		location string
	)

	if err := s.inTx(r, principal, false, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		switch {
		case statement.Representation:
			if statement.Inserted {
				err = tx.QueryRow(ctx, statement.SQL, statement.Args...).Scan(&respBody, &returned, &inserted)
			} else {
				err = tx.QueryRow(ctx, statement.SQL, statement.Args...).Scan(&respBody, &returned)
			}
		case statement.Scan:
			var rows pgx.Rows
			rows, err = tx.Query(ctx, statement.SQL, statement.Args...)
			if err == nil {
				returned, inserted, location = collectReturning(rows, rel, statement)
				err = rows.Err()
			}
		default:
			var tag pgconn.CommandTag
			tag, err = tx.Exec(ctx, statement.SQL, statement.Args...)
			if err == nil {
				returned = tag.RowsAffected()
			}
		}
		if err != nil {
			return err
		}
		if prefs.Strict && prefs.MaxAffected != nil && returned > *prefs.MaxAffected {
			return errMaxAffected(pgrst.MaxAffected, returned)
		}
		if prefs.TxRollback {
			return errRollbackRequested
		}
		return nil
	}); err != nil {
		return serviceError(r, err)
	}

	if len(prefs.applied) > 0 {
		w.Header().Set("Preference-Applied", strings.Join(prefs.applied, ", "))
	}
	if location != "" {
		w.Header().Set("Location", location)
	}

	// An object response only makes sense when there is a body to shape.
	if shape == shapeObject && statement.Representation {
		if returned != 1 {
			return errNotSingular(returned)
		}
		respBody = unwrapSingular(respBody)
	}

	status := writeStatus(write.Kind, prefs.Return, write.Resolution, inserted)
	// A minimal PUT reports no range at all.
	if !(write.Kind == sqlgen.WriteUpsert && prefs.Return == sqlgen.ReturnMinimal) {
		w.Header().Set("Content-Range", writeContentRange(write.Kind, returned))
	}
	if statement.Representation {
		w.Header().Set("Content-Type", shape.contentType())
		w.WriteHeader(status)
		_, _ = io.WriteString(w, respBody)
		return nil
	}
	w.WriteHeader(status)
	return nil
}
