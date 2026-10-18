package httpapi

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/andeshq/subquery/internal/pgrst"
	"github.com/andeshq/subquery/internal/query"
	"github.com/andeshq/subquery/internal/schema"
	"github.com/andeshq/subquery/internal/sqlgen"
)

// writeRowHeaders sets the headers every row-returning response carries and
// returns the offset PostgREST reports, which is zero when no rows came back.
func writeRowHeaders(w http.ResponseWriter, offset, returned int64, total *int64, shape responseShape) int64 {
	if returned == 0 {
		offset = 0
	}
	w.Header().Set("Content-Type", shape.contentType())
	w.Header().Set("Range-Unit", "items")
	w.Header().Set("Content-Range", contentRange(offset, returned, total))
	return offset
}

// responseShape is the negotiated representation of a row-returning response.
type responseShape int

const (
	// shapeArray is the default: a JSON array of rows.
	shapeArray responseShape = iota
	// shapeObject is application/vnd.pgrst.object+json: exactly one JSON object.
	shapeObject
	// shapeCSV is text/csv: one row per line, with a header line.
	shapeCSV
)

// contentType renders the Content-Type for the shape.
func (s responseShape) contentType() string {
	switch s {
	case shapeObject:
		return "application/vnd.pgrst.object+json; charset=utf-8"
	case shapeCSV:
		return "text/csv; charset=utf-8"
	default:
		return "application/json; charset=utf-8"
	}
}

// negotiateAccept chooses the response shape from the Accept header, or rejects
// media types Subquery cannot render.
func negotiateAccept(header string) (responseShape, *APIError) {
	header = strings.TrimSpace(header)
	if header == "" {
		return shapeArray, nil
	}
	for accept := range strings.SplitSeq(header, ",") {
		media, _, _ := strings.Cut(strings.TrimSpace(accept), ";")
		media = strings.TrimSpace(media)
		switch {
		case media == "application/vnd.pgrst.object+json":
			return shapeObject, nil
		case media == "text/csv":
			return shapeCSV, nil
		case media == "*/*" || media == "application/*" ||
			strings.HasPrefix(media, "application/json") || strings.HasSuffix(media, "+json"):
			return shapeArray, nil
		}
	}
	return shapeArray, &APIError{Status: http.StatusNotAcceptable, Code: pgrst.UnsupportedMediaType, Message: "None of the media types in the Accept header are supported"}
}

// renderCSV renders the header and one line per row, matching PostgREST:
// a header line then the rows, and a bare newline when there are no rows.
func renderCSV(header []string, rows [][]string) string {
	if len(rows) == 0 {
		return "\n"
	}
	var sb strings.Builder
	sb.WriteString(strings.Join(header, ","))
	for _, row := range rows {
		sb.WriteByte('\n')
		for i, field := range row {
			if i > 0 {
				sb.WriteByte(',')
			}
			sb.WriteString(csvField(field))
		}
	}
	return sb.String()
}

