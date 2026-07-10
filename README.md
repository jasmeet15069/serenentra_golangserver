# Serenentra — Go API (Hotel Harmony)

Repository: `hms_golangserver`

The Go/Fiber API powering the Serenentra multi-tenant hotel management platform.
Runs on **Hetzner VPS (167.233.158.179)** as `docker-api-1` on port 8787.

## Run (local dev)

1. Install Go 1.22+ and PostgreSQL.
2. Copy `.env.example` to `.env` and fill secrets.
3. Create a database and run `migrations/001_init.sql`.
4. Start the server:

```powershell
.\scripts\run-dev.ps1
```

Or run directly:

```sh
go run ./cmd/server
```

Health check: `GET /health`
API root: `GET /api`

## Auth & tenant isolation

Stateless JWT (HS256, `JWT_ACCESS_SECRET`). Public routes (`/api/auth/*`,
hotel branding, payments, AI, compat, `POST /api/demo-request`) are registered in
`internal/handler/router.go` **before** `api.Use(authGate(secret))`; every
staff-only handler is registered **after** it. Fiber applies the gate only to routes
registered after the mount, so this ordering is load-bearing — mounting it earlier
would gate `sign-in`.

Each request's tenant comes from the JWT `hotel_id` claim via a `hotelID(c)`
helper that falls back to `DemoHotelID` when the claim is absent. Because of that
fallback, **every tenant-scoped query must bind `hotel_id`** (SELECT
`WHERE hotel_id = $1`, INSERT `h.hotelID(c)`, UPDATE/DELETE `AND hotel_id = $N`);
otherwise an ungated or unscoped endpoint serves the demo tenant to any caller.
The `/api/tables/:table` compat layer scopes via `scopeHotel`/`compatTenantScopedTables`.
Admin-only writes additionally require a role via `requireHotelAdmin`/`roleGate`.

## Optimization Notes

- Fiber server with recovery, request IDs, security headers, ETags, gzip compression, CORS, and rate limiting.
- PostgreSQL uses `pgxpool` with statement caching and tuned pool defaults.
- Dashboard stats run as one CTE query and are cached.
- Room list reads are cached and invalidated after room changes.
- Stripe booking/payment checkout has short idempotency locks.
- External API calls use timeouts and cached exchange rates.
- Groq AI calls use retries, circuit breaking, and local fallbacks.
- Migration includes indexes for high-traffic room, booking, payment, order, complaint, and inventory reads.

## Docker (local)

```sh
docker compose -f deployments/docker/docker-compose.yml up --build
```

## Production Deploy (Hetzner VPS)

**Primary server:** `root@167.233.158.179`

### Upload and rebuild API

```bash
# Single file
scp "C:/Users/ACXIOM/Desktop/claude/MHMS_final/golangserver/internal/path/file.go" \
    root@167.233.158.179:/opt/hms/mhms_final/golangserver/internal/path/

# Full resync (safer for multiple files)
rsync -av --exclude='.env' --exclude='vendor/' \
  "C:/Users/ACXIOM/Desktop/claude/MHMS_final/golangserver/" \
  root@167.233.158.179:/opt/hms/mhms_final/golangserver/

# Rebuild on VM
ssh root@167.233.158.179
cd /opt/hms/mhms_final/golangserver/deployments/docker
set -a && source ../../.env && set +a
docker compose -f docker-compose.prod.yml build api
docker compose -f docker-compose.prod.yml up -d api

# Verify
docker compose -f docker-compose.prod.yml logs api --tail 30
curl -s http://localhost:8787/health
```

### Quick commands on VM

```bash
ssh root@167.233.158.179

# Container status
docker ps

# Logs
docker logs docker-api-1 --tail 50
docker logs docker-portal-1 --tail 50
docker logs docker-superadmin-1 --tail 50

# Smoke tests
curl -sL https://superadminportal.serenentra.com/api | head -1
curl -s http://localhost:8787/health
```

## Required `.env` keys

```env
APP_ENV=production
POSTGRES_PASSWORD=<db_password>
DATABASE_URL=postgres://hotel:<db_password>@postgres:5432/hotel_harmony?sslmode=disable
REDIS_URL=redis://redis:6379/0
JWT_ACCESS_SECRET=<32+ chars>
JWT_REFRESH_SECRET=<32+ chars>
FRONTEND_URL=https://hmsadmin.jazverse.online
SMTP_PASSWORD="<app password>"    # quote if it contains spaces

# Automated tenant provisioning
CLOUDFLARE_API_TOKEN=<cf token>
CLOUDFLARE_ZONE_ID=<zone id for serenentra.com>
VERCEL_API_TOKEN=<vercel token>
VERCEL_TEAM_ID=<vercel team id>
TENANT_BASE_DOMAIN=serenentra.com
TENANT_API_URL=https://hmsadmin.serenentra.com
```

## Migrations

SQL migration files in `migrations/` are run automatically on boot via `EnsureAppSchema()`.
All migrations are idempotent (`IF [NOT] EXISTS`, `ON CONFLICT`). Tracked in `schema_migrations`.

Current migrations: `001_init.sql` through `019_provisioning.sql`.
New files are picked up automatically in sorted order on next container start.
# serenentra_golangserver
