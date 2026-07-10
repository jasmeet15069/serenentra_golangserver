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

// ProvisionerClient calls the host-side hms-provisioner daemon that runs on
// the Hetzner VPS host (outside Docker) and has root access to nginx + certbot.
// The daemon listens on 127.0.0.1:9001 and is reachable from the Docker
// container via host.docker.internal:9001.
// When url is empty all methods are no-ops (log.Warn, return nil).
type ProvisionerClient struct {
	url    string
	secret string
	log    *zap.Logger
	client *http.Client
}

func NewProvisionerClient(url, secret string, log *zap.Logger) *ProvisionerClient {
	return &ProvisionerClient{
		url:    url,
		secret: secret,
		log:    log,
		client: &http.Client{Timeout: 180 * time.Second}, // certbot can take ~60s
	}
}

// ProvisionDomain asks the host provisioner to write nginx config + run certbot
// + reload nginx for the given fully-qualified domain (e.g. client1.serenentra.com).
func (c *ProvisionerClient) ProvisionDomain(ctx context.Context, domain string) error {
	if c.url == "" {
		c.log.Warn("provisioner: url not configured, skipping nginx/cert step",
			zap.String("domain", domain))
		return nil
	}

	body, _ := json.Marshal(map[string]string{"domain": domain})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url+"/provision", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("provisioner: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Provisioner-Secret", c.secret)

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("provisioner: request failed: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		var errResp struct {
			Detail string `json:"detail"`
		}
		_ = json.Unmarshal(raw, &errResp)
		if errResp.Detail != "" {
			return fmt.Errorf("provisioner: %s", errResp.Detail)
		}
		return fmt.Errorf("provisioner: status %d: %s", resp.StatusCode, string(raw))
	}

	c.log.Info("provisioner: domain provisioned", zap.String("domain", domain))
	return nil
}

// DeprovisionDomain asks the host provisioner to remove nginx config for the domain.
func (c *ProvisionerClient) DeprovisionDomain(ctx context.Context, domain string) error {
	if c.url == "" {
		return nil
	}

	body, _ := json.Marshal(map[string]string{"domain": domain})
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.url+"/provision", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("provisioner: build deprovision request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Provisioner-Secret", c.secret)

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("provisioner: deprovision request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("provisioner: deprovision status %d: %s", resp.StatusCode, string(raw))
	}

	c.log.Info("provisioner: domain deprovisioned", zap.String("domain", domain))
	return nil
}
