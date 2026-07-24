package client

// VM sub-API of the FCS API:
// /v1/environments/{id}/vms[/{vmId}[/power]].
//
// VMs are brokered server-side; the client only sees stable API fields.
// cloud-init payloads may contain secrets and are never logged.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// Vm mirrors the stable contract fields of the Vm schema. name is an
// additive contract field (optional idempotency key per environment); the
// server may omit it. vm_ip and console_url are nullable on the wire
// (null until the VM is ready / until the console broker exists); JSON
// null leaves the zero value, so both surface as empty strings here and
// the provider maps them to null state attributes.
type Vm struct {
	ID               string  `json:"id"`
	Name             string  `json:"name,omitempty"`
	Image            string  `json:"image,omitempty"`
	HarborArtifactID string  `json:"harbor_artifact_id,omitempty"`
	CPUCores         *int64  `json:"cpu_cores,omitempty"`
	MemoryGB         *int64  `json:"memory_gb,omitempty"`
	DiskGB           *int64  `json:"disk_gb,omitempty"`
	NICNetwork       *string `json:"nic_network,omitempty"`
	Running          *bool   `json:"running,omitempty"`
	VdcID            *string `json:"vdc_id,omitempty"`
	NetworkID        *string `json:"network_id,omitempty"`
	Status           string  `json:"status"`
	VMIP             string  `json:"vm_ip"`
	ConsoleURL       string  `json:"console_url"`
}

// VmStatus is the live KubeVirt status returned by the VM status endpoint.
// Unlike Vm.Status, Phase reflects the observed VMI state and can therefore
// be used to settle asynchronous power operations.
type VmStatus struct {
	Phase         string  `json:"phase"`
	Reason        *string `json:"reason"`
	Message       string  `json:"message"`
	PlatformError bool    `json:"platform_error"`
}

// VmSpec is the create payload (VmSpec schema). Zero values are omitted so
// the server applies its defaults (cpu_cores=2, memory_gb=4, disk_gb=20,
// nic_network=tenant). Running is a pointer because false is a meaningful
// value distinct from "unset" (server default: true).
type VmSpec struct {
	Image                string `json:"image,omitempty"`
	HarborArtifactID     string `json:"harbor_artifact_id,omitempty"`
	Name                 string `json:"name,omitempty"`
	CPUCores             int64  `json:"cpu_cores,omitempty"`
	MemoryGB             int64  `json:"memory_gb,omitempty"`
	DiskGB               int64  `json:"disk_gb,omitempty"`
	NICNetwork           string `json:"nic_network,omitempty"`
	CloudInitUserdata    string `json:"cloud_init_userdata,omitempty"`
	CloudInitNetworkdata string `json:"cloud_init_networkdata,omitempty"`
	Running              *bool  `json:"running,omitempty"`
	VdcID                string `json:"vdc_id,omitempty"`
	NetworkID            string `json:"network_id,omitempty"`
}

func vmCollectionPath(envID string) string {
	return "/v1/environments/" + url.PathEscape(envID) + "/vms"
}

func vmItemPath(envID, vmID string) string {
	return vmCollectionPath(envID) + "/" + url.PathEscape(vmID)
}

// CreateVm POSTs /v1/environments/{id}/vms. 202 (newly created,
// status=provisioning) and 200 (idempotent re-POST: a VM with this name
// already exists in the environment) are both success.
func (c *Client) CreateVm(ctx context.Context, envID string, spec VmSpec) (*Vm, error) {
	status, body, err := c.do(ctx, http.MethodPost, vmCollectionPath(envID), spec)
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusAccepted, http.StatusOK:
		var vm Vm
		if err := json.Unmarshal(body, &vm); err != nil {
			return nil, fmt.Errorf("decoding vm response (HTTP %d): %w", status, err)
		}
		if vm.ID == "" {
			return nil, fmt.Errorf("vm create returned HTTP %d without a vm id", status)
		}
		return &vm, nil
	default:
		return nil, apiError(status, body)
	}
}

// GetVm GETs /v1/environments/{id}/vms/{vmId}. A 404 (unknown or foreign
// tenant) is returned as an APIError for which IsNotFound is true.
func (c *Client) GetVm(ctx context.Context, envID, vmID string) (*Vm, error) {
	status, body, err := c.do(ctx, http.MethodGet, vmItemPath(envID, vmID), nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, apiError(status, body)
	}
	var vm Vm
	if err := json.Unmarshal(body, &vm); err != nil {
		return nil, fmt.Errorf("decoding vm: %w", err)
	}
	return &vm, nil
}

// GetVmStatus reads the live KubeVirt phase for a VM. A 404 (unknown,
// destroyed or foreign tenant) is returned as an APIError.
func (c *Client) GetVmStatus(ctx context.Context, envID, vmID string) (*VmStatus, error) {
	status, body, err := c.do(ctx, http.MethodGet, vmItemPath(envID, vmID)+"/status", nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, apiError(status, body)
	}
	var vmStatus VmStatus
	if err := json.Unmarshal(body, &vmStatus); err != nil {
		return nil, fmt.Errorf("decoding vm status: %w", err)
	}
	if vmStatus.Phase == "" {
		return nil, fmt.Errorf("vm status response did not include a phase")
	}
	return &vmStatus, nil
}

// DeleteVm DELETEs /v1/environments/{id}/vms/{vmId}. 202 (teardown
// started) and 404 (already gone) both count as success.
func (c *Client) DeleteVm(ctx context.Context, envID, vmID string) error {
	status, body, err := c.do(ctx, http.MethodDelete, vmItemPath(envID, vmID), nil)
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

// PowerVm POSTs /v1/environments/{id}/vms/{vmId}/power with one of the
// contract power verbs (restart | stop | start). 202 is success.
func (c *Client) PowerVm(ctx context.Context, envID, vmID, action string) error {
	payload := map[string]string{"action": action}
	status, body, err := c.do(ctx, http.MethodPost, vmItemPath(envID, vmID)+"/power", payload)
	if err != nil {
		return err
	}
	switch status {
	case http.StatusAccepted, http.StatusOK:
		return nil
	default:
		return apiError(status, body)
	}
}
