package httpapi

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/andeshq/subquery/internal/pgrst"
	"github.com/andeshq/subquery/internal/query"
	"github.com/andeshq/subquery/internal/schema"
	"github.com/andeshq/subquery/internal/sqlgen"
)

// handleRPC serves /rpc/{fn}.
func (s *Server) handleRPC(w http.ResponseWriter, r *http.Request) {
	r, principal, apiErr := s.authenticate(r)
	if apiErr != nil {
		s.writeError(w, r, apiErr)
		return
	}
	name := r.PathValue("fn")

	snap := s.store.Snapshot()
	if snap == nil {
		s.writeError(w, r, &APIError{Status: http.StatusInternalServerError, Code: pgrst.SchemaNotLoaded, Message: "The schema cache has not been loaded"})
		return
	}

	if r.Method == http.MethodOptions {
		// PostgREST treats OPTIONS like a parameterless call: a function that
		// requires arguments cannot be resolved, which is a 404.
		provided := slices.Sorted(maps.Keys(argumentNames(r.URL.Query(), snapshotArgNames(snap, name))))
		fn, schemaErr := snap.ResolveFunction(name, provided)
		if schemaErr != nil {
			s.writeError(w, r, apiErrorFromSchema(schemaErr))
			return
		}
		w.Header().Set("Allow", rpcAllowFor(fn))
		w.WriteHeader(http.StatusOK)
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodPost:
	default:
		s.writeError(w, r, errMethodNotAllowed)
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
	argNames := snapshotArgNames(snap, name)

	// Arguments ride on the query string for GET and on the body for POST.
	argValues := argumentNames(values, argNames)
	filterValues := map[string][]string{}
	for key, vals := range values {
		if _, isArg := argValues[key]; isArg && r.Method != http.MethodPost {
			continue
		}
		filterValues[key] = vals
	}

	q, err := query.Parse(filterValues)
	if err != nil {
		s.writeError(w, r, queryAPIError(err))
		return
	}
	applyPreferences(r, q)

	provided := slices.Sorted(maps.Keys(argValues))
	var (
		bodyArgs map[string]jsontext.Value
		rawBody  []byte
	)

	if r.Method == http.MethodPost {
		raw, apiErr := readRPCBody(r)
		if apiErr != nil {
			s.writeError(w, r, apiErr)
			return
		}
		rawBody = raw
		provided = nil
		if len(bytes.TrimSpace(raw)) > 0 {
			if err := json.Unmarshal(raw, &bodyArgs); err == nil {
				provided = slices.Sorted(maps.Keys(bodyArgs))
			}
		}
	}

	fn, schemaErr := snap.ResolveFunction(name, provided)
	if schemaErr != nil {
		s.writeError(w, r, apiErrorFromSchema(schemaErr))
		return
	}

	args, apiErr := buildArgs(fn, r, argValues, bodyArgs, rawBody)
	if apiErr != nil {
		s.writeError(w, r, apiErr)
		return
	}

	statement, err := sqlgen.RPC(snap, fn, q, args, sqlgen.ReadOptions{MaxRows: s.cfg.DBMaxRows})
	if err != nil {
		s.writeQueryError(w, r, err)
		return
	}
	s.logQuery(r.Context(), statement.SQL)

	s.executeRPC(w, r, principal, fn, statement, q, shape)
}

// executeRPC runs the call and renders the result.
func (s *Server) executeRPC(w http.ResponseWriter, r *http.Request, principal Principal, fn *schema.Function, statement *sqlgen.Statement, q *query.Query, shape responseShape) {
	prefs, apiErr := parsePreferences(r.Header.Get("Prefer"), s.cfg.DBTxEnd)
	if apiErr != nil {
		s.writeError(w, r, apiErr)
		return
	}

	// GET and HEAD always run read-only, so a function that writes fails in
	// PostgreSQL exactly as it does under PostgREST.
	readOnly := r.Method == http.MethodGet || r.Method == http.MethodHead

	if fn.Kind == schema.ReturnVoid {
		if err := s.inTx(r, principal, readOnly, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, statement.SQL, statement.Args...)
			return err
		}); err != nil {
			s.writeDBError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}

	var (
		body     string
		returned int64
		total    int64
	)
	wantCount := q.Count != query.CountNone && fn.Kind == schema.ReturnTable
	if err := s.inTx(r, principal, readOnly, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		switch {
		case wantCount:
			err = tx.QueryRow(ctx, statement.SQL, statement.Args...).Scan(&body, &returned, &total)
		case fn.Kind == schema.ReturnTable:
			err = tx.QueryRow(ctx, statement.SQL, statement.Args...).Scan(&body, &returned)
		default:
			// Scalar and record results are a single JSON value.
			returned = 1
			err = tx.QueryRow(ctx, statement.SQL, statement.Args...).Scan(&body)
		}
		if err != nil {
			return err
		}
		if prefs.Strict && prefs.MaxAffected != nil && returned > *prefs.MaxAffected {
			return errMaxAffected(pgrst.MaxAffectedRPC, returned)
		}
		if prefs.TxRollback {
			return errRollbackRequested
		}
		return nil
	}); err != nil {
		s.writeError(w, r, serviceError(r, err))
		return
	}

	if len(prefs.applied) > 0 {
		w.Header().Set("Preference-Applied", strings.Join(prefs.applied, ", "))
	}

	// A table-returning call is the only RPC shape that is a row set, so it is
	// the only one the object media type can constrain.
	if shape == shapeObject && fn.Kind == schema.ReturnTable {
		if returned != 1 {
			s.writeError(w, r, errNotSingular(returned))
			return
		}
		body = unwrapSingular(body)
	}

	// PostgREST reports a row range for every result shape, including scalars.
	var totalPtr *int64
	if wantCount {
		totalPtr = new(total)
	}
	writeRowHeaders(w, q.Offset, returned, totalPtr, shape)
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = io.WriteString(w, body)
}
