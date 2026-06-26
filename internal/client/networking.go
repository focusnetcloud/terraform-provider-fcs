package client

// Ingress/Egress sub-API of the FCS API:
//
//	/v1/environments/{id}/ingress[/{ingressId}]
//	/v1/environments/{id}/egress[/{egressId}]
//
// Both endpoints are an additive facade over the existing NAT-GW DNAT
// (port-forward) and egress-ACL reconcilers; the provider only sees the stable
// API fields.
//
// Status machine: a live record is always status="provisioning" (the row is
// persisted; the NAT-GW reconcile runs asynchronously and there is NO "active"
// terminal state). DELETE flips it to "offboarding"; once the reconcile has
// removed the rule the GET returns 404. Hence: no poll-to-active on create,
// only poll-to-gone on delete.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// Ingress mirrors the stable contract fields of the Ingress schema.
// public_url is nullable on the wire (null until the hostname is known); JSON
// null leaves the zero value, so it surfaces as an empty string here and the
// provider maps it to a null state attribute.
type Ingress struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	PublicURL string `json:"public_url"`
}

// IngressSpec is the create payload (IngressSpec schema). The idempotency key
// is (environment, cluster_id, hostname_prefix). tls defaults to "auto"
// server-side; the provider always sends it explicitly.
type IngressSpec struct {
	ClusterID      string `json:"cluster_id"`
	Service        string `json:"service"`
	Port           int64  `json:"port"`
	TLS            string `json:"tls,omitempty"`
	HostnamePrefix string `json:"hostname_prefix"`
}

// Egress mirrors the stable contract fields of the Egress schema.
type Egress struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// EgressSpec is the create payload (EgressSpec schema). The idempotency key is
// (environment, cluster_id, destination_cidr, protocol, port_range). protocol
// defaults to "any" server-side; the provider always sends it explicitly.
// PortRange is a pointer so the unset case serializes as JSON null (only valid
// for tcp/udp).
type EgressSpec struct {
	ClusterID       string  `json:"cluster_id"`
	DestinationCIDR string  `json:"destination_cidr"`
	Protocol        string  `json:"protocol,omitempty"`
	PortRange       *string `json:"port_range,omitempty"`
}

func ingressCollectionPath(envID string) string {
	return "/v1/environments/" + url.PathEscape(envID) + "/ingress"
}

func ingressItemPath(envID, ingressID string) string {
	return ingressCollectionPath(envID) + "/" + url.PathEscape(ingressID)
}

func egressCollectionPath(envID string) string {
	return "/v1/environments/" + url.PathEscape(envID) + "/egress"
}

func egressItemPath(envID, egressID string) string {
	return egressCollectionPath(envID) + "/" + url.PathEscape(egressID)
}

// CreateIngress POSTs /v1/environments/{id}/ingress. 202 (newly created) and
// 200 (idempotent re-apply over (environment, cluster_id, hostname_prefix))
// are both success. 409 (cluster not business/active or without a public IP)
// and 404 (environment/cluster unknown, foreign tenant or terminal) surface as
// APIError carrying the contract detail.
func (c *Client) CreateIngress(ctx context.Context, envID string, spec IngressSpec) (*Ingress, error) {
	status, body, err := c.do(ctx, http.MethodPost, ingressCollectionPath(envID), spec)
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusAccepted, http.StatusOK:
		var ing Ingress
		if err := json.Unmarshal(body, &ing); err != nil {
			return nil, fmt.Errorf("decoding ingress response (HTTP %d): %w", status, err)
		}
		if ing.ID == "" {
			return nil, fmt.Errorf("ingress create returned HTTP %d without an ingress id", status)
		}
		return &ing, nil
	default:
		return nil, apiError(status, body)
	}
}

// GetIngress GETs /v1/environments/{id}/ingress/{ingressId}. A 404 (unknown,
// reaped or foreign tenant) is returned as an APIError for which IsNotFound is
// true.
func (c *Client) GetIngress(ctx context.Context, envID, ingressID string) (*Ingress, error) {
	status, body, err := c.do(ctx, http.MethodGet, ingressItemPath(envID, ingressID), nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, apiError(status, body)
	}
	var ing Ingress
	if err := json.Unmarshal(body, &ing); err != nil {
		return nil, fmt.Errorf("decoding ingress: %w", err)
	}
	return &ing, nil
}

// DeleteIngress DELETEs /v1/environments/{id}/ingress/{ingressId}. 202
// (teardown started) and 404 (already gone) both count as success.
func (c *Client) DeleteIngress(ctx context.Context, envID, ingressID string) error {
	status, body, err := c.do(ctx, http.MethodDelete, ingressItemPath(envID, ingressID), nil)
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

// CreateEgress POSTs /v1/environments/{id}/egress. 202 (newly created) and 200
// (idempotent re-apply over (environment, cluster_id, destination_cidr,
// protocol, port_range)) are both success. 422 (invalid/missing
// destination_cidr) and 404 (environment/cluster unknown or foreign tenant)
// surface as APIError carrying the contract detail.
func (c *Client) CreateEgress(ctx context.Context, envID string, spec EgressSpec) (*Egress, error) {
	status, body, err := c.do(ctx, http.MethodPost, egressCollectionPath(envID), spec)
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusAccepted, http.StatusOK:
		var eg Egress
		if err := json.Unmarshal(body, &eg); err != nil {
			return nil, fmt.Errorf("decoding egress response (HTTP %d): %w", status, err)
		}
		if eg.ID == "" {
			return nil, fmt.Errorf("egress create returned HTTP %d without an egress id", status)
		}
		return &eg, nil
	default:
		return nil, apiError(status, body)
	}
}

// GetEgress GETs /v1/environments/{id}/egress/{egressId}. A 404 (unknown,
// reaped or foreign tenant) is returned as an APIError for which IsNotFound is
// true.
func (c *Client) GetEgress(ctx context.Context, envID, egressID string) (*Egress, error) {
	status, body, err := c.do(ctx, http.MethodGet, egressItemPath(envID, egressID), nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, apiError(status, body)
	}
	var eg Egress
	if err := json.Unmarshal(body, &eg); err != nil {
		return nil, fmt.Errorf("decoding egress: %w", err)
	}
	return &eg, nil
}

// DeleteEgress DELETEs /v1/environments/{id}/egress/{egressId}. 202 (teardown
// started) and 404 (already gone) both count as success.
func (c *Client) DeleteEgress(ctx context.Context, envID, egressID string) error {
	status, body, err := c.do(ctx, http.MethodDelete, egressItemPath(envID, egressID), nil)
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
