package handler

import (
	"bufio"
	"context"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hotelharmony/api/internal/cache"
	"github.com/hotelharmony/api/internal/worker"
	"github.com/hotelharmony/api/pkg/response"
)

// processStart is captured at package init so the monitoring endpoint can report
// process uptime. Close enough to the real start for an ops dashboard.
var processStart = time.Now()

// MonitoringHandler powers the superadmin monitoring dashboard. It gathers a
// live, point-in-time snapshot of platform health — Go runtime, PostgreSQL
// (connection pool, DB size, active connections), Redis, and the async worker
// pool — in one call. Read-only and platform-admin gated.
type MonitoringHandler struct {
	pool    *pgxpool.Pool
	cache   cache.Cache
	secret  string
	version string
}

func NewMonitoringHandler(pool *pgxpool.Pool, c cache.Cache, secret string) *MonitoringHandler {
	return &MonitoringHandler{pool: pool, cache: c, secret: secret, version: "1.0.0"}
}

func (h *MonitoringHandler) Register(r fiber.Router) {
	r.Get("/platform/monitoring", h.Snapshot)
}

// requirePlatformAdmin mirrors the platform handler's gate so this endpoint is
// only reachable by platform/super admins.
func (h *MonitoringHandler) requirePlatformAdmin(c *fiber.Ctx) bool {
	claims, err := jwtClaimsFromRequest(c, h.secret)
	if err != nil {
		_ = response.Error(c, fiber.StatusUnauthorized, "authentication is required")
		return false
	}
	if pa, _ := claims["platform_admin"].(bool); pa {
		return true
	}
	if rawRoles, ok := claims["roles"].([]interface{}); ok {
		for _, rr := range rawRoles {
			if role, _ := rr.(string); role == "platform_admin" || role == "super_admin" {
				return true
			}
		}
	}
	_ = response.Error(c, fiber.StatusForbidden, "platform admin access required")
	return false
}

// Snapshot (GET /api/platform/monitoring) returns the live metrics snapshot.
// Each section degrades independently — a Redis or Postgres hiccup yields a
// "down" status for that section rather than failing the whole response.
func (h *MonitoringHandler) Snapshot(c *fiber.Ctx) error {
	if !h.requirePlatformAdmin(c) {
		return nil
	}
	ctx := c.Context()
	return response.OK(c, fiber.Map{
		"generated_at": time.Now().UTC(),
		"app":          h.appSnapshot(),
		"runtime":      runtimeSnapshot(),
		"system":       systemSnapshot(),
		"postgres":     h.postgresSnapshot(ctx),
		"redis":        h.redisSnapshot(ctx),
		"workers":      workerSnapshot(),
	})
}

func (h *MonitoringHandler) appSnapshot() fiber.Map {
	return fiber.Map{
		"version":        h.version,
		"uptime_seconds": int64(time.Since(processStart).Seconds()),
	}
}

func runtimeSnapshot() fiber.Map {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return fiber.Map{
		"go_version":   runtime.Version(),
		"goroutines":   runtime.NumGoroutine(),
		"num_cpu":      runtime.NumCPU(),
		"mem_alloc_mb": bytesToMB(m.Alloc),
		"mem_sys_mb":   bytesToMB(m.Sys),
		"heap_objects": m.HeapObjects,
		"gc_runs":      m.NumGC,
	}
}

