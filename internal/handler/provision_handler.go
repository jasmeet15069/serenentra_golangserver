package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/hotelharmony/api/pkg/response"
)

// provStep records the outcome of a single provisioning pipeline step.
type provStep struct {
	Name   string    `json:"name"`
	Status string    `json:"status"` // "done" | "failed" | "skipped"
	Error  string    `json:"error,omitempty"`
	At     time.Time `json:"at"`
}

// ProvisionStatus (GET /api/platform/tenants/:id/provision-status) returns the
// latest provisioning_jobs row for the tenant plus tenant_registry fields.
//
// Response shape:
//
//	{
//	  "job_id": "...", "status": "running|done|failed",
//	  "steps": [...],
//	  "vercel_project_id": "...", "vercel_domain": "...", "dns_record_id": "...",
//	  "provision_status": "...",
//	  "error": "..."
//	}
func (h *OperationsHandler) ProvisionStatus(c *fiber.Ctx) error {
	if !h.requirePlatformAdmin(c) {
		return nil
	}
	id, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "invalid tenant id")
	}

	// Get tenant_registry provisioning columns.
	var vercelProjectID, vercelDomain, dnsRecordID *string
	var provisionStatus string
	err = h.pool.QueryRow(c.Context(),
		`SELECT vercel_project_id, vercel_domain, dns_record_id, provision_status
		 FROM tenant_registry WHERE hotel_id = $1`, id,
	).Scan(&vercelProjectID, &vercelDomain, &dnsRecordID, &provisionStatus)
	if err != nil {
		return response.Error(c, fiber.StatusNotFound, "tenant not found in registry")
	}

	// Get the latest provisioning job (there may be none yet).
	var jobID uuid.UUID
	var jobStatus string
	var stepsRaw []byte
	var jobError *string
	err = h.pool.QueryRow(c.Context(),
		`SELECT id, status, steps, error
		 FROM provisioning_jobs
		 WHERE hotel_id = $1
		 ORDER BY created_at DESC LIMIT 1`, id,
	).Scan(&jobID, &jobStatus, &stepsRaw, &jobError)
	if err != nil {
		// No job exists yet — return registry state with empty steps.
		return response.OK(c, map[string]interface{}{
			"job_id":            nil,
			"status":            "pending",
			"steps":             []interface{}{},
			"vercel_project_id": vercelProjectID,
			"vercel_domain":     vercelDomain,
			"dns_record_id":     dnsRecordID,
			"provision_status":  provisionStatus,
			"error":             nil,
		})
	}

	// Decode steps JSONB array for the response.
	var steps []interface{}
	_ = json.Unmarshal(stepsRaw, &steps)
	if steps == nil {
		steps = []interface{}{}
	}

	return response.OK(c, map[string]interface{}{
		"job_id":            jobID,
		"status":            jobStatus,
		"steps":             steps,
		"vercel_project_id": vercelProjectID,
		"vercel_domain":     vercelDomain,
		"dns_record_id":     dnsRecordID,
		"provision_status":  provisionStatus,
		"error":             jobError,
	})
}

// runProvisioningJob runs all provisioning steps for a newly created tenant.
// It is designed to run as a goroutine (fire-and-forget from CreatePlatformTenant).
//
// Steps executed (each recorded in provisioning_jobs.steps JSONB):
//  1. db    — already done by caller; recorded as "done"
//  2. dns   — GoDaddy A record {slug}.serenentra.com → VPS IP
//  3. nginx — host provisioner: nginx config + certbot SSL + nginx reload
//
// On any step error the function records the failure and returns early.
// On full success it sets tenant_registry.provision_status = "active".
func (h *OperationsHandler) runProvisioningJob(ctx context.Context, jobID, hotelID uuid.UUID, slug string) {
	var steps []provStep

	recordStep := func(name, status, errMsg string) {
		steps = append(steps, provStep{
			Name:   name,
			Status: status,
			Error:  errMsg,
			At:     time.Now().UTC(),
		})
		raw, _ := json.Marshal(steps)
		if status == "failed" {
			_, _ = h.pool.Exec(ctx,
				`UPDATE provisioning_jobs
				 SET status = 'failed', steps = $1::jsonb, error = $2, updated_at = now()
				 WHERE id = $3`,
				string(raw), errMsg, jobID)
		} else {
			_, _ = h.pool.Exec(ctx,
				`UPDATE provisioning_jobs
				 SET steps = $1::jsonb, updated_at = now()
				 WHERE id = $2`,
				string(raw), jobID)
		}
	}

	markDone := func() {
		raw, _ := json.Marshal(steps)
		_, _ = h.pool.Exec(ctx,
			`UPDATE provisioning_jobs
			 SET status = 'done', steps = $1::jsonb, updated_at = now()
			 WHERE id = $2`,
			string(raw), jobID)
		_, _ = h.pool.Exec(ctx,
			`UPDATE tenant_registry
			 SET provision_status = 'active', updated_at = now()
			 WHERE hotel_id = $1`,
			hotelID)
	}

	// Step 1: db — already completed by the caller before starting this goroutine.
	recordStep("db", "done", "")

	baseDomain := h.provCfg.TenantBaseDomain
	tenantDomain := fmt.Sprintf("%s.%s", slug, baseDomain)
	vpsIP := h.provCfg.VpsIP

	// Step 2: dns — create A record {slug}.serenentra.com → VPS IP via GoDaddy API.
	if h.godaddy != nil {
		if err := h.godaddy.AddARecord(ctx, baseDomain, slug, vpsIP); err != nil {
			recordStep("dns", "failed", err.Error())
			return
		}
		_, _ = h.pool.Exec(ctx,
			`UPDATE tenant_registry SET dns_record_id = $1, updated_at = now() WHERE hotel_id = $2`,
			slug, hotelID)
		recordStep("dns", "done", "")
	} else {
		recordStep("dns", "skipped", "")
	}

	// Step 3: nginx — write nginx config + run certbot + reload via host provisioner.
	if h.provisioner != nil {
		if err := h.provisioner.ProvisionDomain(ctx, tenantDomain); err != nil {
			recordStep("nginx", "failed", err.Error())
			return
		}
		_, _ = h.pool.Exec(ctx,
			`UPDATE tenant_registry SET vercel_domain = $1, updated_at = now() WHERE hotel_id = $2`,
			tenantDomain, hotelID)
		recordStep("nginx", "done", "")
	} else {
		recordStep("nginx", "skipped", "")
	}

	// Step 4: redis — confirm the tenant's dedicated key namespace.
	// Redis isolation uses key prefixing (t:{hotel_id}:*) on the shared instance.
	if h.cache != nil {
		nsKey := "t:" + hotelID.String() + ":ready"
		_ = h.cache.Set(ctx, nsKey, "1", 0)
		recordStep("redis", "done", "")
	} else {
		recordStep("redis", "skipped", "")
	}

	markDone()
}
