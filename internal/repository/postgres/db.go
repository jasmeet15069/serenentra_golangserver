// Package postgres provides the pgx/v5 connection pool and shared helpers
// used by every repository. We use pgx directly (no ORM) for maximum
// performance: prepared statements are cached automatically by pgx, and
// pgxpool manages the connection lifecycle with zero manual intervention.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"github.com/hotelharmony/api/internal/config"
)

// DB wraps pgxpool.Pool and exposes transaction helpers.
type DB struct {
	Pool   *pgxpool.Pool
	logger *zap.Logger
}

// Querier is the subset of pgx both *pgxpool.Pool and pgx.Tx implement. Every
// repository method executes through it rather than against a pool directly,
// which is what lets an existing method run inside a caller's transaction
// without any change to its SQL: WithTenantTx puts a pgx.Tx in the context and
// poolFromContext hands it back in place of the pool.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// txContextKey marks a transaction stashed in the context by WithTenantTx.
// A distinct unexported type, so it cannot collide with the string keys Fiber
// uses for Locals.
type txContextKey struct{}

// poolContextKey carries a tenant pool on a context that is no longer a
// request. See WithTenantPool.
type poolContextKey struct{}

// WithTenantPool pins a tenant's pool onto a context.
//
// Background work outlives the request it came from, and the "tenant_pool"
// value the middleware sets lives on the Fiber request context — so a job
// handed to the worker pool loses it and silently falls back to the shared
// database. For a tenant with a dedicated database that means the work lands in
// the wrong one.
//
// Resolve the pool while still on the request, pin it here, and the repository
// methods keep reaching the right database once the request is gone.
func WithTenantPool(ctx context.Context, p *pgxpool.Pool) context.Context {
	if p == nil {
		return ctx
	}
	return context.WithValue(ctx, poolContextKey{}, p)
}

// poolFromContext returns the executor a repository should use, in precedence
// order:
//
//  1. a transaction opened by WithTenantTx, so calls made inside it enlist
//     rather than committing independently on a separate connection;
//  2. the tenant-scoped pool the request middleware stashed under "tenant_pool"
//     (Fiber Locals -> fasthttp user value, readable here via ctx.Value because
//     handlers pass the *fasthttp.RequestCtx);
//  3. the repository's own (shared) pool.
//
// Step 2 makes the shared-pool repositories operate on a dedicated tenant's OWN
// database when one exists — matching the compat/bulk paths. Without it, repo
// reads/writes always hit the shared DB, so a dedicated tenant's
// rooms/reservations created via bulk/compat were invisible to /api/rooms and
// reservation lookups failed. Non-request contexts (background jobs) carry
// neither value and correctly fall back to the shared pool.
func poolFromContext(ctx context.Context, fallback *pgxpool.Pool) Querier {
	if tx, ok := ctx.Value(txContextKey{}).(pgx.Tx); ok && tx != nil {
		return tx
	}
	if p, ok := ctx.Value("tenant_pool").(*pgxpool.Pool); ok && p != nil {
		return p
	}
	// Pinned by WithTenantPool for work that has outlived its request.
	if p, ok := ctx.Value(poolContextKey{}).(*pgxpool.Pool); ok && p != nil {
		return p
	}
	return fallback
}

// PoolForContext exposes the tenant-resolution rule above to callers outside
// this package (handlers that hold a raw pool), without exposing the
// transaction: a handler that wants one should use WithTenantTx.
func PoolForContext(ctx context.Context, fallback *pgxpool.Pool) *pgxpool.Pool {
	if p, ok := ctx.Value("tenant_pool").(*pgxpool.Pool); ok && p != nil {
		return p
	}
	if p, ok := ctx.Value(poolContextKey{}).(*pgxpool.Pool); ok && p != nil {
		return p
	}
	return fallback
}

// Querier returns the executor for ctx — the caller's transaction when
// WithTenantTx opened one, otherwise the tenant pool. Handlers use this to run
// helper functions (accounting postings, promo redemption) inside the same
// transaction as the repository calls around them.
func (d *DB) Querier(ctx context.Context) Querier {
	return poolFromContext(ctx, d.Pool)
}

// New opens and validates a pgxpool connection.
func New(ctx context.Context, cfg *config.Config, log *zap.Logger) (*DB, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.Database.DSN)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse config: %w", err)
	}

	poolCfg.MaxConns = int32(cfg.Database.MaxOpenConns)
	poolCfg.MinConns = int32(cfg.Database.MaxIdleConns)
	poolCfg.MaxConnLifetime = cfg.Database.ConnMaxLifetime
	poolCfg.MaxConnIdleTime = cfg.Database.ConnMaxIdleTime
	poolCfg.HealthCheckPeriod = 30 * time.Second
	poolCfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeCacheStatement

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("postgres: create pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		return nil, fmt.Errorf("postgres: ping failed: %w", err)
	}

	log.Info("postgres: connected",
		zap.String("dsn_host", poolCfg.ConnConfig.Host),
		zap.Int32("max_conns", poolCfg.MaxConns),
	)

	return &DB{Pool: pool, logger: log}, nil
}

