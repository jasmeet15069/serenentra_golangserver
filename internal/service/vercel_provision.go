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

const vercelBaseURL = "https://api.vercel.com"

// VercelProvisionService creates and manages Vercel projects for client portals.
// When token is empty all methods are no-ops (log.Warn, return nil).
type VercelProvisionService struct {
	token      string
	teamID     string
	githubOrg  string
	githubRepo string
	log        *zap.Logger
	client     *http.Client
}

// NewVercelProvisionService creates a VercelProvisionService. Pass empty token
// to run in no-op mode (safe for local development without credentials).
func NewVercelProvisionService(token, teamID, githubOrg, githubRepo string, log *zap.Logger) *VercelProvisionService {
	return &VercelProvisionService{
		token:      token,
		teamID:     teamID,
		githubOrg:  githubOrg,
		githubRepo: githubRepo,
		log:        log,
		client:     &http.Client{Timeout: 20 * time.Second},
	}
}

// teamQuery returns the teamId query parameter string, or "" when no team is set.
func (s *VercelProvisionService) teamQuery() string {
	if s.teamID != "" {
		return "?teamId=" + s.teamID
	}
	return ""
}

// doRequest performs an authenticated HTTP request and returns the raw body,
// status code, and any transport-level error.
func (s *VercelProvisionService) doRequest(ctx context.Context, method, url string, body interface{}) ([]byte, int, error) {
	var bodyReader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, 0, fmt.Errorf("vercel: marshal request body: %w", err)
		}
		bodyReader = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return nil, 0, fmt.Errorf("vercel: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("vercel: do request: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	return raw, resp.StatusCode, nil
}

// CreateProject creates a Vercel project linked to the HMS portal GitHub repo.
// projectName must be unique in the team (use "hms-{slug}").
// Returns the new project's ID.
func (s *VercelProvisionService) CreateProject(ctx context.Context, projectName string) (projectID string, err error) {
	if s.token == "" {
		s.log.Warn("vercel: token not configured, skipping project creation",
			zap.String("project_name", projectName))
		return "", nil
	}

	url := vercelBaseURL + "/v10/projects" + s.teamQuery()
	body := map[string]interface{}{
		"name":      projectName,
		"framework": "other",
		"gitRepository": map[string]interface{}{
			"type": "github",
			"repo": s.githubOrg + "/" + s.githubRepo,
		},
	}

	raw, statusCode, err := s.doRequest(ctx, http.MethodPost, url, body)
	if err != nil {
		return "", err
	}
	if statusCode < 200 || statusCode >= 300 {
		s.log.Warn("vercel: CreateProject non-2xx response",
			zap.Int("status", statusCode),
			zap.String("body", string(raw)))
		return "", fmt.Errorf("vercel: CreateProject: status %d: %s", statusCode, string(raw))
	}

	var result struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", fmt.Errorf("vercel: CreateProject decode response: %w", err)
	}

	s.log.Info("vercel: project created",
		zap.String("project_name", projectName),
		zap.String("project_id", result.ID))
	return result.ID, nil
}

// SetEnvVar sets a plain env var on a project for both production and preview targets.
func (s *VercelProvisionService) SetEnvVar(ctx context.Context, projectID, key, value string) error {
	if s.token == "" {
		s.log.Warn("vercel: token not configured, skipping env var set",
			zap.String("project_id", projectID),
			zap.String("key", key))
		return nil
	}

	url := fmt.Sprintf("%s/v10/projects/%s/env%s", vercelBaseURL, projectID, s.teamQuery())
	body := map[string]interface{}{
		"key":    key,
		"value":  value,
		"type":   "plain",
		"target": []string{"production", "preview"},
	}

	raw, statusCode, err := s.doRequest(ctx, http.MethodPost, url, body)
	if err != nil {
		return err
	}
	if statusCode < 200 || statusCode >= 300 {
		s.log.Warn("vercel: SetEnvVar non-2xx response",
			zap.Int("status", statusCode),
			zap.String("body", string(raw)))
		return fmt.Errorf("vercel: SetEnvVar: status %d: %s", statusCode, string(raw))
	}

	s.log.Info("vercel: env var set", zap.String("project_id", projectID), zap.String("key", key))
	return nil
}

// AddDomain adds a custom domain to a project.
func (s *VercelProvisionService) AddDomain(ctx context.Context, projectID, domain string) error {
	if s.token == "" {
		s.log.Warn("vercel: token not configured, skipping domain add",
			zap.String("project_id", projectID),
			zap.String("domain", domain))
		return nil
	}

	url := fmt.Sprintf("%s/v10/projects/%s/domains%s", vercelBaseURL, projectID, s.teamQuery())
	body := map[string]interface{}{
		"name": domain,
	}

	raw, statusCode, err := s.doRequest(ctx, http.MethodPost, url, body)
	if err != nil {
		return err
	}
	if statusCode < 200 || statusCode >= 300 {
		s.log.Warn("vercel: AddDomain non-2xx response",
			zap.Int("status", statusCode),
			zap.String("body", string(raw)))
		return fmt.Errorf("vercel: AddDomain: status %d: %s", statusCode, string(raw))
	}

	s.log.Info("vercel: domain added", zap.String("project_id", projectID), zap.String("domain", domain))
	return nil
}

// TriggerDeploy triggers a deployment of the latest git commit on the main branch.
func (s *VercelProvisionService) TriggerDeploy(ctx context.Context, projectID string) error {
	if s.token == "" {
		s.log.Warn("vercel: token not configured, skipping deploy trigger",
			zap.String("project_id", projectID))
		return nil
	}

	url := vercelBaseURL + "/v13/deployments" + s.teamQuery()
	body := map[string]interface{}{
		"name": projectID,
		"gitSource": map[string]interface{}{
			"type": "github",
			"org":  s.githubOrg,
			"repo": s.githubRepo,
			"ref":  "main",
		},
	}

	raw, statusCode, err := s.doRequest(ctx, http.MethodPost, url, body)
	if err != nil {
		return err
	}
	if statusCode < 200 || statusCode >= 300 {
		s.log.Warn("vercel: TriggerDeploy non-2xx response",
			zap.Int("status", statusCode),
			zap.String("body", string(raw)))
		return fmt.Errorf("vercel: TriggerDeploy: status %d: %s", statusCode, string(raw))
	}

	s.log.Info("vercel: deploy triggered", zap.String("project_id", projectID))
	return nil
}

// DeleteProject deletes a Vercel project by ID.
func (s *VercelProvisionService) DeleteProject(ctx context.Context, projectID string) error {
	if s.token == "" {
		s.log.Warn("vercel: token not configured, skipping project deletion",
			zap.String("project_id", projectID))
		return nil
	}

	url := fmt.Sprintf("%s/v9/projects/%s%s", vercelBaseURL, projectID, s.teamQuery())

	raw, statusCode, err := s.doRequest(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return err
	}
	if statusCode < 200 || statusCode >= 300 {
		s.log.Warn("vercel: DeleteProject non-2xx response",
			zap.Int("status", statusCode),
			zap.String("body", string(raw)))
		return fmt.Errorf("vercel: DeleteProject: status %d: %s", statusCode, string(raw))
	}

	s.log.Info("vercel: project deleted", zap.String("project_id", projectID))
	return nil
}
