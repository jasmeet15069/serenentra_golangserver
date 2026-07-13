// Package postgres provides the pgx/v5 connection pool and shared helpers
// used by every repository. We use pgx directly (no ORM) for maximum
// performance: prepared statements are cached automatically by pgx, and
// pgxpool manages the connection lifecycle with zero manual intervention.
package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"github.com/hotelharmony/api/internal/config"
)

// DB wraps pgxpool.Pool and exposes transaction helpers.
type DB struct {
	Pool   *pgxpool.Pool
	logger *zap.Logger
}

// poolFromContext returns the tenant-scoped pool the request middleware stashed
// under "tenant_pool" (Fiber Locals -> fasthttp user value, readable here via
// ctx.Value because handlers pass the *fasthttp.RequestCtx), falling back to the
// repository's own (shared) pool. This makes the shared-pool repositories operate
// on a dedicated tenant's OWN database when one exists — matching the compat/bulk
// paths. Without it, repo reads/writes always hit the shared DB, so a dedicated
// tenant's rooms/reservations created via bulk/compat were invisible to /api/rooms
// and reservation lookups failed. Non-request contexts (background jobs) carry no
// such value and correctly fall back to the shared pool.
func poolFromContext(ctx context.Context, fallback *pgxpool.Pool) *pgxpool.Pool {
	if p, ok := ctx.Value("tenant_pool").(*pgxpool.Pool); ok && p != nil {
		return p
	}
	return fallback
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
func (d *DB) WithTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := d.Pool.BeginTx(ctx, pgx.TxOptions{
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
