package client

// Cluster sub-API of the FCS API:
// /v1/environments/{id}/clusters[/{clusterId}[/kubeconfig]].
//
// The provider maps one Terraform resource type per cluster kind onto this
// single sub-API. SA tokens and kubeconfigs are never logged.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// Cluster mirrors the stable contract fields of the Cluster schema.
type Cluster struct {
	ID                      string         `json:"id"`
	Kind                    string         `json:"kind"`
	Status                  string         `json:"status"`
	APIServerURL            string         `json:"api_server_url"`
	ClusterCIDR             string         `json:"cluster_cidr"`
	ServiceCIDR             string         `json:"service_cidr"`
	ProvisioningDiagnostics string         `json:"provisioning_diagnostics"`
	VCPU                    int64          `json:"vcpu"`
	RAMGB                   int64          `json:"ram_gb"`
	StorageGB               int64          `json:"storage_gb"`
	CPNodes                 int64          `json:"cp_nodes"`
	CPVcpu                  int64          `json:"cp_vcpu"`
	CPRamGB                 int64          `json:"cp_ram_gb"`
	WorkerNodes             int64          `json:"worker_nodes"`
	WorkerVcpu              int64          `json:"worker_vcpu"`
	WorkerRamGB             int64          `json:"worker_ram_gb"`
	PVCStorageGB            int64          `json:"pvc_storage_gb"`
	DesiredSpec             *ClusterSizing `json:"desired_spec,omitempty"`
}

// ClusterSizing is the server-resolved target of an in-place resize. The API
// includes it in the PATCH response so clients do not have to duplicate the
// product catalog's t-shirt-size mapping.
type ClusterSizing struct {
	VCPU         int64 `json:"vcpu"`
	RAMGB        int64 `json:"ram_gb"`
	StorageGB    int64 `json:"storage_gb"`
	CPNodes      int64 `json:"cp_nodes"`
	CPVcpu       int64 `json:"cp_vcpu"`
	CPRamGB      int64 `json:"cp_ram_gb"`
	WorkerNodes  int64 `json:"worker_nodes"`
	WorkerVcpu   int64 `json:"worker_vcpu"`
	WorkerRamGB  int64 `json:"worker_ram_gb"`
	PVCStorageGB int64 `json:"pvc_storage_gb"`
}

// ClusterSpec is the create payload (ClusterSpec schema). Size and the
// custom vcpu/ram_gb/storage_gb sizing are mutually exclusive; zero values
// are omitted so the server applies its defaults (size=S).
//
// The CP*/Worker*/PVCStorageGB/RKE2Version fields are used by
// kind="dedicated": a real RKE2 cluster sized via explicit control-plane and
// worker node pools instead of the t-shirt size. They are omitempty so they
// never appear in a business/flex/namespace payload.
type ClusterSpec struct {
	Kind       string `json:"kind"`
	Size       string `json:"size,omitempty"`
	VCPU       int64  `json:"vcpu,omitempty"`
	RAMGB      int64  `json:"ram_gb,omitempty"`
	StorageGB  int64  `json:"storage_gb,omitempty"`
	K8sVersion string `json:"k8s_version,omitempty"`

	// Dedicated (kind="dedicated") node-pool sizing.
	CPNodes      int64  `json:"cp_nodes,omitempty"`
	CPVcpu       int64  `json:"cp_vcpu,omitempty"`
	CPRamGB      int64  `json:"cp_ram_gb,omitempty"`
	WorkerNodes  int64  `json:"worker_nodes,omitempty"`
	WorkerVcpu   int64  `json:"worker_vcpu,omitempty"`
	WorkerRamGB  int64  `json:"worker_ram_gb,omitempty"`
	PVCStorageGB int64  `json:"pvc_storage_gb,omitempty"`
	RKE2Version  string `json:"rke2_version,omitempty"`
}

// ClusterResizeSpec is the partial in-place sizing payload. Kubernetes
// versions are intentionally absent because version changes still replace the
// resource.
type ClusterResizeSpec struct {
	Size         string `json:"size,omitempty"`
	VCPU         int64  `json:"vcpu,omitempty"`
	RAMGB        int64  `json:"ram_gb,omitempty"`
	StorageGB    int64  `json:"storage_gb,omitempty"`
	CPNodes      int64  `json:"cp_nodes,omitempty"`
	CPVcpu       int64  `json:"cp_vcpu,omitempty"`
	CPRamGB      int64  `json:"cp_ram_gb,omitempty"`
	WorkerNodes  *int64 `json:"worker_nodes,omitempty"`
	WorkerVcpu   *int64 `json:"worker_vcpu,omitempty"`
	WorkerRamGB  *int64 `json:"worker_ram_gb,omitempty"`
	PVCStorageGB int64  `json:"pvc_storage_gb,omitempty"`
}

