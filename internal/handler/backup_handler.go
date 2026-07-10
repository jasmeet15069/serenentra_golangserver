package handler

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/hotelharmony/api/pkg/response"
)

// backupDir is where dump artifacts are written. It is a host-mounted volume so
// backups survive container restarts (see docker-compose.prod.yml).
const backupDir = "/app/backups"

// Backup configuration superadmin endpoints (Phase 4 slice one). These persist a
// per-client backup policy. The dump/upload execution engine is a later slice;
// this is the configuration surface the Backups tab edits. Self-checked via
// requirePlatformAdmin.

// allowedBackupDestinations are the storage targets the UI offers. Stored as a
// plain string so adding a destination needs no migration.
var allowedBackupDestinations = map[string]bool{
	"local": true, "gdrive": true, "supabase": true, "s3": true, "r2": true,
	"azure": true, "dropbox": true, "mega": true, "b2": true, "ftp": true,
	"sftp": true, "nas": true, "minio": true,
}

type backupConfig struct {
	HotelID       string `json:"hotel_id"`
	Enabled       bool   `json:"enabled"`
	CronExpr      string `json:"cron_expr"`
	Destination   string `json:"destination"`
	RetentionDays int    `json:"retention_days"`
	Encrypt       bool   `json:"encrypt"`
	NotifyEmail   string `json:"notify_email"`
}

// PlatformTenantBackupConfig (GET /api/platform/tenants/:id/backup-config) returns
// a client's backup policy, falling back to disabled defaults when no row exists.
func (h *OperationsHandler) PlatformTenantBackupConfig(c *fiber.Ctx) error {
	if !h.requirePlatformAdmin(c) {
		return nil
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid tenant id")
	}
	cfg := backupConfig{
		HotelID: id.String(), Enabled: false, CronExpr: "0 3 * * *",
		Destination: "local", RetentionDays: 30, Encrypt: true,
	}
	var notify *string
	err = h.pool.QueryRow(c.Context(),
		`SELECT enabled, cron_expr, destination, retention_days, encrypt, notify_email
		   FROM backup_configurations WHERE hotel_id = $1`, id).
		Scan(&cfg.Enabled, &cfg.CronExpr, &cfg.Destination, &cfg.RetentionDays, &cfg.Encrypt, &notify)
	if err == nil && notify != nil {
		cfg.NotifyEmail = *notify
	}
	return response.OK(c, fiber.Map{
		"config":       cfg,
		"destinations": backupDestinationList(),
	})
}

// UpdatePlatformTenantBackupConfig (PUT /api/platform/tenants/:id/backup-config)
// upserts a client's backup policy.
func (h *OperationsHandler) UpdatePlatformTenantBackupConfig(c *fiber.Ctx) error {
	if !h.requirePlatformAdmin(c) {
		return nil
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid tenant id")
	}
	var req backupConfig
	if err := c.BodyParser(&req); err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid request body")
	}

	dest := strings.ToLower(strings.TrimSpace(req.Destination))
	if !allowedBackupDestinations[dest] {
		return response.Error(c, fiber.StatusUnprocessableEntity, "unsupported destination")
	}
	cron := strings.TrimSpace(req.CronExpr)
	if cron == "" {
		cron = "0 3 * * *"
	}
	if req.RetentionDays < 1 {
		req.RetentionDays = 1
	}
	var notify interface{}
	if e := strings.TrimSpace(req.NotifyEmail); e != "" {
		notify = e
	}

	if _, err := h.pool.Exec(c.Context(),
		`INSERT INTO backup_configurations
		   (hotel_id, enabled, cron_expr, destination, retention_days, encrypt, notify_email, updated_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7, now())
		 ON CONFLICT (hotel_id) DO UPDATE SET
		   enabled = EXCLUDED.enabled, cron_expr = EXCLUDED.cron_expr,
		   destination = EXCLUDED.destination, retention_days = EXCLUDED.retention_days,
		   encrypt = EXCLUDED.encrypt, notify_email = EXCLUDED.notify_email, updated_at = now()`,
		id, req.Enabled, cron, dest, req.RetentionDays, req.Encrypt, notify); err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "failed to save backup config")
	}

	req.HotelID = id.String()
	req.CronExpr = cron
	req.Destination = dest
	go h.saveConfigSnapshot(id)
	return response.OK(c, fiber.Map{
		"config":       req,
		"destinations": backupDestinationList(),
	})
}

