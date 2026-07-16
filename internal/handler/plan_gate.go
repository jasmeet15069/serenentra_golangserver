package handler

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hotelharmony/api/pkg/response"
)

// Plan / Feature Gate — server-side enforcement of the plan tiers in the
// architecture diagram (BASIC / PRO / PREMIUM). Until now plan tiers and the
// per-tenant module registry were metadata only: the portal used them to hide
// nav, but the API itself happily served PRO/PREMIUM endpoints to a BASIC
// client. This middleware closes that gap. It runs AFTER authGate (so hotel_id
// is already resolved) and BEFORE the staff handlers in router.go.
//
// Two independent checks are applied per request:
//  1. Plan tier — the route's module must be included in the tenant's plan.
//  2. Module mask — the master-admin may additionally disable any module for a
//     tenant (hotels.modules), which overrides the plan default.
//
// Resolution is fail-open: a DB error or an unknown tenant allows the request,
// so a transient registry hiccup never takes down core operations. Platform
// admins always bypass.

// planRank orders the tiers so "client plan >= required plan" is a comparison.
func planRank(plan string) int {
	switch normalizePlanTier(plan) {
	case "premium":
		return 2
	case "pro":
		return 1
	default:
		return 0
	}
}

// featureRule maps a route group to the module it belongs to and the minimum
// plan rank required to use it. moduleKey aligns with moduleRegistry so the
// master-admin's per-tenant module mask is enforced on the same path.
type featureRule struct {
	moduleKey string
	minRank   int
}

// featureRules is matched by longest path-prefix (after the /api group prefix).
// Only PRO/PREMIUM groups carry a minRank > 0; everything else is BASIC and is
// listed purely so an explicit module mask is still enforced. Public groups
// (auth, hotels, payments, ai concierge, compat) are registered before the gate
// in router.go and never reach this table.
var featureRules = map[string]featureRule{
	// PRO (Basic +)
	"/revenue":     {"revenue", 1},
	"/channel":     {"channel_manager", 1},
	"/procurement": {"procurement", 1},
	"/night-audit": {"night_audit", 1},
	// POS is available on ALL plans (2026-07-16) — masking only, no plan rank.
	"/pos": {"pos", 0},
	// PREMIUM (Pro +)
	"/email":      {"", 2},
	"/sms":        {"", 2},
	"/accounting": {"accounting", 2},
	// BASIC groups — masking only.
	"/dashboard":    {"dashboard", 0},
	"/rooms":        {"front_desk", 0},
	"/reservations": {"reservations", 0},
	"/housekeeping": {"housekeeping", 0},
	"/crm":          {"crm", 0},
	"/billing":      {"billing", 0},
	"/booking":      {"booking_engine", 0},
	"/maintenance":  {"maintenance", 0},
}

// ruleForPath returns the feature rule governing a request path, matched by the
// longest registered prefix. The bool is false when no rule applies (allow).
func ruleForPath(path string) (featureRule, bool) {
	// Strip the /api group prefix the gate is mounted under.
	p := strings.TrimPrefix(path, "/api")
	best := ""
	for prefix := range featureRules {
		if (p == prefix || strings.HasPrefix(p, prefix+"/")) && len(prefix) > len(best) {
			best = prefix
		}
	}
	if best == "" {
		return featureRule{}, false
	}
	return featureRules[best], true
}

// planConf is the cached, per-tenant plan + module state used by the gate.
type planConf struct {
	rank     int
	plan     string          // normalized plan tier (basic|pro|premium)
	modules  map[string]bool // effective (default-on) per-tenant module mask
	planFeat map[string]bool // configurable plan->feature overrides for this plan
	exp      time.Time
}

// planGateState owns the gate's DB handle and a short-lived per-tenant cache so
// the hot path does not query hotels on every request.
type planGateState struct {
	pool   *pgxpool.Pool
	secret string

	mu   sync.Mutex
	conf map[uuid.UUID]planConf
}

