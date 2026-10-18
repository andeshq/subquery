// Package subquery is the embeddable entry point: it owns the pool, the schema
// cache, and the HTTP handler.
package subquery

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/andeshq/subquery/internal/auth"
	"github.com/andeshq/subquery/internal/config"
	"github.com/andeshq/subquery/internal/httpapi"
	"github.com/andeshq/subquery/internal/observe"
	"github.com/andeshq/subquery/internal/pg"
	"github.com/andeshq/subquery/internal/schema"
)

// Config is the public alias for the resolved configuration.
type Config = config.Config

// DefaultConfig returns the documented defaults.
func DefaultConfig() *Config { return config.Default() }

// LoadConfig reads path (which may be empty) and applies PGRST_* variables.
func LoadConfig(path string) (*Config, error) { return config.Load(path) }

// Server owns the database pool, the schema snapshot, and the HTTP handler.
type Server struct {
	cfg     *Config
	log     *slog.Logger
	pool    *pgxpool.Pool
	store   *schema.Store
	handler http.Handler
}

// New builds a server. The caller must Close it.
func New(ctx context.Context, cfg *Config) (*Server, error) {
	if cfg == nil {
		return nil, errors.New("subquery: nil config")
	}
	log := observe.NewLogger(cfg.LogLevel)

	pool, err := pg.NewPool(ctx, cfg)
	if err != nil {
		return nil, err
	}
	verifier, err := auth.NewVerifier(cfg)
	if err != nil {
		pool.Close()
		return nil, err
	}

	schemas := dedupe(slices.Concat(cfg.DBSchemas, cfg.DBExtraSearchPath))
	store := schema.NewStore(pool, log, schema.Options{
		Schemas:        schemas,
		Exposed:        cfg.DBSchemas,
		Channel:        cfg.DBChannel,
		ChannelEnabled: cfg.DBChannelEnabled,
	})
	if err := store.Reload(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	store.Start(ctx)

	api := httpapi.New(httpapi.Options{
		Config:   cfg,
		Logger:   log,
		Pool:     pool,
		Verifier: verifier,
		Store:    store,
	})

	return &Server{
		cfg:     cfg,
		log:     log,
		pool:    pool,
		store:   store,
		handler: api.Handler(),
	}, nil
}

// Handler returns the root HTTP handler.
func (s *Server) Handler() http.Handler { return s.handler }

// Config returns the resolved configuration.
func (s *Server) Config() *Config { return s.cfg }

// Pool exposes the underlying pool for advanced embedding.
func (s *Server) Pool() *pgxpool.Pool { return s.pool }

// TriggerSchemaReload schedules a debounced schema cache refresh, as SIGUSR1
// and NOTIFY pgrst, 'reload schema' do.
func (s *Server) TriggerSchemaReload() { s.store.TriggerReload() }

// Close stops background work and releases the pool.
func (s *Server) Close() {
	s.store.Close()
	s.pool.Close()
}

// dedupe preserves order while removing repeated schema names.
func dedupe(names []string) []string {
	seen := make(map[string]struct{}, len(names))
	out := make([]string, 0, len(names))
	for _, name := range names {
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out
}
