package httpapi

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/andeshq/subquery/internal/config"
	"github.com/andeshq/subquery/internal/pgrst"
	"github.com/andeshq/subquery/internal/query"
	"github.com/andeshq/subquery/internal/schema"
	"github.com/andeshq/subquery/internal/sqlgen"
)

// APIError is a PostgREST-shaped error. Code is either a PGRST code or a
// PostgreSQL SQLSTATE.
type APIError struct {
	Status  int
	Code    string
	Message string
	Details any
	Hint    any
}

func (e *APIError) Error() string { return e.Code + ": " + e.Message }

// WithDetails returns a copy carrying details.
func (e *APIError) WithDetails(details any) *APIError {
	clone := *e
	clone.Details = details
	return &clone
}

// PostgREST error codes used by the server itself.
var (
	errSecretMissing = &APIError{
		Status: http.StatusInternalServerError, Code: pgrst.SecretMissing,
		Message: "JWT secret is missing from the configuration",
	}
	errInvalidToken = &APIError{
		Status: http.StatusUnauthorized, Code: pgrst.InvalidToken,
		Message: "Invalid authentication credentials",
	}
	errAnonDisabled = &APIError{
		Status: http.StatusUnauthorized, Code: pgrst.AnonDisabled,
		Message: "Anonymous access is disabled",
	}
	errInvalidClaims = &APIError{
		Status: http.StatusUnauthorized, Code: pgrst.InvalidClaims,
		Message: "JWT claims validation failed",
	}
	errMethodNotAllowed = &APIError{
		Status: http.StatusMethodNotAllowed, Code: pgrst.MethodNotAllowed,
		Message: "Unsupported HTTP method",
	}
	errInvalidPath = &APIError{
		Status: http.StatusNotFound, Code: pgrst.InvalidPath,
		Message: "Invalid path",
	}
	errOpenAPIDisabled = &APIError{
		Status: http.StatusNotFound, Code: pgrst.OpenAPIDisabled,
		Message: "Open API is disabled",
	}
	errInternalPanic = &APIError{
		Status: http.StatusInternalServerError, Code: pgrst.Internal,
		Message: "Internal server error",
	}
)

// queryAPIError maps a query parse failure onto the API contract.
func queryAPIError(err error) *APIError {
	if parseErr, ok := errors.AsType[*query.Error](err); ok {
		return &APIError{
			Status:  http.StatusBadRequest,
			Code:    parseErr.Code,
			Message: parseErr.Message,
			Details: nullableString(parseErr.Details),
		}
	}
	return &APIError{Status: http.StatusBadRequest, Code: pgrst.ParseError, Message: err.Error()}
}

// generationError maps a SQL generation failure onto the API contract.
func (s *Server) generationError(err error) *APIError {
	if schemaErr, ok := errors.AsType[*schema.SchemaError](err); ok {
		return apiErrorFromSchema(schemaErr)
	}
	if columnErr, ok := errors.AsType[*sqlgen.ColumnError](err); ok {
		code := pgrst.ColumnNotFound
		if strings.Contains(columnErr.Message, "ON CONFLICT") {
			code = pgrst.ParseError
		}
		return &APIError{Status: http.StatusBadRequest, Code: code, Message: columnErr.Message}
	}
	if fieldErr, ok := errors.AsType[*sqlgen.FieldError](err); ok {
		return &APIError{Status: http.StatusBadRequest, Code: pgrst.ParseError, Message: fieldErr.Message}
	}
	if unsupported, ok := errors.AsType[*sqlgen.UnsupportedError](err); ok {
		return &APIError{Status: http.StatusBadRequest, Code: pgrst.NotImplemented, Message: unsupported.Error()}
	}
	return &APIError{Status: http.StatusInternalServerError, Code: pgrst.Internal, Message: err.Error()}
}

// errMaxAffected reports that a request affected more rows than the max-affected
// preference allows. The code differs by endpoint (PGRST124 writes, PGRST128 RPC).
func errMaxAffected(code string, returned int64) *APIError {
	return &APIError{
		Status:  http.StatusBadRequest,
		Code:    code,
		Message: "Query result exceeds max-affected preference constraint",
		Details: fmt.Sprintf("The query affects %d rows", returned),
	}
}

