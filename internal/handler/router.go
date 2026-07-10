package handler

import (
	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hotelharmony/api/internal/cache"
	"github.com/hotelharmony/api/internal/tenant"
	"github.com/hotelharmony/api/pkg/response"
)

type Handlers struct {
	Health         *HealthHandler
	Auth           *AuthHandler
	Hotels         *HotelHandler
	Payments       *PaymentHandler
	Dashboard      *DashboardHandler
	Rooms          *RoomHandler
	Ops            *OperationsHandler
	AI             *AIHandler
	Compat         *CompatHandler
	Users          *UserHandler
	Reservations   *ReservationHandler
	Billing        *BillingHandler
	Housekeeping   *HousekeepingHandler
	Revenue        *RevenueHandler
	Procurement    *ProcurementHandler
	CRM            *CRMHandler
	Channel        *ChannelHandler
	NightAudit     *NightAuditHandler
	Booking        *BookingHandler
	Asset          *AssetHandler
	Communications *CommunicationsHandler
	POS            *POSHandler
	Bulk           *BulkHandler
	Monitoring     *MonitoringHandler
	Accounting     *AccountingHandler
	Demo           *DemoHandler
	Newsletter     *NewsletterHandler
}

func Register(app *fiber.App, h Handlers, secret string, pool *pgxpool.Pool, c cache.Cache, tenantMgr *tenant.Manager) {
	app.Get("/api", func(c *fiber.Ctx) error {
		return response.OK(c, map[string]interface{}{
			"name":    "Hotel Harmony Go API",
			"status":  "ok",
			"version": "1.0.0",
			"routes": []string{
				"GET /health",
				"GET /ready",
				"POST /api/auth/sign-in",
				"POST /api/auth/sign-up",
				"POST /api/auth/sign-out",
				"POST /api/auth/refresh",
				"PATCH /api/auth/user",
				"GET /api/hotel/branding",
				"PUT /api/hotel/branding",
				"POST /api/onboarding/hotel",
				"GET /api/payment-config",
				"GET /api/exchange-rate",
				"POST /api/bookings/checkout",
				"POST /api/bookings/hold",
				"POST /api/bookings/razorpay/order",
				"POST /api/payments/checkout",
				"POST /api/payments/complete",
				"POST /api/payments/razorpay/order",
				"POST /api/payments/razorpay/verify",
				"GET /api/dashboard/stats",
				"GET /api/dashboard/data",
				"GET /api/rooms",
				"POST /api/rooms",
				"PATCH /api/rooms/:id/status",
				"POST /api/housekeeping/guest-requests",
				"GET /api/housekeeping/tasks",
				"POST /api/housekeeping/tasks",
				"PATCH /api/housekeeping/tasks/:id",
				"GET /api/housekeeping/lost-items",
				"POST /api/housekeeping/lost-items",
				"PATCH /api/housekeeping/lost-items/:id",
				"GET /api/housekeeping/linen",
				"POST /api/housekeeping/linen/issue",
				"POST /api/housekeeping/linen/return",
				"GET /api/plan/limits",
				"GET /api/platform/plans",
				"GET /api/platform/plan-features",
				"PUT /api/platform/plan-features",
				"GET /api/platform/tenants",
				"POST /api/platform/tenants",
				"PUT /api/platform/tenants/:id/plan",
				"DELETE /api/platform/tenants/:id",
				"POST /api/platform/tenants/:id/impersonate",
				"POST /api/platform/tenants/:id/reset-admin-password",
				"POST /api/auth/impersonate/exchange",
				"GET /api/platform/tenants/:id/feature-matrix",
				"PUT /api/platform/tenants/:id/feature-matrix",
				"GET /api/platform/monitoring",
				"GET /api/platform/tenants/:id/backup-config",
				"PUT /api/platform/tenants/:id/backup-config",
				"GET /api/platform/tenants/:id/backup/bundle",
				"GET /api/platform/tenants/:id/config.json",
				"GET /api/platform/tenants/:id/config",
				"GET /api/platform/security",
				"GET /api/reports/occupancy",
				"GET /api/reports/revenue",
				"GET /api/reports/complaints",
				"GET /api/reports/bookings-pace",
				"GET /api/reports/staff-activity",
				"GET /api/reports/ai-usage",
				"GET /api/reports/consolidated",
				"POST /api/ai/chat",
				"POST /api/functions/ai-menu-suggestions",
				"POST /api/functions/ai-complaint-analysis",
				"POST /api/functions/voice-assistant-token",
				"GET /api/settings/payment",
				"PUT /api/settings/payment",
				"GET /api/settings/role-portals",
				"PUT /api/settings/role-portals",
				"GET /api/users",
				"GET /api/users/:id",
				"PATCH /api/users/:id",
				"POST /api/users/:id/roles",
				"DELETE /api/users/:id/roles/:role",
				"GET /api/reservations",
				"GET /api/reservations/calendar",
				"GET /api/reservations/:id",
				"POST /api/reservations",
				"PATCH /api/reservations/:id",
				"DELETE /api/reservations/:id",
				"POST /api/reservations/:id/checkin",
				"POST /api/reservations/:id/checkout",
				"GET /api/billing/folios",
				"GET /api/billing/folios/:id",
				"POST /api/billing/folios",
				"POST /api/billing/folios/:id/charges",
				"POST /api/billing/folios/:id/payments",
				"GET /api/billing/charges",
				"GET /api/billing/invoices",
				"POST /api/billing/invoices",
				"GET /api/billing/invoices/:id",
				"POST /api/billing/invoices/:id/email",
				"GET /api/billing/transactions",
				"GET /api/revenue/pricing",
				"GET /api/revenue/pricing-rules",
				"POST /api/revenue/pricing",
				"POST /api/revenue/pricing-rules",
				"PUT /api/revenue/pricing-rules/:id",
				"PATCH /api/revenue/pricing-rules/:id",
				"DELETE /api/revenue/pricing/:id",
				"DELETE /api/revenue/pricing-rules/:id",
				"GET /api/revenue/yield",
				"GET /api/revenue/competitors",
				"GET /api/revenue/forecast",
				"GET /api/procurement/vendors",
				"POST /api/procurement/vendors",
				"PATCH /api/procurement/vendors/:id",
				"GET /api/procurement/purchase-orders",
				"POST /api/procurement/purchase-orders",
				"PATCH /api/procurement/purchase-orders/:id/status",
				"PATCH /api/procurement/purchase-orders/:id",
				"GET /api/crm/guests",
				"POST /api/crm/guests",
				"GET /api/crm/guests/:id",
				"PATCH /api/crm/guests/:id",
				"GET /api/crm/loyalty/tiers",
				"POST /api/crm/loyalty/tiers",
				"PUT /api/crm/loyalty/tiers/:id",
				"PATCH /api/crm/loyalty/tiers/:id",
				"GET /api/crm/loyalty/members",
				"POST /api/crm/loyalty/points/award",
				"POST /api/crm/loyalty/points/redeem",
				"GET /api/channel/connections",
				"POST /api/channel/connections",
				"PATCH /api/channel/connections/:id",
				"DELETE /api/channel/connections/:id",
				"GET /api/channel/analytics",
				"GET /api/night-audit/checklist",
				"GET /api/night-audit/revenue-audit",
				"GET /api/night-audit/tax-audit",
				"POST /api/night-audit/close-day",
				"GET /api/night-audit/reports",
				"GET /api/booking/availability",
				"POST /api/booking/search",
				"GET /api/booking/promotions",
				"POST /api/booking/promotions",
				"PATCH /api/booking/promotions/:id",
				"DELETE /api/booking/promotions/:id",
				"POST /api/booking/reservations",
				"POST /api/booking/validate-promo",
				"GET /api/maintenance/assets",
				"POST /api/maintenance/assets",
				"PATCH /api/maintenance/assets/:id",
				"GET /api/maintenance/schedule",
				"POST /api/maintenance/schedule",
				"PATCH /api/maintenance/schedule/:id/complete",
				"POST /api/email/send",
				"POST /api/sms/send",
				"GET /api/tables/:table",
				"POST /api/tables/:table",
				"PATCH /api/tables/:table",
				"DELETE /api/tables/:table",
			},
		})
	})
	if h.Health != nil {
		h.Health.Register(app)
	}
	api := app.Group("/api")

	// Route registration order is security-significant. authGate is mounted as
	// group middleware (api.Use) which, in Fiber, only gates routes registered
	// AFTER it. So everything before the api.Use(authGate) line below is public,
	// and everything after is staff-only. Previously Dashboard/Rooms/Users/
	// Reservations were registered before the (then implicit) gate, so they fell
	// through to hotelID()'s demo-tenant fallback and served real tenant data
	// (guest PII, staff emails/roles, room status) to unauthenticated callers.

	// --- Public / self-authenticating handlers (must stay reachable without a
	// staff session: login, guest checkout, public hotel branding, AI concierge,
	// the platform/ops endpoints that enforce their own role checks). ---
	if h.Auth != nil {
		h.Auth.Register(api)
	}
	if h.Hotels != nil {
		h.Hotels.Register(api)
	}
	if h.Payments != nil {
		h.Payments.Register(api)
	}
	if h.AI != nil {
		h.AI.Register(api)
	}
	if h.Compat != nil {
		h.Compat.Register(api)
	}
	if h.Demo != nil {
		h.Demo.Register(api)
	}
	if h.Newsletter != nil {
		h.Newsletter.Register(api)
	}

	// --- Staff-only auth gate. Every handler registered below requires a valid
	// bearer token. ---
	api.Use(authGate(secret))

	// Resolve the tenant's DB pool right after authentication so every
	// downstream handler gets the correct pool (dedicated or shared) via
	// tenantPool(c, h.pool) without knowing about the tenant manager.
	if tenantMgr != nil {
		api.Use(func(c *fiber.Ctx) error {
			hotelID := tenantHotelID(c)
			c.Locals("tenant_pool", tenantMgr.PoolForHotel(c.Context(), hotelID))
			return c.Next()
		})
	}

	// --- Per-client rate limiter + Plan/Feature gate. Both run after authGate
	// (hotel_id resolved) and before the staff handlers. They share one
	// planGateState so plan resolution is cached and costs at most one DB lookup
	// per tenant per 30s. Rate limit runs first (cheap rejection), then the plan
	// gate blocks PRO/PREMIUM route groups for lower-plan clients and enforces
	// per-tenant module masks. Both fail open on backend errors. ---
	if pool != nil {
		gate := newPlanGate(pool, secret)
		if c != nil {
			api.Use(clientRateLimit(c, gate))
		}
		api.Use(gate.handler())

		// Feature-matrix gate runs after the plan gate: the plan decides whether
		// the client's tier includes a module, then this decides whether the
		// caller's ROLE is allowed that feature on this client. Default-on and
		// fail-open, so it never blocks until the superadmin turns something off.
		fgate := newFeatureGate(pool, secret)
		api.Use(fgate.handler())
		if h.Ops != nil {
			h.Ops.featureGate = fgate
		}
		// The compat data layer (/api/tables/*) is registered before these
		// middlewares (it self-authenticates), so it would otherwise bypass plan,
		// module-mask, and role gating. Hand it the gate state so its write path
		// enforces the same rules table-by-table (see enforceCompatWrite).
		if h.Compat != nil {
			h.Compat.planGate = gate
			h.Compat.featureGate = fgate
		}
	}

	if h.Ops != nil {
		// Ops reports/settings/plan endpoints read tenant data and must not be
		// reachable unauthenticated; its guest-request and platform routes already
		// self-check, so gating the whole handler here is safe.
		h.Ops.Register(api)
	}
	if h.Dashboard != nil {
		h.Dashboard.Register(api)
	}
	if h.Rooms != nil {
		h.Rooms.Register(api)
	}
	if h.Users != nil {
		h.Users.Register(api)
	}
	if h.Reservations != nil {
		h.Reservations.Register(api)
	}
	if h.Billing != nil {
		h.Billing.Register(api)
	}
	if h.Housekeeping != nil {
		h.Housekeeping.Register(api)
	}
	if h.Revenue != nil {
		h.Revenue.Register(api)
	}
	if h.Procurement != nil {
		h.Procurement.Register(api)
	}
	if h.CRM != nil {
		h.CRM.Register(api)
	}
	if h.Channel != nil {
		h.Channel.Register(api)
	}
	if h.NightAudit != nil {
		h.NightAudit.Register(api)
	}
	if h.Booking != nil {
		h.Booking.Register(api)
	}
	if h.Asset != nil {
		h.Asset.Register(api)
	}
	if h.Communications != nil {
		h.Communications.Register(api)
	}
	if h.POS != nil {
		h.POS.Register(api)
	}
	if h.Bulk != nil {
		h.Bulk.Register(api)
	}
	if h.Monitoring != nil {
		h.Monitoring.Register(api)
	}
	if h.Accounting != nil {
		h.Accounting.Register(api)
	}
	if h.Reservations != nil {
		api.Post("/booking/reservations", h.Reservations.Create)
	}
	if h.Procurement != nil {
		api.Patch("/procurement/purchase-orders/:id", h.Procurement.UpdatePOStatus)
	}
}
