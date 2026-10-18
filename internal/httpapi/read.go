package httpapi

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/andeshq/subquery/internal/pgrst"
	"github.com/andeshq/subquery/internal/query"
	"github.com/andeshq/subquery/internal/schema"
	"github.com/andeshq/subquery/internal/sqlgen"
)

// readRelation serves a read through the M2 pipeline: parse, generate,
// execute, and shape the response.
func (s *Server) readRelation(w http.ResponseWriter, r *http.Request, snap *schema.Snapshot, rel *schema.Relation) {
	q, shape, apiErr := s.parseReadQuery(r)
	if apiErr != nil {
		s.writeError(w, r, apiErr)
		return
	}
	if shape == shapeCSV {
		s.readRelationCSV(w, r, snap, rel, q)
		return
	}

	statement, err := sqlgen.Read(snap, rel, q, sqlgen.ReadOptions{MaxRows: s.cfg.DBMaxRows})
	if err != nil {
		s.writeQueryError(w, r, err)
		return
	}
	s.logQuery(r.Context(), statement.SQL)

	principal, _ := PrincipalFrom(r.Context())
	var (
		body     string
		returned int64
		total    int64
	)
	wantCount := q.Count != query.CountNone
	if err := s.inTx(r, principal, true, func(ctx context.Context, tx pgx.Tx) error {
		if wantCount {
			return tx.QueryRow(ctx, statement.SQL, statement.Args...).Scan(&body, &returned, &total)
		}
		return tx.QueryRow(ctx, statement.SQL, statement.Args...).Scan(&body, &returned)
	}); err != nil {
		s.writeDBError(w, r, err)
		return
	}

	var totalPtr *int64
	if wantCount {
		totalPtr = new(total)
	}
	if shape == shapeObject {
		if returned != 1 {
			s.writeError(w, r, errNotSingular(returned))
			return
		}
		body = unwrapSingular(body)
	}
	offset := writeRowHeaders(w, q.Offset, returned, totalPtr, shape)
	if wantCount {
		w.Header().Set("Preference-Applied", "count="+countName(q.Count))
	}
	status := http.StatusOK
	if totalPtr != nil && offset+returned < *totalPtr {
		status = http.StatusPartialContent
	}
	w.WriteHeader(status)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = io.WriteString(w, body)
}

// readRelationCSV serves a read as CSV. Only plain columns, aliases, and casts
// are supported; a richer selection is refused with the media-type error,
// because its header would not be a plain column name.
func (s *Server) readRelationCSV(w http.ResponseWriter, r *http.Request, snap *schema.Snapshot, rel *schema.Relation, q *query.Query) {
	statement, err := sqlgen.ReadCSV(snap, rel, q, sqlgen.ReadOptions{MaxRows: s.cfg.DBMaxRows})
	if err != nil {
		if unsupported, ok := errors.AsType[*sqlgen.UnsupportedError](err); ok {
			s.writeError(w, r, &APIError{
				Status:  http.StatusNotAcceptable,
				Code:    pgrst.UnsupportedMediaType,
				Message: unsupported.Error(),
				Details: "CSV output supports plain columns, aliases, and casts",
			})
			return
		}
		s.writeQueryError(w, r, err)
		return
	}
	s.logQuery(r.Context(), statement.SQL)

	principal, _ := PrincipalFrom(r.Context())
	var (
		rows  [][]string
		total int64
	)
	wantCount := q.Count != query.CountNone
	if err := s.inTx(r, principal, true, func(ctx context.Context, tx pgx.Tx) error {
		if wantCount {
			if err := tx.QueryRow(ctx, statement.CountSQL, statement.Args...).Scan(&total); err != nil {
				return err
			}
		}
		result, err := tx.Query(ctx, statement.SQL, statement.Args...)
		if err != nil {
			return err
		}
		defer result.Close()
		for result.Next() {
			values, err := result.Values()
			if err != nil {
				return err
			}
			row := make([]string, len(values))
			for i, value := range values {
				switch typed := value.(type) {
				case nil:
					// A NULL is an empty field.
				case string:
					row[i] = typed
				case []byte:
					row[i] = string(typed)
				default:
					row[i] = fmt.Sprint(typed)
				}
			}
			rows = append(rows, row)
		}
		return result.Err()
	}); err != nil {
		s.writeDBError(w, r, err)
		return
	}

	returned := int64(len(rows))
	var totalPtr *int64
	if wantCount {
		totalPtr = new(total)
	}
	offset := writeRowHeaders(w, q.Offset, returned, totalPtr, shapeCSV)
	if wantCount {
		w.Header().Set("Preference-Applied", "count="+countName(q.Count))
	}
	status := http.StatusOK
	if totalPtr != nil && offset+returned < *totalPtr {
		status = http.StatusPartialContent
	}
	w.WriteHeader(status)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = io.WriteString(w, renderCSV(statement.Columns, rows))
}

