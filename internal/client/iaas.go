package client

// IaaS-vDC sub-API of the FCS API:
//
//	/v1/environments/{id}/iaas-vdcs[/{vdcId}]
//	/v1/environments/{id}/iaas-vdcs/{vdcId}/networks[/{networkId}]
//
// The API owns infrastructure details. The provider only persists stable API
// fields and treats server-side infrastructure state as computed.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// IaasVdc mirrors the stable IaaS-vDC contract.
type IaasVdc struct {
	ID             string `json:"id"`
	EnvironmentID  string `json:"environment_id"`
	Name           string `json:"name"`
	Status         string `json:"status"`
	IaasVdcSeq     int64  `json:"iaas_vdc_seq"`
	GatewayScope   string `json:"gateway_scope"`
	ScopeKey       string `json:"scope_key"`
	GatewayVPCName string `json:"gateway_vpc_name"`
	GatewayName    string `json:"gateway_name"`
	CreatedAt      string `json:"created_at"`
}

// IaasVdcSpec is the create payload.
type IaasVdcSpec struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels,omitempty"`
}

// IaasNetwork mirrors the stable IaaS-vDC network contract. Nullable API
// strings decode as empty strings; nullable vlan_id is kept as a pointer.
type IaasNetwork struct {
	ID                 string `json:"id"`
	IaasVdcID          string `json:"iaas_vdc_id"`
	EnvironmentID      string `json:"environment_id"`
	Name               string `json:"name"`
	CIDR               string `json:"cidr"`
	VLANID             *int64 `json:"vlan_id"`
	HarvesterNamespace string `json:"harvester_namespace"`
	HarvesterNADName   string `json:"harvester_nad_name"`
	KubeovnSubnetName  string `json:"kubeovn_subnet_name"`
	DefaultGatewayIP   string `json:"default_gateway_ip"`
	PolicyDefault      string `json:"policy_default"`
	Status             string `json:"status"`
	CreatedAt          string `json:"created_at"`
}

// IaasNetworkSpec is the create payload.
type IaasNetworkSpec struct {
	Name          string `json:"name"`
	CIDR          string `json:"cidr"`
	PolicyDefault string `json:"policy_default,omitempty"`
}

func iaasVdcCollectionPath(envID string) string {
	return "/v1/environments/" + url.PathEscape(envID) + "/iaas-vdcs"
}

func iaasVdcItemPath(envID, vdcID string) string {
	return iaasVdcCollectionPath(envID) + "/" + url.PathEscape(vdcID)
}

func iaasNetworkCollectionPath(envID, vdcID string) string {
	return iaasVdcItemPath(envID, vdcID) + "/networks"
}

func iaasNetworkItemPath(envID, vdcID, networkID string) string {
	return iaasNetworkCollectionPath(envID, vdcID) + "/" + url.PathEscape(networkID)
}

// CreateIaasVdc POSTs /v1/environments/{id}/iaas-vdcs. 202 and idempotent
// 200 are both success.
func (c *Client) CreateIaasVdc(ctx context.Context, envID string, spec IaasVdcSpec) (*IaasVdc, error) {
	status, body, err := c.do(ctx, http.MethodPost, iaasVdcCollectionPath(envID), spec)
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusAccepted, http.StatusOK:
		var vdc IaasVdc
		if err := json.Unmarshal(body, &vdc); err != nil {
			return nil, fmt.Errorf("decoding iaas vdc response (HTTP %d): %w", status, err)
		}
		if vdc.ID == "" {
			return nil, fmt.Errorf("iaas vdc create returned HTTP %d without a vdc id", status)
		}
		return &vdc, nil
	default:
		return nil, apiError(status, body)
	}
}

// GetIaasVdc GETs /v1/environments/{id}/iaas-vdcs/{vdcId}.
func (c *Client) GetIaasVdc(ctx context.Context, envID, vdcID string) (*IaasVdc, error) {
	status, body, err := c.do(ctx, http.MethodGet, iaasVdcItemPath(envID, vdcID), nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, apiError(status, body)
	}
	var vdc IaasVdc
	if err := json.Unmarshal(body, &vdc); err != nil {
		return nil, fmt.Errorf("decoding iaas vdc: %w", err)
	}
	return &vdc, nil
}

// DeleteIaasVdc DELETEs /v1/environments/{id}/iaas-vdcs/{vdcId}. 202 and
// 404 both count as success.
func (c *Client) DeleteIaasVdc(ctx context.Context, envID, vdcID string) error {
	status, body, err := c.do(ctx, http.MethodDelete, iaasVdcItemPath(envID, vdcID), nil)
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

// CreateIaasNetwork POSTs an IaaS-vDC network. 202 and idempotent 200 are
// both success.
func (c *Client) CreateIaasNetwork(ctx context.Context, envID, vdcID string, spec IaasNetworkSpec) (*IaasNetwork, error) {
	status, body, err := c.do(ctx, http.MethodPost, iaasNetworkCollectionPath(envID, vdcID), spec)
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusAccepted, http.StatusOK:
		var network IaasNetwork
		if err := json.Unmarshal(body, &network); err != nil {
			return nil, fmt.Errorf("decoding iaas network response (HTTP %d): %w", status, err)
		}
		if network.ID == "" {
			return nil, fmt.Errorf("iaas network create returned HTTP %d without a network id", status)
		}
		return &network, nil
	default:
		return nil, apiError(status, body)
	}
}

// GetIaasNetwork GETs one IaaS-vDC network.
func (c *Client) GetIaasNetwork(ctx context.Context, envID, vdcID, networkID string) (*IaasNetwork, error) {
	status, body, err := c.do(ctx, http.MethodGet, iaasNetworkItemPath(envID, vdcID, networkID), nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, apiError(status, body)
	}
	var network IaasNetwork
	if err := json.Unmarshal(body, &network); err != nil {
		return nil, fmt.Errorf("decoding iaas network: %w", err)
	}
	return &network, nil
}

// DeleteIaasNetwork DELETEs one IaaS-vDC network. 202 and 404 both count as
// success.
func (c *Client) DeleteIaasNetwork(ctx context.Context, envID, vdcID, networkID string) error {
	status, body, err := c.do(ctx, http.MethodDelete, iaasNetworkItemPath(envID, vdcID, networkID), nil)
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
