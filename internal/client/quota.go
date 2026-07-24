package client

// Top-level read endpoints of the FCS API:
// GET /v1/quota (usage vs. limits, GCP quota-page parity) and
// GET /v1/images (VM image catalog; names are valid VmSpec.image values).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// Quota mirrors the Quota contract schema.
type Quota struct {
	MaxConcurrentEnvironments  int64 `json:"max_concurrent_environments"`
	UsedEnvironments           int64 `json:"used_environments"`
	MaxVMs                     int64 `json:"max_vms"`
	UsedVMs                    int64 `json:"used_vms"`
	MaxVCPU                    int64 `json:"max_vcpu"`
	UsedVCPU                   int64 `json:"used_vcpu"`
	MaxRAMGB                   int64 `json:"max_ram_gb"`
	UsedRAMGB                  int64 `json:"used_ram_gb"`
	MaxPublicIPs               int64 `json:"max_public_ips"`
	UsedPublicIPs              int64 `json:"used_public_ips"`
	MaxHarborRobotAccounts     int64 `json:"max_harbor_robot_accounts"`
	UsedHarborRobotAccounts    int64 `json:"used_harbor_robot_accounts"`
	MaxHarborArtifacts         int64 `json:"max_harbor_artifacts"`
	UsedHarborArtifacts        int64 `json:"used_harbor_artifacts"`
	MaxHarborRegistryBindings  int64 `json:"max_harbor_registry_bindings"`
	UsedHarborRegistryBindings int64 `json:"used_harbor_registry_bindings"`
}

// Image mirrors the Image schema.
type Image struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Source      string `json:"source"` // catalog | env
}

// GetQuota GETs /v1/quota (tenant-scoped usage vs. limits).
func (c *Client) GetQuota(ctx context.Context) (*Quota, error) {
	status, body, err := c.do(ctx, http.MethodGet, "/v1/quota", nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, apiError(status, body)
	}
	var q Quota
	if err := json.Unmarshal(body, &q); err != nil {
		return nil, fmt.Errorf("decoding quota: %w", err)
	}
	return &q, nil
}

// ListImages GETs /v1/images (VM image catalog approved for environments).
func (c *Client) ListImages(ctx context.Context) ([]Image, error) {
	status, body, err := c.do(ctx, http.MethodGet, "/v1/images", nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, apiError(status, body)
	}
	var images []Image
	if err := json.Unmarshal(body, &images); err != nil {
		return nil, fmt.Errorf("decoding image list: %w", err)
	}
	return images, nil
}
