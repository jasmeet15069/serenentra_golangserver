package handler

import (
	"context"
	"sync"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hotelharmony/api/pkg/response"
)

// Feature-Matrix Gate (Phase 2) — per-client, per-role feature enforcement.
//
// The plan gate answers "does this client's PLAN include this module?". This
// gate answers the finer question "is this client's ROLE allowed this feature?".
// The same receptionist role can see Billing on one client and not another,
// fully independent of the plan tier. Plan sets the ceiling; this sets what each
// role actually sees.
//
// It runs AFTER authGate (hotel_id + roles available) and AFTER the plan gate,
// and BEFORE the staff handlers. Resolution is DEFAULT-ON and FAIL-OPEN: a
// missing permission row, an unknown tenant, or a DB error all allow the request
// so a registry hiccup never takes down core operations. Platform admins bypass.

// featureConf is the cached, per-tenant deny set: role -> feature_key -> true
// when that (role, feature) pair is explicitly disabled for the tenant. Only
// denials are stored; anything absent is allowed (default-on).
type featureConf struct {
	deny map[string]map[string]bool
	exp  time.Time
}

// featureGateState owns the gate's DB handle and a short-lived per-tenant cache
// so the hot path does not query the matrix on every request. Mirrors
// planGateState (same TTL, same fail-open contract).
type featureGateState struct {
	pool   *pgxpool.Pool
	secret string

	mu   sync.Mutex
	conf map[uuid.UUID]featureConf
}

func newFeatureGate(pool *pgxpool.Pool, secret string) *featureGateState {
	return &featureGateState{pool: pool, secret: secret, conf: map[uuid.UUID]featureConf{}}
}

// confFor returns the tenant's cached deny set, loading from the database on a
// cache miss. On any load error it returns ok=false so the caller fails open.
func (g *featureGateState) confFor(ctx context.Context, hotelID uuid.UUID) (featureConf, bool) {
	g.mu.Lock()
	c, ok := g.conf[hotelID]
	g.mu.Unlock()
	if ok && time.Now().Before(c.exp) {
		return c, true
	}

	rows, err := g.pool.Query(ctx,
		`SELECT role, feature_key FROM client_role_permissions WHERE hotel_id = $1 AND enabled = false`,
		hotelID,
	)
	if err != nil {
		return featureConf{}, false
	}
	defer rows.Close()

	deny := map[string]map[string]bool{}
	for rows.Next() {
		var role, feature string
		if err := rows.Scan(&role, &feature); err != nil {
			return featureConf{}, false
		}
		if deny[role] == nil {
			deny[role] = map[string]bool{}
		}
		deny[role][feature] = true
	}
	if rows.Err() != nil {
		return featureConf{}, false
	}

	c = featureConf{deny: deny, exp: time.Now().Add(planConfTTL)}
	g.mu.Lock()
	g.conf[hotelID] = c
	g.mu.Unlock()
	return c, true
}

// invalidate drops a tenant's cached deny set so a matrix edit takes effect
// immediately rather than after planConfTTL.
func (g *featureGateState) invalidate(hotelID uuid.UUID) {
	g.mu.Lock()
	delete(g.conf, hotelID)
	g.mu.Unlock()
}

// handler is the Fiber middleware. Mount it on the api group AFTER the plan gate.
func (g *featureGateState) handler() fiber.Handler {
	return func(c *fiber.Ctx) error {
		// Reuse the plan gate's path -> module mapping. Only module-backed routes
		// are matrix-governed; everything else (auth, platform, comms) passes.
		rule, governed := ruleForPath(c.Path())
		if !governed || rule.moduleKey == "" {
			return c.Next()
		}

		claims, err := jwtClaimsFromRequest(c, g.secret)
		if err != nil {
			// authGate already validated the token to reach here; if we cannot
			// re-read claims, fail open rather than block a legitimate request.
			return c.Next()
		}

		// Platform admins operate across tenants and roles.
		if pa, _ := claims["platform_admin"].(bool); pa {
			return c.Next()
		}
		roles := make([]string, 0, 4)
		if rawRoles, ok := claims["roles"].([]interface{}); ok {
			for _, rr := range rawRoles {
				role, _ := rr.(string)
				if role == "platform_admin" || role == "super_admin" {
					return c.Next()
				}
				if role != "" {
					roles = append(roles, role)
				}
			}
		}
		if len(roles) == 0 {
			// No role information — authGate already authenticated the caller, so
			// do not block on the matrix.
			return c.Next()
		}

		conf, ok := g.confFor(c.Context(), tenantHotelID(c))
		if !ok {
			return c.Next() // fail open
		}

		// Allow when ANY of the caller's roles is permitted (default-on). Deny
		// only when every role the caller holds is explicitly disabled for this
		// feature on this client.
		feature := rule.moduleKey
		for _, role := range roles {
			d := conf.deny[role]
			if d == nil || !d[feature] {
				return c.Next()
			}
		}
		return response.Error(c, fiber.StatusForbidden,
			"this feature is not enabled for your role")
	}
}