// parseReadQuery merges the query string, the Range header, and the Prefer
// header into a single query, and negotiates the response shape.
func (s *Server) parseReadQuery(r *http.Request) (*query.Query, responseShape, *APIError) {
	values := r.URL.Query()

	shape, apiErr := negotiateAccept(r.Header.Get("Accept"))
	if apiErr != nil {
		return nil, shapeArray, apiErr
	}

	q, err := query.Parse(values)
	if err != nil {
		return nil, shapeArray, queryAPIError(err)
	}

	if err := applyRangeHeader(r, q); err != nil {
		return nil, shapeArray, err
	}
	applyPreferences(r, q)
	if q.HasAggregate() && !s.cfg.DBAggregatesEnabled {
		return nil, shapeArray, &APIError{
			Status:  http.StatusBadRequest,
			Code:    pgrst.AggregatesDisabled,
			Message: "Aggregate functions are disabled",
			Details: "Enable them with db-aggregates-enabled",
		}
	}
	return q, shape, nil
}

// applyRangeHeader lets `Range: 0-19` or `Range: 10-` override limit/offset.
func applyRangeHeader(r *http.Request, q *query.Query) *APIError {
	header := strings.TrimSpace(r.Header.Get("Range"))
	if header == "" {
		return nil
	}
	if unit := strings.TrimSpace(r.Header.Get("Range-Unit")); unit != "" && !strings.EqualFold(unit, "items") {
		return &APIError{Status: http.StatusRequestedRangeNotSatisfiable, Code: pgrst.InvalidRange, Message: "Invalid range"}
	}
	// A range that is not `start-end` with numeric parts is ignored, as
	// PostgREST ignores it.
	start, end, ok := strings.Cut(header, "-")
	if !ok || strings.Contains(end, "-") || (start == "" && end == "") || !numeric(start) || !numeric(end) {
		return nil
	}
	offset, err := strconv.ParseInt(start, 10, 64)
	if err != nil || offset < 0 {
		return &APIError{Status: http.StatusRequestedRangeNotSatisfiable, Code: pgrst.InvalidRange, Message: "Invalid range"}
	}
	q.Offset = offset
	q.Limit = nil
	if end != "" {
		last, err := strconv.ParseInt(end, 10, 64)
		if err != nil || last < offset {
			return nil
		}
		q.Limit = new(last - offset + 1)
	}
	return nil
}

func numeric(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// applyPreferences reads the subset of Prefer that affects reads.
func applyPreferences(r *http.Request, q *query.Query) {
	eachPreference(r.Header.Get("Prefer"), func(entry string) {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || name != "count" {
			return
		}
		switch value {
		case "exact":
			q.Count = query.CountExact
		case "planned":
			q.Count = query.CountPlanned
		case "estimated":
			q.Count = query.CountEstimated
		}
	})
}

// requestGUCs builds the request.* and app.settings.* settings Postgres
// functions and RLS policies read. request.jwt.claims is set only when a token
// was presented, matching PostgREST, so `current_setting(..., true)` is NULL
// for anonymous requests.
func (s *Server) requestGUCs(r *http.Request, principal Principal) map[string]string {
	gucs := map[string]string{
		"request.method": r.Method,
		"request.path":   r.URL.Path,
	}
	// request.jwt.claims is always set. PostgREST synthesizes {"role": <anon>}
	// for a request without a token; leaving it unset is not equivalent,
	// because once a placeholder exists on a connection it reverts to the empty
	// string rather than to NULL when the transaction ends, and a policy that
	// casts it to json would then fail on the next anonymous request that reuses
	// that connection.
	if len(principal.Claims) > 0 {
		gucs["request.jwt.claims"] = string(principal.Claims)
	} else if principal.Role != "" {
		if encoded, err := json.Marshal(map[string]string{"role": principal.Role}); err == nil {
			gucs["request.jwt.claims"] = string(encoded)
		}
	}

	headers := make(map[string]string, len(r.Header))
	for name, values := range r.Header {
		headers[strings.ToLower(name)] = strings.Join(values, ",")
	}
	if encoded, err := json.Marshal(headers); err == nil {
		gucs["request.headers"] = string(encoded)
	}

	// Cookies are always set too, so a cookie-less request never leaves an empty
	// placeholder behind.
	jar := make(map[string]string, len(r.Cookies()))
	for _, c := range r.Cookies() {
		jar[c.Name] = c.Value
	}
	if encoded, err := json.Marshal(jar); err == nil {
		gucs["request.cookies"] = string(encoded)
	}

	for name, value := range s.cfg.AppSettings {
		gucs["app.settings."+name] = value
	}
	return gucs
}
