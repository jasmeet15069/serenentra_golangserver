// Package cache provides a Redis-backed cache with typed methods, TTL strategy,
// and a simple distributed lock used for idempotency critical sections.
package cache

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/hotelharmony/api/internal/config"
)

// TTL constants used across the application.
// Centralised here so changes are always global.
const (
	TTLDashboardStats   = 30 * time.Second
	TTLRoomList         = 60 * time.Second
	TTLPOSOrders        = 15 * time.Second
	TTLMenuItems        = 5 * time.Minute
	TTLInventoryAlerts  = 2 * time.Minute
	TTLExchangeRate     = 10 * time.Minute
	TTLAIMenuSuggestion = 10 * time.Minute
	TTLSession          = 168 * time.Hour
	TTLRevokedToken     = 168 * time.Hour
	TTLLock             = 10 * time.Second
)

// Cache is the public interface consumed by services.
// All methods accept a context for cancellation propagation.
type Cache interface {
	Get(ctx context.Context, key string) (string, error)
	Set(ctx context.Context, key, value string, ttl time.Duration) error
	Delete(ctx context.Context, keys ...string) error
	Exists(ctx context.Context, key string) (bool, error)
	Increment(ctx context.Context, key string) (int64, error)
	IncrementWithTTL(ctx context.Context, key string, ttl time.Duration) (int64, error)
	SetNX(ctx context.Context, key, value string, ttl time.Duration) (bool, error)
	Publish(ctx context.Context, channel, message string) error
	Ping(ctx context.Context) error
	// Stats returns a small set of server metrics (used_memory_bytes,
	// connected_clients, uptime, hits/misses) parsed from Redis INFO. Returns an
	// empty map when unavailable (e.g. NoopCache). Used by the monitoring endpoint.
	Stats(ctx context.Context) map[string]string
	// BackupNamespace scans keys under prefix and returns a gzipped JSONL backup
	// (one {"key","ttl_ms","dump"} per line; dump is base64 of Redis DUMP, so it
	// is RESTORE-able). Returns the gzipped bytes and the key count.
	BackupNamespace(ctx context.Context, prefix string) ([]byte, int, error)
	// FlushNamespace deletes all keys whose name starts with prefix. Used to
	// wipe a tenant's Redis namespace when the tenant is permanently deleted.
	FlushNamespace(ctx context.Context, prefix string) (int, error)
	Close() error
}

type redisCache struct {
	client *redis.Client
	log    *zap.Logger
}

// New creates a connected Redis client and verifies connectivity.
func New(ctx context.Context, cfg *config.Config, log *zap.Logger) (Cache, error) {
	opt, err := redis.ParseURL(cfg.Redis.URL)
	if err != nil {
		opt = &redis.Options{
			Addr:     cfg.Redis.URL,
			Password: cfg.Redis.Password,
			DB:       cfg.Redis.DB,
		}
	}

	opt.DialTimeout = cfg.Redis.DialTimeout
	opt.ReadTimeout = cfg.Redis.ReadTimeout
	opt.WriteTimeout = cfg.Redis.WriteTimeout
	opt.PoolSize = cfg.Redis.PoolSize
	opt.MinIdleConns = cfg.Redis.MinIdleConns

	client := redis.NewClient(opt)

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		return nil, fmt.Errorf("cache: redis ping failed: %w", err)
	}

	log.Info("cache: redis connected", zap.String("addr", opt.Addr))
	return &redisCache{client: client, log: log}, nil
}

func (c *redisCache) Get(ctx context.Context, key string) (string, error) {
	return c.client.Get(ctx, key).Result()
}

func (c *redisCache) Set(ctx context.Context, key, value string, ttl time.Duration) error {
	return c.client.Set(ctx, key, value, ttl).Err()
}

func (c *redisCache) Delete(ctx context.Context, keys ...string) error {
	return c.client.Del(ctx, keys...).Err()
}

func (c *redisCache) Exists(ctx context.Context, key string) (bool, error) {
	n, err := c.client.Exists(ctx, key).Result()
	return n > 0, err
}

// Increment is used for rate limiting counters.
func (c *redisCache) Increment(ctx context.Context, key string) (int64, error) {
	return c.client.Incr(ctx, key).Result()
}

// incrWindowScript atomically increments a fixed-window counter and sets the
// window TTL ONLY when the key is first created (count == 1). Re-setting EXPIRE
// on every hit would slide the window forever and never reset the limit.
var incrWindowScript = redis.NewScript(`
local v = redis.call('INCR', KEYS[1])
if v == 1 then
  redis.call('PEXPIRE', KEYS[1], ARGV[1])
end
return v
`)

// IncrementWithTTL atomically increments a counter and sets its TTL on the first
// hit of a window. It powers the per-client, plan-aware rate limiter: the key is
// a fixed-window bucket whose TTL expires the window.
func (c *redisCache) IncrementWithTTL(ctx context.Context, key string, ttl time.Duration) (int64, error) {
	return incrWindowScript.Run(ctx, c.client, []string{key}, ttl.Milliseconds()).Int64()
}

// SetNX is the building block for distributed locks.
func (c *redisCache) SetNX(ctx context.Context, key, value string, ttl time.Duration) (bool, error) {
	return c.client.SetNX(ctx, key, value, ttl).Result()
}

func (c *redisCache) Publish(ctx context.Context, channel, message string) error {
	return c.client.Publish(ctx, channel, message).Err()
}

