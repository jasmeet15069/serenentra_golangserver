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

const godaddyBaseURL = "https://api.godaddy.com/v1"

// GoDaddyService manages DNS A records for tenant subdomains via the GoDaddy API.
// When apiKey or apiSecret are empty all methods are no-ops (log.Warn, return nil).
type GoDaddyService struct {
	apiKey    string
	apiSecret string
	log       *zap.Logger
	client    *http.Client
}

func NewGoDaddyService(apiKey, apiSecret string, log *zap.Logger) *GoDaddyService {
	return &GoDaddyService{
		apiKey:    apiKey,
		apiSecret: apiSecret,
		log:       log,
		client:    &http.Client{Timeout: 15 * time.Second},
	}
}

// AddARecord creates or replaces an A record: {name}.{domain} → ip.
// GoDaddy PUT is idempotent — safe to call multiple times for the same subdomain.
func (s *GoDaddyService) AddARecord(ctx context.Context, domain, name, ip string) error {
	if s.apiKey == "" || s.apiSecret == "" {
		s.log.Warn("godaddy: credentials not configured, skipping A record creation",
			zap.String("name", name))
		return nil
	}

	body, _ := json.Marshal([]map[string]interface{}{
		{"data": ip, "ttl": 600},
	})

	url := fmt.Sprintf("%s/domains/%s/records/A/%s", godaddyBaseURL, domain, name)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("godaddy: build request: %w", err)
	}
	req.Header.Set("Authorization", fmt.Sprintf("sso-key %s:%s", s.apiKey, s.apiSecret))
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("godaddy: AddARecord request failed: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("godaddy: AddARecord status %d: %s", resp.StatusCode, string(raw))
	}

	s.log.Info("godaddy: A record created",
		zap.String("domain", domain),
		zap.String("name", name),
		zap.String("ip", ip))
	return nil
}

// DeleteARecord removes an A record for the given subdomain name.
func (s *GoDaddyService) DeleteARecord(ctx context.Context, domain, name string) error {
	if s.apiKey == "" || s.apiSecret == "" {
		return nil
	}

	url := fmt.Sprintf("%s/domains/%s/records/A/%s", godaddyBaseURL, domain, name)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return fmt.Errorf("godaddy: build delete request: %w", err)
	}
	req.Header.Set("Authorization", fmt.Sprintf("sso-key %s:%s", s.apiKey, s.apiSecret))

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("godaddy: DeleteARecord request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("godaddy: DeleteARecord status %d: %s", resp.StatusCode, string(raw))
	}

	s.log.Info("godaddy: A record deleted", zap.String("domain", domain), zap.String("name", name))
	return nil
}
