package handler

import (
	"strconv"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/hotelharmony/api/internal/cache"
	"github.com/hotelharmony/api/pkg/response"
)

// Per-client, plan-aware rate limiter (the "Rate Limiter + plan quotas
// (Redis-backed)" box in the architecture diagram). The global in-memory Fiber
// limiter in main.go caps per-IP request rate as a DoS guard; this one is the
// SaaS quota layer — it counts requests per CLIENT (hotel) in a fixed 1-minute
// window in Redis and enforces a ceiling that scales with the client's plan.
//
// Mounted after authGate (needs hotel_id) and after the rate counter is cheap,
// before the heavier handlers. Fail-open: a Redis error never blocks traffic.

// planMinuteQuota is the per-client request ceiling per minute, by plan rank
// (0=basic, 1=pro, 2=premium). Premium is effectively uncapped for normal use
// but still bounded to absorb runaway clients.
var planMinuteQuota = map[int]int64{
	0: 300,  // basic
	1: 1200, // pro
	2: 6000, // premium
}

const rateWindow = time.Minute

// clientRateLimit returns middleware that enforces planMinuteQuota per client.
// It reuses the plan gate's cached per-tenant state so plan resolution adds no
// extra DB round-trip on the hot path.
func clientRateLimit(c cache.Cache, gate *planGateState) fiber.Handler {
	return func(ctx *fiber.Ctx) error {
		// Platform admins are not metered.
		if claims, err := jwtClaimsFromRequest(ctx, gate.secret); err == nil {
			if pa, _ := claims["platform_admin"].(bool); pa {
				return ctx.Next()
			}
		}

		hotelID := tenantHotelID(ctx)
		limit := planMinuteQuota[0]
		if conf, ok := gate.confFor(ctx.Context(), hotelID); ok {
			if q, exists := planMinuteQuota[conf.rank]; exists {
				limit = q
			}
		}

		// Fixed-window bucket key: t:<hotelID>:ratelimit:<unix-minute>.
		bucket := strconv.FormatInt(time.Now().Unix()/60, 10)
		key := cache.TenantKey(hotelID.String(), "ratelimit:"+bucket)

		count, err := c.IncrementWithTTL(ctx.Context(), key, rateWindow)
		if err != nil {
			// Fail open on cache errors.
			return ctx.Next()
		}

		remaining := limit - count
		if remaining < 0 {
			remaining = 0
		}
		ctx.Set("X-RateLimit-Limit", strconv.FormatInt(limit, 10))
		ctx.Set("X-RateLimit-Remaining", strconv.FormatInt(remaining, 10))

		if count > limit {
			ctx.Set("Retry-After", "60")
			return response.Error(ctx, fiber.StatusTooManyRequests,
				"client request quota exceeded for this minute; please slow down or upgrade your plan")
		}
		return ctx.Next()
	}
}