func (c *redisCache) Ping(ctx context.Context) error {
	return c.client.Ping(ctx).Err()
}

// Stats parses a few useful fields out of Redis INFO for the monitoring endpoint.
func (c *redisCache) Stats(ctx context.Context) map[string]string {
	out := map[string]string{}
	raw, err := c.client.Info(ctx, "server", "clients", "memory", "stats").Result()
	if err != nil {
		return out
	}
	want := map[string]bool{
		"used_memory":               true,
		"used_memory_human":         true,
		"connected_clients":         true,
		"uptime_in_seconds":         true,
		"keyspace_hits":             true,
		"keyspace_misses":           true,
		"instantaneous_ops_per_sec": true,
	}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		k, v, ok := strings.Cut(line, ":")
		if ok && want[k] {
			out[k] = v
		}
	}
	return out
}

// BackupNamespace dumps all keys under prefix as a gzipped JSONL stream.
func (c *redisCache) BackupNamespace(ctx context.Context, prefix string) ([]byte, int, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	enc := json.NewEncoder(gz)
	count := 0
	var cursor uint64
	for {
		keys, next, err := c.client.Scan(ctx, cursor, prefix+"*", 200).Result()
		if err != nil {
			return nil, 0, err
		}
		for _, k := range keys {
			dump, err := c.client.Dump(ctx, k).Result()
			if err != nil {
				continue // key may have expired between scan and dump
			}
			ttl, _ := c.client.PTTL(ctx, k).Result()
			ms := ttl.Milliseconds()
			if ms < 0 {
				ms = 0
			}
			_ = enc.Encode(map[string]interface{}{
				"key":    k,
				"ttl_ms": ms,
				"dump":   base64.StdEncoding.EncodeToString([]byte(dump)),
			})
			count++
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	if err := gz.Close(); err != nil {
		return nil, 0, err
	}
	return buf.Bytes(), count, nil
}

// FlushNamespace deletes all Redis keys that start with prefix.
func (c *redisCache) FlushNamespace(ctx context.Context, prefix string) (int, error) {
	count := 0
	var cursor uint64
	for {
		keys, next, err := c.client.Scan(ctx, cursor, prefix+"*", 200).Result()
		if err != nil {
			return count, err
		}
		if len(keys) > 0 {
			if err := c.client.Del(ctx, keys...).Err(); err != nil {
				return count, err
			}
			count += len(keys)
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	return count, nil
}

func (c *redisCache) Close() error {
	return c.client.Close()
}

// TenantKey namespaces a cache key under a tenant ("t:<hotelID>:<suffix>") so
// Redis data is isolated per client. ALL tenant-scoped cache data must go through
// this helper — it is the Redis half of per-tenant isolation. Global, non-tenant
// data (FX rates, the token deny-list) intentionally does not use it.
func TenantKey(hotelID, suffix string) string { return "t:" + hotelID + ":" + suffix }

// Key builders used by services and handlers.
// Dashboard + room keys are tenant-namespaced (Phase 4c) alongside their now
// hotel_id-scoped repositories, so cache, data and invalidation stay coherent.
// NOTE: KeyMenuItems/KeyInventoryAlerts are still global pending their repos.
func KeyDashboardStats(hotelID string) string   { return TenantKey(hotelID, "dashboard:stats") }
func KeyRoomList(hotelID, status string) string { return TenantKey(hotelID, "rooms:list:"+status) }
func KeyRoomByID(hotelID, id string) string     { return TenantKey(hotelID, "rooms:"+id) }
func KeyMenuItems() string                      { return "menu:items:all" }
func KeyPOSOrders(hotelID string) string        { return TenantKey(hotelID, "pos:orders") }
func KeyInventoryAlerts() string                { return "inventory:alerts" }
func KeyExchangeRate(b, t string) string        { return fmt.Sprintf("fx:%s:%s", b, t) }
func KeyAIMenu(h string) string                 { return TenantKey(h, "ai:menu") }
func KeyRevokedToken(t string) string {
	if len(t) > 32 {
		return "revoked:" + t[len(t)-32:]
	}
	return "revoked:" + t
}

// NoopCache satisfies the Cache interface without any network I/O.
// Use in unit tests to avoid a Redis dependency.
type NoopCache struct{}

func (NoopCache) Get(_ context.Context, _ string) (string, error)           { return "", redis.Nil }
func (NoopCache) Set(_ context.Context, _, _ string, _ time.Duration) error { return nil }
func (NoopCache) Delete(_ context.Context, _ ...string) error               { return nil }
func (NoopCache) Exists(_ context.Context, _ string) (bool, error)          { return false, nil }
func (NoopCache) Increment(_ context.Context, _ string) (int64, error)      { return 0, nil }
func (NoopCache) IncrementWithTTL(_ context.Context, _ string, _ time.Duration) (int64, error) {
	return 0, nil
}
func (NoopCache) SetNX(_ context.Context, _, _ string, _ time.Duration) (bool, error) {
	return true, nil
}
func (NoopCache) Publish(_ context.Context, _, _ string) error { return nil }
func (NoopCache) Ping(_ context.Context) error                 { return nil }
func (NoopCache) Stats(_ context.Context) map[string]string    { return map[string]string{} }
func (NoopCache) BackupNamespace(_ context.Context, _ string) ([]byte, int, error) {
	return nil, 0, nil
}
func (NoopCache) FlushNamespace(_ context.Context, _ string) (int, error) { return 0, nil }
func (NoopCache) Close() error                                             { return nil }