// KubeconfigCredentials is the response of the kubeconfig mint endpoint.
// All fields are short-lived; the caller must never persist or log them.
// kubeconfig and sa_token may be absent depending on backend capabilities.
type KubeconfigCredentials struct {
	APIServerURL string `json:"api_server_url"`
	Kubeconfig   string `json:"kubeconfig"`
	SAToken      string `json:"sa_token"`
	ExpiresAt    string `json:"expires_at"`
}

func clusterCollectionPath(envID string) string {
	return "/v1/environments/" + url.PathEscape(envID) + "/clusters"
}

func clusterItemPath(envID, clusterID string) string {
	return clusterCollectionPath(envID) + "/" + url.PathEscape(clusterID)
}

// CreateCluster POSTs /v1/environments/{id}/clusters. 202 (newly created,
// status=provisioning) and 200 (idempotent re-apply: the server is
// idempotent over (environment, kind) and returns the existing non-terminal
// cluster, even when the spec differs) are success.
func (c *Client) CreateCluster(ctx context.Context, envID string, spec ClusterSpec) (*Cluster, error) {
	status, body, err := c.do(ctx, http.MethodPost, clusterCollectionPath(envID), spec)
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusAccepted, http.StatusOK:
		var cl Cluster
		if err := json.Unmarshal(body, &cl); err != nil {
			return nil, fmt.Errorf("decoding cluster response (HTTP %d): %w", status, err)
		}
		if cl.ID == "" {
			return nil, fmt.Errorf("cluster create returned HTTP %d without a cluster id", status)
		}
		return &cl, nil
	default:
		return nil, apiError(status, body)
	}
}

// GetCluster GETs /v1/environments/{id}/clusters/{clusterId}. A 404 is
// returned as an APIError for which IsNotFound is true.
func (c *Client) GetCluster(ctx context.Context, envID, clusterID string) (*Cluster, error) {
	status, body, err := c.do(ctx, http.MethodGet, clusterItemPath(envID, clusterID), nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, apiError(status, body)
	}
	var cl Cluster
	if err := json.Unmarshal(body, &cl); err != nil {
		return nil, fmt.Errorf("decoding cluster: %w", err)
	}
	return &cl, nil
}

// ResizeCluster PATCHes the desired size of an existing cluster. The API
// returns 202 while the same cluster ID converges asynchronously.
func (c *Client) ResizeCluster(ctx context.Context, envID, clusterID string, spec ClusterResizeSpec) (*Cluster, error) {
	status, body, err := c.do(ctx, http.MethodPatch, clusterItemPath(envID, clusterID), spec)
	if err != nil {
		return nil, err
	}
	if status != http.StatusAccepted && status != http.StatusOK {
		return nil, apiError(status, body)
	}
	var cl Cluster
	if err := json.Unmarshal(body, &cl); err != nil {
		return nil, fmt.Errorf("decoding cluster resize response (HTTP %d): %w", status, err)
	}
	if cl.ID == "" {
		return nil, fmt.Errorf("cluster resize returned HTTP %d without a cluster id", status)
	}
	return &cl, nil
}

// DeleteCluster DELETEs /v1/environments/{id}/clusters/{clusterId}.
// 202 (teardown started) and 404 (already gone) both count as success.
func (c *Client) DeleteCluster(ctx context.Context, envID, clusterID string) error {
	status, body, err := c.do(ctx, http.MethodDelete, clusterItemPath(envID, clusterID), nil)
	if err != nil {
		return err
	}
	switch status {
	case http.StatusAccepted, http.StatusNotFound:
		return nil
	default:
		return apiError(status, body)
	}
}

// MintKubeconfig POSTs /v1/environments/{id}/clusters/{clusterId}/kubeconfig
// and returns short-lived, cluster-scoped credentials (contract: 201).
func (c *Client) MintKubeconfig(ctx context.Context, envID, clusterID string) (*KubeconfigCredentials, error) {
	status, body, err := c.do(ctx, http.MethodPost, clusterItemPath(envID, clusterID)+"/kubeconfig", nil)
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusCreated, http.StatusOK:
		var creds KubeconfigCredentials
		if err := json.Unmarshal(body, &creds); err != nil {
			return nil, fmt.Errorf("decoding kubeconfig response (HTTP %d): %w", status, err)
		}
		return &creds, nil
	default:
		return nil, apiError(status, body)
	}
}