func backupDestinationList() []string {
	return []string{
		"local", "gdrive", "supabase", "s3", "r2", "azure",
		"dropbox", "mega", "b2", "ftp", "sftp", "nas", "minio",
	}
}

// resolveBackupDSN returns the connection string + database name to back up for a
// tenant. Dedicated tenants dump their own DB; shared tenants dump the shared
// application DB (which holds their rows). Built from the api's DATABASE_URL.
func (h *OperationsHandler) resolveBackupDSN(ctx context.Context, hotelID uuid.UUID) (string, string) {
	base := os.Getenv("DATABASE_URL")
	if base == "" {
		return "", ""
	}
	mode, dbName := "shared", ""
	_ = h.pool.QueryRow(ctx,
		`SELECT isolation_mode, COALESCE(db_name, '') FROM tenant_registry WHERE hotel_id = $1`, hotelID).
		Scan(&mode, &dbName)

	u, err := url.Parse(base)
	if err != nil {
		return base, strings.TrimPrefix(base, "/")
	}
	if mode == "dedicated" && dbName != "" {
		u.Path = "/" + dbName
		return u.String(), dbName
	}
	return base, strings.TrimPrefix(u.Path, "/")
}

// runPgDumpGzip streams pg_dump output through gzip into fpath and returns the
// compressed artifact size. The connection string is passed as pg_dump's dbname
// argument (never logged).
func runPgDumpGzip(ctx context.Context, dsn, fpath string) (int64, error) {
	f, err := os.Create(fpath)
	if err != nil {
		return 0, fmt.Errorf("create artifact: %w", err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)

	cmd := exec.CommandContext(ctx, "pg_dump", "--no-owner", "--no-privileges", dsn)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		gz.Close()
		return 0, err
	}
	if err := cmd.Start(); err != nil {
		gz.Close()
		return 0, err
	}
	if _, err := io.Copy(gz, stdout); err != nil {
		_ = cmd.Wait()
		gz.Close()
		return 0, err
	}
	if err := gz.Close(); err != nil {
		_ = cmd.Wait()
		return 0, err
	}
	if err := cmd.Wait(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return 0, fmt.Errorf("pg_dump: %s", msg)
	}
	info, err := os.Stat(fpath)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

// RunPlatformTenantBackup (POST /api/platform/tenants/:id/backup/run) performs a
// real pg_dump of the tenant's database over the live connection, gzips it to the
// backups volume, and records a backup_jobs row.
func (h *OperationsHandler) RunPlatformTenantBackup(c *fiber.Ctx) error {
	if !h.requirePlatformAdmin(c) {
		return nil
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid tenant id")
	}

	ctx, cancel := context.WithTimeout(c.Context(), 5*time.Minute)
	defer cancel()

	dsn, dbName := h.resolveBackupDSN(ctx, id)
	if dsn == "" {
		return response.Error(c, fiber.StatusServiceUnavailable, "backup not available: database connection not configured")
	}
	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "backup storage unavailable")
	}

	jobID := uuid.New()
	fname := fmt.Sprintf("%s-%s.sql.gz", id.String(), time.Now().UTC().Format("20060102-150405"))
	fpath := filepath.Join(backupDir, fname)

	_, _ = h.pool.Exec(ctx,
		`INSERT INTO backup_jobs (id, hotel_id, trigger, status, kind, db_name, file_path)
		 VALUES ($1, $2, 'manual', 'running', 'postgres', $3, $4)`, jobID, id, dbName, fpath)

	size, runErr := runPgDumpGzip(ctx, dsn, fpath)
	if runErr != nil {
		_ = os.Remove(fpath)
		_, _ = h.pool.Exec(ctx,
			`UPDATE backup_jobs SET status='failed', error=$2, finished_at=now() WHERE id=$1`,
			jobID, runErr.Error())
		return response.Error(c, fiber.StatusInternalServerError, "backup failed: "+runErr.Error())
	}
	_, _ = h.pool.Exec(ctx,
		`UPDATE backup_jobs SET status='success', bytes=$2, finished_at=now() WHERE id=$1`, jobID, size)

	return response.OK(c, fiber.Map{
		"id": jobID, "status": "success", "kind": "postgres", "db_name": dbName,
		"bytes": size, "file": fname,
	})
}

