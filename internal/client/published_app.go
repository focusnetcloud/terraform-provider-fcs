package client

// Published App sub-API:
//
//	/v1/k8s/namespaces/{namespaceId}/published-apps[/{appId}]
//
// The namespaceId is the ID returned by fcs_namespace or fcs_flex_cluster. This
// API publishes an in-cluster Service through the tenant-scoped public app
// gateway and returns the stable generated hostname.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// PublishedApp mirrors the public API response for a Free/Flex published app.
// The endpoint contract defines id as an integer (PublishedAppOut.id).
type PublishedApp struct {
	ID                int64  `json:"id"`
	Hostname          string `json:"hostname"`
	AppSlug           string `json:"app_slug"`
	ServiceName       string `json:"service_name"`
	ServicePort       int64  `json:"service_port"`
	VclusterNamespace string `json:"vcluster_namespace"`
	PathPrefix        string `json:"path_prefix"`
	TLSMode           string `json:"tls_mode"`
}

// PublishedAppSpec is the create payload. The idempotency key is the generated
// hostname: app_slug plus the namespace tier domain. PathPrefix is a pointer so
// unset serializes as an omitted field instead of an empty string.
type PublishedAppSpec struct {
	AppSlug           string  `json:"app_slug"`
	ServiceName       string  `json:"service_name"`
	ServicePort       int64   `json:"service_port"`
	VclusterNamespace string  `json:"vcluster_namespace,omitempty"`
	PathPrefix        *string `json:"path_prefix,omitempty"`
}

func publishedAppCollectionPath(namespaceID string) string {
	return "/v1/k8s/namespaces/" + url.PathEscape(namespaceID) + "/published-apps"
}

func publishedAppItemPath(namespaceID string, appID int64) string {
	return publishedAppCollectionPath(namespaceID) + "/" + strconv.FormatInt(appID, 10)
}

// CreatePublishedApp POSTs a published app intent. 201 (new) and 200
// (idempotent re-apply over the same hostname and target) are success.
func (c *Client) CreatePublishedApp(ctx context.Context, namespaceID string, spec PublishedAppSpec) (*PublishedApp, error) {
	status, body, err := c.do(ctx, http.MethodPost, publishedAppCollectionPath(namespaceID), spec)
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusCreated, http.StatusOK:
		var app PublishedApp
		if err := json.Unmarshal(body, &app); err != nil {
			return nil, fmt.Errorf("decoding published app response (HTTP %d): %w", status, err)
		}
		if app.ID == 0 {
			return nil, fmt.Errorf("published app create returned HTTP %d without an app id", status)
		}
		return &app, nil
	default:
		return nil, apiError(status, body)
	}
}

// ListPublishedApps lists live published app intents for a namespace.
func (c *Client) ListPublishedApps(ctx context.Context, namespaceID string) ([]PublishedApp, error) {
	status, body, err := c.do(ctx, http.MethodGet, publishedAppCollectionPath(namespaceID), nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, apiError(status, body)
	}
	var apps []PublishedApp
	if err := json.Unmarshal(body, &apps); err != nil {
		return nil, fmt.Errorf("decoding published app list: %w", err)
	}
	return apps, nil
}

// GetPublishedApp GETs a single published app. A 404 is returned as an
// APIError for which IsNotFound is true.
func (c *Client) GetPublishedApp(ctx context.Context, namespaceID string, appID int64) (*PublishedApp, error) {
	status, body, err := c.do(ctx, http.MethodGet, publishedAppItemPath(namespaceID, appID), nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, apiError(status, body)
	}
	var app PublishedApp
	if err := json.Unmarshal(body, &app); err != nil {
		return nil, fmt.Errorf("decoding published app: %w", err)
	}
	return &app, nil
}

// DeletePublishedApp DELETEs a published app. 204 and 404 both count as
// success: the API deletes synchronously or reports it already gone.
func (c *Client) DeletePublishedApp(ctx context.Context, namespaceID string, appID int64) error {
	status, body, err := c.do(ctx, http.MethodDelete, publishedAppItemPath(namespaceID, appID), nil)
	if err != nil {
		return err
	}
	switch status {
	case http.StatusNoContent, http.StatusNotFound:
		return nil
	default:
		return apiError(status, body)
	}
}
