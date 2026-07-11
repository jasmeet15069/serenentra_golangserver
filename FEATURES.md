# Serenentra — Product Features

Serenentra is a multi-tenant, cloud-hosted **Hotel Management System (HMS)** for
hotels, resorts, and restaurants. Each client runs an isolated instance of the
platform, branded to them, with only the modules their plan includes.

This document is a high-level, shareable overview of what the product does. It is
the same across every Serenentra repository so each codebase carries the same
feature context.

---

## Who it's for

- **Independent hotels & resorts** — front desk, housekeeping, F&B, billing in one place.
- **Restaurant / F&B outlets** — dine-in POS with KOT, GST-compliant billing.
- **Small hotel groups** — multiple properties (branches) under one client account.

Each client is provisioned as its own tenant with a dedicated subdomain, its own
data, its own branding, and a plan that controls which modules are visible.

---

## Feature modules

### Guest stay lifecycle
- **Dashboard** — occupancy, revenue, arrivals/departures, and operational KPIs at a glance.
- **Reservations** — create, modify, and manage bookings; availability and room search.
- **Front Desk** — check-in / check-out, room assignment, guest lookup, walk-ins.

### Food & Beverage
- **Restaurant POS (Dine-In)** — outlets, tables, dining sessions, KOT (kitchen order tickets),
  itemized bills, split/partial payments (cash / card / UPI), and printable GST invoices.
- **Menu Management** — categories, items, pricing, availability, and customizations.
- **Room service / orders** — in-room dining tied to a guest's stay.

### Operations
- **Housekeeping** — room status, cleaning assignments, and inspection workflow.
- **Maintenance** — work orders, asset tracking, and issue resolution.
- **Inventory** — stock items, reorder thresholds, and perishable/expiry tracking.
- **Procurement** — vendors, purchase orders, and goods receipt.

### Revenue & distribution
- **Booking Engine** — direct-booking widget, promotions/promo codes, and **rate plans**
  (named pricing rules with discounts and minimum-stay requirements).
- **Channel Manager** — OTA/channel configuration and rate-parity checks.
- **Revenue Management** — dynamic pricing signals and recommendations.

### Money
- **Billing & Finance** — folios, invoices, payments, and receivables.
- **Accounting** — double-entry journal, trial balance, and GST-aware records.
- **Night Audit** — end-of-day reconciliation and posting.

### Insights
- **Reports & Analytics** — occupancy, revenue by department, guest, and channel breakdowns,
  with CSV export.

### Property & organization
- **Properties / Branches** — real multi-property management under one client, with
  plan-enforced limits on how many branches a client can add.
- **Room Management** — rooms, room types, floors, capacity, rates, and amenities.
- **Setup Wizard** — guided onboarding for a new client.
- **Users & Roles** — staff accounts with role-based access (admin, receptionist, cashier,
  housekeeping, food manager, and more), plus CSV bulk import.

### Guest relationship
- **CRM & Loyalty** — guest profiles, stay history, loyalty points, and campaigns.

### Platform / Superadmin (operator-facing)
- **Client provisioning** — create a new client with its own subdomain and branding.
- **Plans & module masking** — control which modules each client sees, per plan tier.
- **Feature matrix** — fine-grained, role-by-feature access control per tenant.
- **Support access** — secure, time-limited impersonation so support can help a client.
- **Monitoring & security overview** — platform-level health and posture at a glance.

---

## Plans

Modules are gated by plan tier (e.g. **Basic / Pro / Premium**) combined with a
per-tenant module mask. A feature is visible to a client only when its plan
includes the module **and** the operator has enabled it for that client — so
downgrading a plan cleanly hides the higher-tier features.

---

## Architecture (high level)

- **Backend** — a single Go (Fiber) API service. Stateless request handling,
  JSON over HTTPS, JWT-based auth.
- **Data** — PostgreSQL for durable data, Redis for caching and short-lived tokens.
- **Frontends** — the client-facing **HMS admin portal** and the operator-facing
  **superadmin console**, both built with TanStack Start (React) and server-side
  rendering; plus a public marketing site.
- **Multi-tenancy & isolation** — identity/auth is centralized; a client's
  operational data can live either row-scoped in a shared database or, for higher
  tiers, in a **dedicated per-client database**. Requests resolve to the right
  data store automatically based on the tenant.
- **Gating layers** — every request passes authentication, a plan-inclusion check,
  and a role/feature-matrix check before reaching tenant data.

---

## Repositories

| Repo | What it is |
|---|---|
| `serenentra_golangserver` | The Go/Fiber backend API (the one shared backend). |
| `HmsAdminStaffPortal` | The client-facing HMS admin portal (staff app). |
| `superadmin_serenentra` | The operator-facing superadmin console. |
| `serenentra-landing` | The public marketing / landing site. |

---

*This is a product overview only. It intentionally contains no credentials,
infrastructure addresses, or other operational secrets.*
