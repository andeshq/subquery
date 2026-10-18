package schema

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// reloadDebounce coalesces bursts of NOTIFY events into a single reload.
const reloadDebounce = 100 * time.Millisecond

// Options configures a Store.
type Options struct {
	// Schemas is introspected: the exposed schemas plus db-extra-search-path.
	Schemas []string
	// Exposed is the subset that gets routes.
	Exposed []string
	// Channel is the LISTEN channel name (db-channel).
	Channel string
	// ChannelEnabled toggles LISTEN/NOTIFY reloading.
	ChannelEnabled bool
	// Timeout bounds a single introspection pass.
	Timeout time.Duration
}

// Store owns the current schema snapshot and refreshes it on demand.
type Store struct {
	pool    *pgxpool.Pool
	log     *slog.Logger
	opts    Options
	current atomic.Pointer[Snapshot]
	gen     atomic.Int64
	reload  chan struct{}

	cancel    context.CancelFunc
	wg        sync.WaitGroup
	closeOnce sync.Once
}

// NewStore returns a store that has not yet loaded a snapshot.
func NewStore(pool *pgxpool.Pool, log *slog.Logger, opts Options) *Store {
	if opts.Timeout <= 0 {
		opts.Timeout = 30 * time.Second
	}
	return &Store{
		pool:   pool,
		log:    log,
		opts:   opts,
		reload: make(chan struct{}, 1),
	}
}

// Snapshot returns the current snapshot, or nil before the first successful
// load.
func (s *Store) Snapshot() *Snapshot { return s.current.Load() }

// Generation returns the number of successful loads so far.
func (s *Store) Generation() int64 { return s.gen.Load() }

// Reload introspects the catalog and atomically replaces the snapshot. On
// failure the previous snapshot is kept.
func (s *Store) Reload(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, s.opts.Timeout)
	defer cancel()

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	cat, err := Load(ctx, tx, s.opts.Schemas)
	if err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	// Publish only after the introspection transaction commits, so no reader
	// can observe a snapshot derived from an aborted read.
	snap := Build(s.gen.Add(1), s.opts.Exposed, cat)
	s.current.Store(snap)
	return nil
}

// Start launches the reload watcher and, when enabled, the NOTIFY listener.
func (s *Store) Start(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	s.cancel = cancel

	s.wg.Go(func() { s.watchReloads(ctx) })

	if s.opts.ChannelEnabled && s.opts.Channel != "" {
		s.wg.Go(func() { s.listen(ctx) })
	}
}

// TriggerReload schedules a debounced reload. It never blocks.
func (s *Store) TriggerReload() {
	select {
	case s.reload <- struct{}{}:
	default:
	}
}

// Close stops background work and waits for it to finish.
func (s *Store) Close() {
	s.closeOnce.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
		s.wg.Wait()
	})
}

func (s *Store) watchReloads(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.reload:
			timer := time.NewTimer(reloadDebounce)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			if err := s.Reload(ctx); err != nil {
				s.log.ErrorContext(ctx, "schema cache reload failed", "error", err)
				continue
			}
			s.log.InfoContext(ctx, "schema cache reloaded", "generation", s.gen.Load())
		}
	}
}

func (s *Store) listen(ctx context.Context) {
	for {
		if err := s.listenOnce(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			s.log.WarnContext(ctx, "notification listener reconnecting", "error", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			continue
		}
		return
	}
}

func (s *Store) listenOnce(ctx context.Context) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "listen "+pgx.Identifier{s.opts.Channel}.Sanitize()); err != nil {
		return err
	}
	for {
		notification, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			return err
		}
		if strings.Contains(notification.Payload, "reload schema") ||
			strings.Contains(notification.Payload, "reload config") {
			s.TriggerReload()
		}
	}
}