// RunPlatformTenantRedisBackup (POST /api/platform/tenants/:id/redis-backup/run)
// dumps the client's Redis namespace (t:<id>:*) to a gzipped JSONL artifact on the
// backups volume and records a backup_jobs row of kind 'redis'.
func (h *OperationsHandler) RunPlatformTenantRedisBackup(c *fiber.Ctx) error {
	if !h.requirePlatformAdmin(c) {
		return nil
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid tenant id")
	}
	if h.cache == nil {
		return response.Error(c, fiber.StatusServiceUnavailable, "redis backup not available: cache not configured")
	}
	ctx, cancel := context.WithTimeout(c.Context(), 5*time.Minute)
	defer cancel()
	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		return response.Error(c, fiber.StatusInternalServerError, "backup storage unavailable")
	}

	prefix := "t:" + id.String() + ":"
	jobID := uuid.New()
	fname := fmt.Sprintf("%s-redis-%s.jsonl.gz", id.String(), time.Now().UTC().Format("20060102-150405"))
	fpath := filepath.Join(backupDir, fname)
	_, _ = h.pool.Exec(ctx,
		`INSERT INTO backup_jobs (id, hotel_id, trigger, status, kind, db_name, file_path)
		 VALUES ($1, $2, 'manual', 'running', 'redis', $3, $4)`, jobID, id, prefix+"*", fpath)

	data, keys, runErr := h.cache.BackupNamespace(ctx, prefix)
	if runErr == nil {
		runErr = os.WriteFile(fpath, data, 0o644)
	}
	if runErr != nil {
		_ = os.Remove(fpath)
		_, _ = h.pool.Exec(ctx, `UPDATE backup_jobs SET status='failed', error=$2, finished_at=now() WHERE id=$1`, jobID, runErr.Error())
		return response.Error(c, fiber.StatusInternalServerError, "redis backup failed: "+runErr.Error())
	}
	_, _ = h.pool.Exec(ctx, `UPDATE backup_jobs SET status='success', bytes=$2, finished_at=now() WHERE id=$1`, jobID, int64(len(data)))
	return response.OK(c, fiber.Map{
		"id": jobID, "status": "success", "kind": "redis", "keys": keys,
		"bytes": len(data), "file": fname,
	})
}

