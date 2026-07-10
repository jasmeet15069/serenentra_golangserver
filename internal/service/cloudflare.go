package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"go.uber.org/zap"
)

const cfBaseURL = "https://api.cloudflare.com/client/v4"

// CloudflareService manages DNS records for tenant subdomains via the
// Cloudflare API v4. When token or zoneID are empty, all methods are no-ops
// (log.Warn and return nil).
type CloudflareService struct {
	token  string
	zoneID string
	log    *zap.Logger
	client *http.Client
}

// NewCloudflareService creates a CloudflareService. Pass empty token or zoneID
// to run in no-op mode (safe for local development without credentials).
func NewCloudflareService(token, zoneID string, log *zap.Logger) *CloudflareService {
	return &CloudflareService{
		token:  token,
		zoneID: zoneID,
		log:    log,
		client: &http.Client{Timeout: 15 * time.Second},
	}
}

// AddCNAME creates a proxied=false CNAME record: name → content.
// name is the subdomain label only (e.g. "marigold"), not the FQDN.
// Returns the Cloudflare record ID that can be used later with DeleteRecord.
func (s *CloudflareService) AddCNAME(ctx context.Context, name, content string) (recordID string, err error) {
	if s.token == "" || s.zoneID == "" {
		s.log.Warn("cloudflare: token or zoneID not configured, skipping CNAME creation",
			zap.String("name", name))
		return "", nil
	}

	body, _ := json.Marshal(map[string]interface{}{
		"type":    "CNAME",
		"name":    name,
		"content": content,
		"ttl":     1,
		"proxied": false,
	})

	url := fmt.Sprintf("%s/zones/%s/dns_records", cfBaseURL, s.zoneID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("cloudflare: build AddCNAME request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("cloudflare: AddCNAME request failed: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)

	var cfResp struct {
		Success bool `json:"success"`
		Result  struct {
			ID string `json:"id"`
		} `json:"result"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &cfResp); err != nil {
		return "", fmt.Errorf("cloudflare: AddCNAME decode response: %w", err)
	}
	if !cfResp.Success {
		msg := "unknown error"
		if len(cfResp.Errors) > 0 {
			msg = cfResp.Errors[0].Message
		}
		s.log.Warn("cloudflare: AddCNAME failed", zap.String("name", name), zap.String("error", msg))
		return "", fmt.Errorf("cloudflare: AddCNAME: %s", msg)
	}

	s.log.Info("cloudflare: CNAME created",
		zap.String("name", name),
		zap.String("content", content),
		zap.String("record_id", cfResp.Result.ID))
	return cfResp.Result.ID, nil
}

// DeleteRecord deletes a DNS record by its Cloudflare record ID.
func (s *CloudflareService) DeleteRecord(ctx context.Context, recordID string) error {
	if s.token == "" || s.zoneID == "" {
		s.log.Warn("cloudflare: token or zoneID not configured, skipping record deletion",
			zap.String("record_id", recordID))
		return nil
	}

	url := fmt.Sprintf("%s/zones/%s/dns_records/%s", cfBaseURL, s.zoneID, recordID)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return fmt.Errorf("cloudflare: build DeleteRecord request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+s.token)

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("cloudflare: DeleteRecord request failed: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)

	var cfResp struct {
		Success bool `json:"success"`
		Errors  []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &cfResp); err != nil {
		return fmt.Errorf("cloudflare: DeleteRecord decode response: %w", err)
	}
	if !cfResp.Success {
		msg := "unknown error"
		if len(cfResp.Errors) > 0 {
			msg = cfResp.Errors[0].Message
		}
		s.log.Warn("cloudflare: DeleteRecord failed",
			zap.String("record_id", recordID),
			zap.String("error", msg))
		return fmt.Errorf("cloudflare: DeleteRecord: %s", msg)
	}

	s.log.Info("cloudflare: DNS record deleted", zap.String("record_id", recordID))
	return nil
}