// errNotSingular reports that an object was requested but the result did not
// contain exactly one row.
func errNotSingular(returned int64) *APIError {
	return &APIError{
		Status:  http.StatusNotAcceptable,
		Code:    pgrst.NotSingular,
		Message: "JSON object requested, multiple (or no) rows returned",
		Details: fmt.Sprintf("The result contains %d rows", returned),
	}
}

// serviceError maps an error raised inside the request transaction onto the API
// contract: an explicit *APIError passes through, anything else is a database
// error.
func serviceError(r *http.Request, err error) *APIError {
	if apiErr, ok := errors.AsType[*APIError](err); ok {
		return apiErr
	}
	return apiErrorFromDB(r, err)
}

// apiErrorFromDB converts a database error into the API contract.
func apiErrorFromDB(r *http.Request, err error) *APIError {
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok {
		return &APIError{
			Status:  pgStatus(pgErr.Code, r),
			Code:    pgErr.Code,
			Message: pgErr.Message,
			Details: nullableString(pgErr.Detail),
			Hint:    nullableString(pgErr.Hint),
		}
	}
	return &APIError{Status: http.StatusInternalServerError, Code: pgrst.Internal, Message: err.Error()}
}

// writeQueryError maps a SQL generation failure onto the error contract.
func (s *Server) writeQueryError(w http.ResponseWriter, r *http.Request, err error) {
	s.writeError(w, r, s.generationError(err))
}

// writeDBError translates a database error into PostgREST's error contract.
func (s *Server) writeDBError(w http.ResponseWriter, r *http.Request, err error) {
	s.writeError(w, r, apiErrorFromDB(r, err))
}

func errRelationNotFound(schemaName, relation string, cacheLoaded bool) *APIError {
	e := &APIError{
		Status:  http.StatusNotFound,
		Code:    pgrst.RelationNotFound,
		Message: "Could not find the table '" + schemaName + "." + relation + "' in the schema cache",
	}
	if !cacheLoaded {
		e.Details = "The schema cache has not been loaded yet"
	}
	return e
}

// apiErrorFromSchema maps a schema resolution failure onto the HTTP contract.
func apiErrorFromSchema(err *schema.SchemaError) *APIError {
	status := http.StatusBadRequest
	switch err.Code {
	case pgrst.RelationNotFound, pgrst.FunctionNotFound:
		status = http.StatusNotFound
	case pgrst.AmbiguousEmbed, pgrst.AmbiguousFunction:
		status = http.StatusMultipleChoices // 300
	}
	return &APIError{
		Status:  status,
		Code:    err.Code,
		Message: err.Message,
		Details: err.Details,
		Hint:    err.Hint,
	}
}

// wwwAuthenticate describes why a request was refused, as PostgREST does.
func wwwAuthenticate(code string) string {
	switch code {
	case pgrst.InvalidToken, pgrst.InvalidClaims:
		return `Bearer error="invalid_token"`
	default:
		return "Bearer"
	}
}

// writeError renders err using the configured verbosity and sets the
// headers PostgREST sets for error responses.
func (s *Server) writeError(w http.ResponseWriter, r *http.Request, err *APIError) {
	if rl := requestLogFrom(r.Context()); rl != nil {
		rl.errorCode = err.Code
	}
	status := err.Status
	if status == 0 {
		status = http.StatusInternalServerError
	}
	body := any(struct {
		Code    string `json:"code"`
		Details any    `json:"details"`
		Hint    any    `json:"hint"`
		Message string `json:"message"`
	}{err.Code, err.Details, err.Hint, err.Message})
	if s.cfg.ClientErrorVerbosity == config.ErrorMinimal {
		body = struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}{err.Code, err.Message}
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Proxy-Status", "PostgREST; error="+err.Code)
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", wwwAuthenticate(err.Code))
	}
	payload, marshalErr := json.Marshal(body)
	if marshalErr != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(status)
	_, _ = w.Write(payload)
}