// DownloadPlatformTenantBackup (GET /api/platform/tenants/:id/backup/:job/download)
// streams a backup artifact for download. The job must belong to the tenant.
func (h *OperationsHandler) DownloadPlatformTenantBackup(c *fiber.Ctx) error {
	if !h.requirePlatformAdmin(c) {
		return nil
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid tenant id")
	}
	jobID, err := uuid.Parse(c.Params("job"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid job id")
	}
	var fpath, status string
	if err := h.pool.QueryRow(c.Context(),
		`SELECT COALESCE(file_path,''), status FROM backup_jobs WHERE id = $1 AND hotel_id = $2`,
		jobID, id).Scan(&fpath, &status); err != nil {
		return response.Error(c, fiber.StatusNotFound, "backup not found")
	}
	if status != "success" || fpath == "" {
		return response.Error(c, fiber.StatusConflict, "backup artifact is not available")
	}
	if _, err := os.Stat(fpath); err != nil {
		return response.Error(c, fiber.StatusGone, "backup file no longer on disk")
	}
	return c.Download(fpath, filepath.Base(fpath))
}

type backupJob struct {
	ID         string  `json:"id"`
	Kind       string  `json:"kind"`
	Status     string  `json:"status"`
	Trigger    string  `json:"trigger"`
	DBName     string  `json:"db_name"`
	Bytes      int64   `json:"bytes"`
	Error      *string `json:"error"`
	StartedAt  string  `json:"started_at"`
	FinishedAt *string `json:"finished_at"`
}

// PlatformTenantBackupHistory (GET /api/platform/tenants/:id/backup/history)
// lists recent backup jobs for a client (postgres + redis).
func (h *OperationsHandler) PlatformTenantBackupHistory(c *fiber.Ctx) error {
	if !h.requirePlatformAdmin(c) {
		return nil
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid tenant id")
	}
	jobs := []backupJob{}
	rows, err := h.pool.Query(c.Context(),
		`SELECT id, COALESCE(kind,'postgres'), status, trigger, COALESCE(db_name,''), bytes, error,
		        to_char(started_at, 'YYYY-MM-DD HH24:MI'),
		        to_char(finished_at, 'YYYY-MM-DD HH24:MI')
		 FROM backup_jobs WHERE hotel_id = $1 ORDER BY started_at DESC LIMIT 20`, id)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var j backupJob
			if err := rows.Scan(&j.ID, &j.Kind, &j.Status, &j.Trigger, &j.DBName, &j.Bytes, &j.Error, &j.StartedAt, &j.FinishedAt); err == nil {
				jobs = append(jobs, j)
			}
		}
	}
	return response.OK(c, fiber.Map{"jobs": jobs})
}

// PlatformTenantIsolation is an alias surfacing isolation + redis namespace for
// the client detail view. (Reuses the same data as TenantIsolation.)
func (h *OperationsHandler) PlatformTenantDetail(c *fiber.Ctx) error {
	if !h.requirePlatformAdmin(c) {
		return nil
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid tenant id")
	}

	var mode, redisNS string
	var dbName *string
	regErr := h.pool.QueryRow(c.Context(),
		`SELECT isolation_mode, db_name, redis_namespace FROM tenant_registry WHERE hotel_id = $1`, id).
		Scan(&mode, &dbName, &redisNS)
	if regErr != nil {
		mode, redisNS, dbName = "shared", id.String(), nil
	}

	// Connection descriptor (redacted -- never expose the password).
	host, port, user, sharedDB := connectionInfoFromEnv()
	effectiveDB := sharedDB
	if mode == "dedicated" && dbName != nil && *dbName != "" {
		effectiveDB = *dbName
	}

	return response.OK(c, fiber.Map{
		"hotel_id":        id.String(),
		"isolation_mode":  mode,
		"db_name":         effectiveDB,
		"redis_namespace": "t:" + id.String() + ":*",
		"connection": fiber.Map{
			"host":     host,
			"port":     port,
			"database": effectiveDB,
			"user":     user,
			"password": "********",
			"sslmode":  "disable",
			"scoped_by": func() string {
				if mode == "dedicated" {
					return "dedicated database"
				}
				return "hotel_id row-level (shared database)"
			}(),
		},
		"redis_ns_raw": redisNS,
	})
}

// connectionInfoFromEnv parses the api DATABASE_URL into display-safe parts.
func connectionInfoFromEnv() (host, port, user, db string) {
	host, port, user, db = "postgres", "5432", "hotel", "hotel_harmony"
	u, err := url.Parse(os.Getenv("DATABASE_URL"))
	if err != nil || u.Host == "" {
		return
	}
	if h := u.Hostname(); h != "" {
		host = h
	}
	if p := u.Port(); p != "" {
		port = p
	}
	if u.User != nil && u.User.Username() != "" {
		user = u.User.Username()
	}
	if d := strings.TrimPrefix(u.Path, "/"); d != "" {
		db = d
	}
	return
}