// csvField quotes a field when it contains a delimiter, a quote, a line break,
// or a space, doubling embedded quotes. PostgREST quotes on a space too, which
// plain PostgreSQL COPY does not.
func csvField(value string) string {
	if !strings.ContainsAny(value, ",\"\r\n ") {
		return value
	}
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

// unwrapSingular turns the one-element JSON array PostgreSQL produced into its
// element. The caller has already established that exactly one row was returned.
func unwrapSingular(body string) string {
	if len(body) >= 2 && body[0] == '[' && body[len(body)-1] == ']' {
		return body[1 : len(body)-1]
	}
	return body
}

// contentRange renders the RFC 7233 header PostgREST returns.
func contentRange(offset, returned int64, total *int64) string {
	span := "*"
	if total != nil {
		span = strconv.FormatInt(*total, 10)
	}
	if returned == 0 {
		return "*/" + span
	}
	return strconv.FormatInt(offset, 10) + "-" + strconv.FormatInt(offset+returned-1, 10) + "/" + span
}

// writeContentRange reports the range Prefer covers. PostgREST reports the
// affected window for PATCH and an unspecified range for the other verbs.
func writeContentRange(kind sqlgen.WriteKind, returned int64) string {
	if kind == sqlgen.WriteUpdate && returned > 0 {
		return contentRange(0, returned, nil)
	}
	return "*/*"
}

func writeStatus(kind sqlgen.WriteKind, mode sqlgen.ReturnMode, resolution sqlgen.Resolution, inserted bool) int {
	switch kind {
	case sqlgen.WriteInsert:
		// A merge that updated an existing row reports 200; ignore-duplicates
		// never updates, so it always reports 201.
		if resolution == sqlgen.ResolutionMerge && !inserted {
			return http.StatusOK
		}
		return http.StatusCreated
	case sqlgen.WriteUpsert:
		if mode == sqlgen.ReturnMinimal {
			return http.StatusNoContent
		}
		if inserted {
			return http.StatusCreated
		}
		return http.StatusOK
	default:
		if mode == sqlgen.ReturnRepresentation {
			return http.StatusOK
		}
		return http.StatusNoContent
	}
}

// countName renders the count preference back as its wire name.
func countName(mode query.CountMode) string {
	switch mode {
	case query.CountExact:
		return "exact"
	case query.CountPlanned:
		return "planned"
	case query.CountEstimated:
		return "estimated"
	}
	return ""
}

// buildLocation renders PostgREST's Location header for a single created row.
func buildLocation(rel *schema.Relation, columns []string, values []any) string {
	params := url.Values{}
	for i, column := range columns {
		if i >= len(values) {
			break
		}
		params.Set(column, "eq."+fmt.Sprint(values[i]))
	}
	return "/" + rel.ID.Name + "?" + params.Encode()
}

// collectReturning drains a headers-only RETURNING projection.
func collectReturning(rows pgx.Rows, rel *schema.Relation, statement *sqlgen.WriteStatement) (int64, bool, string) {
	var (
		count    int64
		inserted bool
		location string
	)
	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			break
		}
		if count == 0 && len(statement.ReturnedColumns) > 0 && len(values) >= len(statement.ReturnedColumns) {
			location = buildLocation(rel, statement.ReturnedColumns, values)
		}
		if statement.Inserted && len(values) > 0 {
			if flag, ok := values[len(values)-1].(bool); ok {
				inserted = flag
			}
		}
		count++
	}
	if count != 1 {
		// Location names a single created row.
		location = ""
	}
	return count, inserted, location
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// pgStatus maps PostgreSQL SQLSTATE classes to HTTP statuses, following
// PostgREST's documented table.
func pgStatus(code string, r *http.Request) int {
	switch code {
	case "23503", "23505":
		return http.StatusConflict // 409
	case "25006":
		return http.StatusMethodNotAllowed // 405
	case "42501":
		if principal, ok := PrincipalFrom(r.Context()); ok && len(principal.Claims) > 0 {
			return http.StatusForbidden
		}
		return http.StatusUnauthorized
	case "42883", "42P01":
		return http.StatusNotFound // 404
	case "P0001":
		return http.StatusBadRequest // 400
	case "57014":
		return http.StatusInternalServerError
	}
	switch {
	case strings.HasPrefix(code, "08"):
		return http.StatusServiceUnavailable // 503
	case strings.HasPrefix(code, "09"),
		strings.HasPrefix(code, "25"),
		strings.HasPrefix(code, "2D"),
		strings.HasPrefix(code, "38"),
		strings.HasPrefix(code, "39"):
		return http.StatusInternalServerError
	case strings.HasPrefix(code, "0L"), strings.HasPrefix(code, "0P"),
		strings.HasPrefix(code, "28"):
		return http.StatusForbidden // 403
	case strings.HasPrefix(code, "40"), strings.HasPrefix(code, "53"),
		strings.HasPrefix(code, "54"), strings.HasPrefix(code, "55"),
		strings.HasPrefix(code, "57"), strings.HasPrefix(code, "58"),
		strings.HasPrefix(code, "F0"), strings.HasPrefix(code, "HV"),
		strings.HasPrefix(code, "P0"), strings.HasPrefix(code, "XX"):
		return http.StatusInternalServerError
	}
	return http.StatusBadRequest
}