const planConfTTL = 30 * time.Second

func newPlanGate(pool *pgxpool.Pool, secret string) *planGateState {
	return &planGateState{pool: pool, secret: secret, conf: map[uuid.UUID]planConf{}}
}

// confFor returns the tenant's cached plan rank + effective modules, loading
// from the database on a cache miss. On any load error it returns ok=false so
// the caller fails open.
func (g *planGateState) confFor(ctx context.Context, hotelID uuid.UUID) (planConf, bool) {
	g.mu.Lock()
	c, ok := g.conf[hotelID]
	g.mu.Unlock()
	if ok && time.Now().Before(c.exp) {
		return c, true
	}

	var plan string
	var modulesRaw []byte
	if err := g.pool.QueryRow(ctx,
		`SELECT plan_tier, COALESCE(modules, '{}'::jsonb) FROM hotels WHERE id = $1`, hotelID,
	).Scan(&plan, &modulesRaw); err != nil {
		return planConf{}, false
	}
	stored := map[string]bool{}
	_ = json.Unmarshal(modulesRaw, &stored)

	norm := normalizePlanTier(plan)
	c = planConf{
		rank:     planRank(plan),
		plan:     norm,
		modules:  effectiveModules(stored),
		planFeat: loadPlanFeatureOverrides(ctx, g.pool, norm),
		exp:      time.Now().Add(planConfTTL),
	}
	g.mu.Lock()
	g.conf[hotelID] = c
	g.mu.Unlock()
	return c, true
}

// invalidate drops a tenant's cached plan/module state so a plan change or
// module toggle takes effect immediately rather than after planConfTTL.
func (g *planGateState) invalidate(hotelID uuid.UUID) {
	g.mu.Lock()
	delete(g.conf, hotelID)
	g.mu.Unlock()
}

// handler is the Fiber middleware. Mount it on the api group AFTER authGate.
func (g *planGateState) handler() fiber.Handler {
	return func(c *fiber.Ctx) error {
		rule, governed := ruleForPath(c.Path())
		if !governed {
			return c.Next()
		}

		// Platform admins operate across tenants and plans.
		if claims, err := jwtClaimsFromRequest(c, g.secret); err == nil {
			if pa, _ := claims["platform_admin"].(bool); pa {
				return c.Next()
			}
			if rawRoles, ok := claims["roles"].([]interface{}); ok {
				for _, rr := range rawRoles {
					if role, _ := rr.(string); role == "platform_admin" || role == "super_admin" {
						return c.Next()
					}
				}
			}
		}

		hotelID := tenantHotelID(c)
		conf, ok := g.confFor(c.Context(), hotelID)
		if !ok {
			// Fail open: never break core operations on a registry lookup error.
			return c.Next()
		}

		// Plan inclusion. Module-backed routes consult the configurable plan ->
		// feature matrix (default = the module's tier vs the plan; override =
		// plan_features). Non-module premium routes (e.g. /email, /sms) keep the
		// plain rank gate. With no overrides this is identical to the prior behaviour.
		if rule.moduleKey == "" {
			if conf.rank < rule.minRank {
				return response.Error(c, fiber.StatusForbidden,
					"this feature requires a higher plan tier; please upgrade your subscription")
			}
		} else {
			included := planFeatureDefault(conf.plan, rule.moduleKey)
			if v, ok := conf.planFeat[rule.moduleKey]; ok {
				included = v
			}
			if !included {
				return response.Error(c, fiber.StatusForbidden,
					"this feature is not included in your plan; please upgrade your subscription")
			}
		}
		// Per-tenant module mask (master-admin override for a specific client).
		if rule.moduleKey != "" {
			if enabled, exists := conf.modules[rule.moduleKey]; exists && !enabled {
				return response.Error(c, fiber.StatusForbidden,
					"this module is disabled for your account")
			}
		}
		return c.Next()
	}
}