// NewForDatabase opens a pool against a specific database on the same server as
// baseDSN (the database name is swapped on the parsed config). Used by the
// tenant Manager to reach a dedicated per-tenant database for provisioning and
// (later) live routing.
func NewForDatabase(ctx context.Context, baseDSN, dbName string, log *zap.Logger) (*DB, error) {
	poolCfg, err := pgxpool.ParseConfig(baseDSN)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse config: %w", err)
	}
	poolCfg.ConnConfig.Database = dbName
	poolCfg.MaxConns = 8
	poolCfg.MinConns = 0
	poolCfg.MaxConnLifetime = 30 * time.Minute
	poolCfg.HealthCheckPeriod = 30 * time.Second
	poolCfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeCacheStatement

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("postgres: create pool for %s: %w", dbName, err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: ping %s failed: %w", dbName, err)
	}
	log.Info("postgres: tenant pool connected", zap.String("database", dbName))
	return &DB{Pool: pool, logger: log}, nil
}

// Close gracefully shuts down the connection pool.
func (d *DB) Close() {
	d.Pool.Close()
	d.logger.Info("postgres: pool closed")
}

// WithTx runs fn inside a database transaction, rolling back on any error
// and committing on success. Nested calls are not supported.
//
// The transaction is opened on the pool the context resolves to, not
// unconditionally on the shared pool. For a tenant with a dedicated database
// those are different pools, and beginning on the shared one would commit the
// transaction's writes to the wrong database while the rest of the request read
// and wrote the tenant's own.
func (d *DB) WithTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := PoolForContext(ctx, d.Pool).BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.ReadCommitted,
		AccessMode: pgx.ReadWrite,
	})
	if err != nil {
		return fmt.Errorf("postgres: begin tx: %w", err)
	}

	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback(ctx)
			panic(p)
		}
	}()

	if err := fn(tx); err != nil {
		if rbErr := tx.Rollback(ctx); rbErr != nil {
			d.logger.Error("postgres: rollback failed", zap.Error(rbErr))
		}
		return err
	}

	return tx.Commit(ctx)
}

// WithTenantTx runs fn inside a transaction on the context's tenant pool,
// handing fn a context that carries the transaction. Any repository method
// called with that context executes inside the transaction instead of on its
// own connection, so a multi-step write either lands whole or not at all
// without duplicating a single line of the SQL those methods already hold.
//
// This is what makes reservation creation atomic: the guest record, the stay,
// the folio, the payment and the ledger posting are one unit, where previously
// each was a separate statement whose error the caller discarded.
//
// Nested calls are not supported — fn is already inside a transaction, so
// calling WithTenantTx again with the derived context would open a second,
// independent one on another connection and quietly defeat the atomicity. The
// guard below returns an error rather than allowing that.
func (d *DB) WithTenantTx(ctx context.Context, fn func(context.Context) error) error {
	if tx, ok := ctx.Value(txContextKey{}).(pgx.Tx); ok && tx != nil {
		return fmt.Errorf("postgres: WithTenantTx called inside an existing transaction")
	}

	tx, err := PoolForContext(ctx, d.Pool).BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.ReadCommitted,
		AccessMode: pgx.ReadWrite,
	})
	if err != nil {
		return fmt.Errorf("postgres: begin tenant tx: %w", err)
	}

	txCtx := context.WithValue(ctx, txContextKey{}, tx)

	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback(ctx)
			panic(p)
		}
	}()

	if err := fn(txCtx); err != nil {
		if rbErr := tx.Rollback(ctx); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
			d.logger.Error("postgres: rollback failed", zap.Error(rbErr))
		}
		return err
	}

	return tx.Commit(ctx)
}

// Stats returns current pool statistics for the /health endpoint.
func (d *DB) Stats() map[string]interface{} {
	s := d.Pool.Stat()
	return map[string]interface{}{
		"acquired_conns":   s.AcquiredConns(),
		"idle_conns":       s.IdleConns(),
		"total_conns":      s.TotalConns(),
		"constructing":     s.ConstructingConns(),
		"max_conns":        s.MaxConns(),
		"new_conns_count":  s.NewConnsCount(),
		"acquire_count":    s.AcquireCount(),
		"acquire_duration": s.AcquireDuration().String(),
		"canceled_acquire": s.CanceledAcquireCount(),
		"empty_acquire":    s.EmptyAcquireCount(),
	}
}
