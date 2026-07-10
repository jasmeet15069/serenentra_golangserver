// Package tenant resolves the database for a tenant and provisions dedicated
// per-tenant databases. Shared-mode tenants use the primary pool with row-level
// hotel_id scoping; dedicated-mode tenants get their own database (created +
// migrated by Provision, routed by PoolFor).
package tenant

import (
	"context"
	"fmt"
	"regexp"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"github.com/hotelharmony/api/internal/repository/postgres"
)

var validDBName = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// confTTL bounds how long a tenant's isolation config is cached in-process, so
// the per-request resolver does not hit the registry table on every call.
const confTTL = 30 * time.Second

type tenantConf struct {
	mode string
	db   string
	exp  time.Time
}

// Manager owns the shared pool and a lazily-built cache of dedicated tenant pools.
type Manager struct {
	shared  *postgres.DB
	baseDSN string
	log     *zap.Logger

	mu    sync.Mutex
	pools map[string]*postgres.DB  // dbName -> dedicated pool
	conf  map[uuid.UUID]tenantConf // hotelID -> cached isolation config
}

func NewManager(shared *postgres.DB, baseDSN string, log *zap.Logger) *Manager {
	return &Manager{
		shared:  shared,
		baseDSN: baseDSN,
		log:     log,
		pools:   map[string]*postgres.DB{},
		conf:    map[uuid.UUID]tenantConf{},
	}
}

// PoolForHotel resolves the pool a request for hotelID should use. It reads the
// tenant's isolation config from the registry (cached for confTTL) and returns
// the dedicated pool only when isolation_mode is 'dedicated'; shared/provisioned
// (and any lookup error) fall back to the shared pool. Never returns nil — it is
// safe to call from any handler and defaults to current behaviour.
func (m *Manager) PoolForHotel(ctx context.Context, hotelID uuid.UUID) *pgxpool.Pool {
	m.mu.Lock()
	c, ok := m.conf[hotelID]
	m.mu.Unlock()

	if !ok || time.Now().After(c.exp) {
		mode := "shared"
		var dbName *string
		if err := m.shared.Pool.QueryRow(ctx,
			`SELECT isolation_mode, db_name FROM tenant_registry WHERE hotel_id = $1`, hotelID,
		).Scan(&mode, &dbName); err != nil {
			mode = "shared"
		}
		c = tenantConf{mode: mode, exp: time.Now().Add(confTTL)}
		if dbName != nil {
			c.db = *dbName
		}
		m.mu.Lock()
		m.conf[hotelID] = c
		m.mu.Unlock()
	}

	pool, err := m.PoolFor(ctx, c.mode, c.db)
	if err != nil {
		m.log.Warn("tenant: dedicated pool resolve failed; using shared",
			zap.String("hotel_id", hotelID.String()), zap.Error(err))
		return m.shared.Pool
	}
	return pool
}

// PoolFor returns the pool a request should use. Shared-mode (or empty dbName)
// returns the primary pool — current behaviour, zero change. Dedicated-mode
// lazily opens and caches a pool to the tenant's own database. (Wired into live
// request routing in Phase 4c.)
func (m *Manager) PoolFor(ctx context.Context, mode, dbName string) (*pgxpool.Pool, error) {
	if mode != "dedicated" || dbName == "" {
		return m.shared.Pool, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if db, ok := m.pools[dbName]; ok {
		return db.Pool, nil
	}
	db, err := postgres.NewForDatabase(ctx, m.baseDSN, dbName, m.log)
	if err != nil {
		return nil, err
	}
	m.pools[dbName] = db
	return db.Pool, nil
}

// Provision creates the tenant database if absent and runs the full schema on
// it (idempotent). It does NOT route live traffic — the caller flips the
// registry to 'provisioned'; cutover to 'dedicated' happens in Phase 4c.
func (m *Manager) Provision(ctx context.Context, dbName string) error {
	if !validDBName.MatchString(dbName) {
		return fmt.Errorf("invalid database name %q", dbName)
	}

	// CREATE DATABASE cannot run in the extended/prepared protocol, so use a
	// dedicated simple-protocol admin connection on the primary server.
	connCfg, err := pgx.ParseConfig(m.baseDSN)
	if err != nil {
		return fmt.Errorf("tenant: parse dsn: %w", err)
	}
	connCfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	admin, err := pgx.ConnectConfig(ctx, connCfg)
	if err != nil {
		return fmt.Errorf("tenant: admin connect: %w", err)
	}
	defer admin.Close(ctx)

	var exists bool
	if err := admin.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname=$1)", dbName).Scan(&exists); err != nil {
		return fmt.Errorf("tenant: check db: %w", err)
	}
	if !exists {
		// dbName is validated against validDBName, so quoting is safe.
		if _, err := admin.Exec(ctx, `CREATE DATABASE "`+dbName+`"`); err != nil {
			return fmt.Errorf("tenant: create database: %w", err)
		}
		m.log.Info("tenant: database created", zap.String("database", dbName))
	}

	// Run the full schema (migrations + ensured tables) on the new database.
	tdb, err := postgres.NewForDatabase(ctx, m.baseDSN, dbName, m.log)
	if err != nil {
		return err
	}
	defer tdb.Close()
	if err := tdb.EnsureAppSchema(ctx); err != nil {
		return fmt.Errorf("tenant: migrate %s: %w", dbName, err)
	}
	m.log.Info("tenant: database migrated", zap.String("database", dbName))
	return nil
}

// Close releases all cached dedicated pools.
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, db := range m.pools {
		db.Close()
	}
}