// postgresSnapshot reports pool utilisation + DB size + active connections.
func (h *MonitoringHandler) postgresSnapshot(ctx context.Context) fiber.Map {
	out := fiber.Map{"status": "down"}
	if h.pool == nil {
		return out
	}
	// Connectivity + latency.
	start := time.Now()
	if err := h.pool.Ping(ctx); err != nil {
		return out
	}
	out["status"] = "up"
	out["ping_ms"] = time.Since(start).Milliseconds()

	// Pool utilisation (no I/O — in-process counters).
	s := h.pool.Stat()
	out["pool"] = fiber.Map{
		"total":          s.TotalConns(),
		"acquired":       s.AcquiredConns(),
		"idle":           s.IdleConns(),
		"max":            s.MaxConns(),
		"acquired_total": s.AcquireCount(),
	}

	// DB size + active connections + version (best-effort).
	var sizeBytes int64
	if err := h.pool.QueryRow(ctx, `SELECT pg_database_size(current_database())`).Scan(&sizeBytes); err == nil {
		out["db_size_mb"] = bytesToMB(uint64(sizeBytes))
	}
	var active int
	if err := h.pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_stat_activity WHERE datname = current_database()`).Scan(&active); err == nil {
		out["active_connections"] = active
	}
	var version string
	if err := h.pool.QueryRow(ctx, `SHOW server_version`).Scan(&version); err == nil {
		out["version"] = version
	}
	return out
}

// redisSnapshot reports reachability + ping latency + a few INFO fields.
func (h *MonitoringHandler) redisSnapshot(ctx context.Context) fiber.Map {
	out := fiber.Map{"status": "down"}
	if h.cache == nil {
		return out
	}
	start := time.Now()
	if err := h.cache.Ping(ctx); err != nil {
		return out
	}
	out["status"] = "up"
	out["ping_ms"] = time.Since(start).Milliseconds()

	info := h.cache.Stats(ctx)
	if len(info) == 0 {
		return out
	}
	if v, ok := info["used_memory"]; ok {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			out["used_memory_mb"] = bytesToMB(n)
		}
	}
	if v, ok := info["connected_clients"]; ok {
		out["connected_clients"] = atoiOr0(v)
	}
	if v, ok := info["uptime_in_seconds"]; ok {
		out["uptime_seconds"] = atoiOr0(v)
	}
	if v, ok := info["instantaneous_ops_per_sec"]; ok {
		out["ops_per_sec"] = atoiOr0(v)
	}
	hits := atoiOr0(info["keyspace_hits"])
	misses := atoiOr0(info["keyspace_misses"])
	out["keyspace_hits"] = hits
	out["keyspace_misses"] = misses
	if hits+misses > 0 {
		out["hit_rate_pct"] = int(float64(hits) / float64(hits+misses) * 100)
	}
	return out
}

// workerSnapshot reads the process-wide async worker pool counters.
func workerSnapshot() fiber.Map {
	if worker.Default == nil {
		return fiber.Map{"status": "down"}
	}
	s := worker.Default.Stats()
	return fiber.Map{
		"status":    "up",
		"submitted": s["submitted"],
		"completed": s["completed"],
		"failed":    s["failed"],
		"dropped":   s["dropped"],
		"queued":    s["queued"],
	}
}

// systemSnapshot reads OS-level memory and load stats from /proc on Linux.
// Returns safe zero values on non-Linux hosts so the endpoint never errors.
func systemSnapshot() fiber.Map {
	out := fiber.Map{}

	// /proc/meminfo — RAM stats.
	if f, err := os.Open("/proc/meminfo"); err == nil {
		defer f.Close()
		minfo := map[string]uint64{}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			parts := strings.Fields(sc.Text())
			if len(parts) >= 2 {
				key := strings.TrimSuffix(parts[0], ":")
				if v, err := strconv.ParseUint(parts[1], 10, 64); err == nil {
					minfo[key] = v // values are in kB
				}
			}
		}
		totalKB := minfo["MemTotal"]
		availKB := minfo["MemAvailable"]
		buffersKB := minfo["Buffers"]
		cachedKB := minfo["Cached"] + minfo["SReclaimable"] - minfo["Shmem"]
		usedKB := totalKB - availKB
		swapTotalKB := minfo["SwapTotal"]
		swapFreeKB := minfo["SwapFree"]

		totalMB := float64(totalKB) / 1024
		usedMB := float64(usedKB) / 1024
		availMB := float64(availKB) / 1024
		buffersMB := float64(buffersKB) / 1024
		cachedMB := float64(cachedKB) / 1024
		usedPct := 0.0
		if totalKB > 0 {
			usedPct = float64(usedKB) / float64(totalKB) * 100
		}
		out["mem_total_mb"] = totalMB
		out["mem_used_mb"] = usedMB
		out["mem_available_mb"] = availMB
		out["mem_buffers_mb"] = buffersMB
		out["mem_cached_mb"] = cachedMB
		out["mem_used_pct"] = usedPct
		out["swap_total_mb"] = float64(swapTotalKB) / 1024
		out["swap_used_mb"] = float64(swapTotalKB-swapFreeKB) / 1024
	}

	// /proc/loadavg — load averages (no two-sample CPU measurement needed).
	if raw, err := os.ReadFile("/proc/loadavg"); err == nil {
		parts := strings.Fields(string(raw))
		if len(parts) >= 3 {
			if v, err := strconv.ParseFloat(parts[0], 64); err == nil {
				out["load_1m"] = v
			}
			if v, err := strconv.ParseFloat(parts[1], 64); err == nil {
				out["load_5m"] = v
			}
			if v, err := strconv.ParseFloat(parts[2], 64); err == nil {
				out["load_15m"] = v
			}
		}
		// Running/total processes: "2/512" in parts[3].
		if len(parts) >= 4 {
			pp := strings.SplitN(parts[3], "/", 2)
			if len(pp) == 2 {
				if v, err := strconv.Atoi(pp[0]); err == nil {
					out["procs_running"] = v
				}
				if v, err := strconv.Atoi(pp[1]); err == nil {
					out["procs_total"] = v
				}
			}
		}
	}

	// /proc/uptime — system uptime (distinct from process uptime).
	if raw, err := os.ReadFile("/proc/uptime"); err == nil {
		parts := strings.Fields(string(raw))
		if len(parts) >= 1 {
			if v, err := strconv.ParseFloat(parts[0], 64); err == nil {
				out["uptime_seconds"] = int64(v)
			}
		}
	}

	return out
}

func bytesToMB(b uint64) float64 {
	return float64(b) / (1024 * 1024)
}

func atoiOr0(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}
