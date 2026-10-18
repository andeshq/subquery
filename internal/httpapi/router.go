// Package httpapi exposes the PostgREST-shaped HTTP surface.
package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/andeshq/subquery/internal/auth"
	"github.com/andeshq/subquery/internal/config"
	"github.com/andeshq/subquery/internal/observe"
	"github.com/andeshq/subquery/internal/schema"
)

// Principal is the authenticated identity for one request.
type Principal struct {
	Role   string
	Claims []byte
}

type principalKey struct{}

// PrincipalFrom returns the principal attached by authenticate.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

// snapshotter supplies the current schema snapshot.
type snapshotter interface {
	Snapshot() *schema.Snapshot
}

// tokenVerifier authenticates a bearer token.
type tokenVerifier interface {
	Verify(ctx context.Context, raw string) (*auth.Claims, error)
}

// dbPool is the subset of the connection pool the handlers use. *pgxpool.Pool
// satisfies it.
type dbPool interface {
	Ping(ctx context.Context) error
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Options are the dependencies of a Server.
type Options struct {
	Config   *config.Config
	Logger   *slog.Logger
	Pool     dbPool
	Verifier tokenVerifier
	Store    snapshotter
}

// Server is the HTTP handler and its dependencies.
type Server struct {
	cfg      *config.Config
	log      *slog.Logger
	pool     dbPool
	verifier tokenVerifier
	store    snapshotter

	handler http.Handler
}

// New wires the router. Route patterns are deliberately method-agnostic for
// relations and RPC because PostgREST derives Allow from database grants.
func New(opts Options) *Server {
	s := &Server{cfg: opts.Config, log: opts.Logger, pool: opts.Pool, verifier: opts.Verifier, store: opts.Store}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /_health", s.handleHealth)
	mux.HandleFunc("GET /_ready", s.handleReady)
	mux.HandleFunc("/{$}", s.handleRoot)
	mux.HandleFunc("/rpc/{fn}", s.handleRPC)
	mux.HandleFunc("/{table}", s.handleRelation)
	mux.HandleFunc("/", s.handleNotFound)

	s.handler = s.withLogging(s.withRecovery(mux))
	return s
}

// Handler returns the root handler, suitable for embedding or mounting.
func (s *Server) Handler() http.Handler { return s.handler }

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	// Ready means the pool answers and a schema snapshot has been loaded, so a
	// load balancer does not route traffic to an instance that cannot serve a
	// request yet.
	if err := s.pool.Ping(ctx); err != nil || s.store.Snapshot() == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *Server) handleRelation(w http.ResponseWriter, r *http.Request) {
	r, principal, apiErr := s.authenticate(r)
	if apiErr != nil {
		s.writeError(w, r, apiErr)
		return
	}

	name := r.PathValue("table")
	snap := s.store.Snapshot()
	if snap == nil {
		s.writeError(w, r, errRelationNotFound(s.cfg.DBSchemas[0], name, false))
		return
	}
	rel, ok := snap.Relation(name)
	if !ok {
		s.writeError(w, r, errRelationNotFound(s.cfg.DBSchemas[0], name, true))
		return
	}

	switch r.Method {
	case http.MethodOptions:
		allow, err := s.relationAllow(r.Context(), rel, principal.Role)
		if err != nil {
			s.writeDBError(w, r, err)
			return
		}
		w.Header().Set("Allow", allow)
		w.WriteHeader(http.StatusOK)
	case http.MethodGet, http.MethodHead:
		s.readRelation(w, r, snap, rel)
	case http.MethodPost, http.MethodPatch, http.MethodPut, http.MethodDelete:
		s.writeRelation(w, r, snap, rel)
	default:
		if allow, err := s.relationAllow(r.Context(), rel, principal.Role); err == nil {
			w.Header().Set("Allow", allow)
		}
		s.writeError(w, r, errMethodNotAllowed)
	}
}

func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	s.writeError(w, r, errInvalidPath)
}

// authenticate resolves the request role: verified JWT role claim first, then
// db-anon-role. It returns a copy of the request carrying the principal in its
// context, leaving the caller's request untouched.
func (s *Server) authenticate(r *http.Request) (*http.Request, Principal, *APIError) {
	token, hasToken := bearerToken(r.Header.Get("Authorization"))
	if !hasToken {
		if s.cfg.DBAnonRole == "" {
			return r, Principal{}, errAnonDisabled
		}
		p := Principal{Role: s.cfg.DBAnonRole}
		return withPrincipal(r, p), p, nil
	}

	claims, err := s.verifier.Verify(r.Context(), token)
	if err != nil {
		if errors.Is(err, auth.ErrSecretMissing) {
			return r, Principal{}, errSecretMissing
		}
		if _, ok := errors.AsType[*auth.ClaimsError](err); ok {
			return r, Principal{}, errInvalidClaims.WithDetails(err.Error())
		}
		return r, Principal{}, errInvalidToken.WithDetails(err.Error())
	}

	role := claims.Role
	if role == "" {
		role = s.cfg.DBAnonRole
	}
	if role == "" {
		return r, Principal{}, errAnonDisabled
	}
	p := Principal{Role: role, Claims: claims.Raw}
	return withPrincipal(r, p), p, nil
}

func withPrincipal(r *http.Request, p Principal) *http.Request {
	if rl := requestLogFrom(r.Context()); rl != nil {
		rl.role = p.Role
	}
	return r.WithContext(context.WithValue(r.Context(), principalKey{}, p))
}

func bearerToken(header string) (string, bool) {
	scheme, token, ok := strings.Cut(strings.TrimSpace(header), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	payload, err := json.Marshal(body)
	if err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(payload)
}

type statusRecorder struct {
	http.ResponseWriter
	log *requestLog
}

func (r *statusRecorder) WriteHeader(status int) {
	r.log.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.log.status == 0 {
		r.log.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.log.bytes += n
	return n, err
}

// written reports whether a response has started.
func (r *statusRecorder) written() bool { return r.log.status != 0 }

// requestLog is the request-scoped record the access log is built from.
// Middleware, handlers, and the response recorder fill it in as the request
// progresses, so the log can report a role, an error code, and a byte count that
// none of them could see alone.
type requestLog struct {
	status    int
	bytes     int
	role      string
	errorCode string
}

type requestLogKey struct{}

// requestLogFrom returns the record attached by withLogging, or nil when the
// handlers are driven without that middleware.
func requestLogFrom(ctx context.Context) *requestLog {
	log, _ := ctx.Value(requestLogKey{}).(*requestLog)
	return log
}

func (s *Server) withRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			recovered := recover()
			if recovered == nil {
				return
			}
			s.log.ErrorContext(r.Context(), "panic recovered",
				"panic", recovered, "method", r.Method, "path", r.URL.Path,
				"stack", string(debug.Stack()))
			// The handler may have started a response already; in that case the
			// status is on the wire and only the log records the panic.
			if rec, ok := w.(*statusRecorder); ok && rec.written() {
				return
			}
			s.writeError(w, r, errInternalPanic)
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rl := &requestLog{}
		r = r.WithContext(context.WithValue(r.Context(), requestLogKey{}, rl))
		next.ServeHTTP(&statusRecorder{ResponseWriter: w, log: rl}, r)

		if rl.status == 0 {
			rl.status = http.StatusOK
		}
		observe.LogRequest(r.Context(), s.log, observe.Request{
			Method:       r.Method,
			Path:         r.URL.Path,
			Status:       rl.status,
			Duration:     time.Since(start),
			BytesWritten: rl.bytes,
			RequestID:    s.requestID(r),
			Role:         rl.role,
			ErrorCode:    rl.errorCode,
		})
	})
}

// requestID returns the trace id for the request: the value of the configured
// trace header when the client sent one, otherwise a generated id.
func (s *Server) requestID(r *http.Request) string {
	if name := s.cfg.ServerTraceHeader; name != "" {
		if id := r.Header.Get(name); id != "" {
			return id
		}
	}
	return newRequestID()
}

// newRequestID returns a random 128-bit trace id as hex, or "" if the system
// entropy source fails (in which case the log simply omits the id).
func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(b[:])
}

// logQuery logs the generated SQL when log-query is enabled. Bound values are
// never logged: they carry JWTs and header JSON.
func (s *Server) logQuery(ctx context.Context, sql string) {
	if s.cfg.LogQuery {
		s.log.InfoContext(ctx, "query", "sql", sql)
	}
}
