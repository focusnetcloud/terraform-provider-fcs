package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// HarborRobotAccount contains only the stable tenant-facing registry fields.
// Secret is populated exclusively by Create and Rotate responses.
type HarborRobotAccount struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	Description       string `json:"description"`
	PermissionProfile string `json:"permission_profile"`
	ExpiresInDays     int64  `json:"expires_in_days"`
	Disabled          bool   `json:"disabled"`
	Status            string `json:"status"`
	Project           string `json:"project"`
	Registry          string `json:"registry"`
	Username          string `json:"username"`
	Secret            string `json:"secret,omitempty"`
	ExpiresAt         string `json:"expires_at"`
	CreatedAt         string `json:"created_at"`
	UpdatedAt         string `json:"updated_at"`
}

// HarborRobotAccountSpec is the create payload. The project and permissions
// are derived and constrained server-side from the tenant token.
type HarborRobotAccountSpec struct {
	Name              string `json:"name"`
	Description       string `json:"description"`
	PermissionProfile string `json:"permission_profile"`
	ExpiresInDays     int64  `json:"expires_in_days"`
	Disabled          bool   `json:"disabled"`
}

// HarborRobotAccountUpdate is a full desired update payload. The resource
// always knows every configurable field, so pointers/partial ambiguity are
// unnecessary.
type HarborRobotAccountUpdate struct {
	Description       string `json:"description"`
	PermissionProfile string `json:"permission_profile"`
	ExpiresInDays     int64  `json:"expires_in_days"`
	Disabled          bool   `json:"disabled"`
}

// HarborArtifact is an FCS registration of an immutable digest that already
// exists in the tenant's Harbor project. Deleting it does not delete OCI data.
type HarborArtifact struct {
	ID                 string `json:"id"`
	Repository         string `json:"repository"`
	Reference          string `json:"reference"`
	Kind               string `json:"kind"`
	Project            string `json:"project"`
	Registry           string `json:"registry"`
	Digest             string `json:"digest"`
	CanonicalReference string `json:"canonical_reference"`
	MediaType          string `json:"media_type"`
	SizeBytes          int64  `json:"size_bytes"`
	Status             string `json:"status"`
	CreatedAt          string `json:"created_at"`
	UpdatedAt          string `json:"updated_at"`
}

// HarborArtifactSpec identifies an existing tag or digest. The API derives
// the tenant project and resolves a tag to its immutable digest.
type HarborArtifactSpec struct {
	Repository string `json:"repository"`
	Reference  string `json:"reference"`
	Kind       string `json:"kind"`
}

// HarborRegistryBinding is secret-free metadata for a server-managed pull
// credential installed in a tenant cluster namespace.
type HarborRegistryBinding struct {
	ID              string   `json:"id"`
	EnvironmentID   string   `json:"environment_id"`
	ClusterID       string   `json:"cluster_id"`
	Namespace       string   `json:"namespace"`
	SecretName      string   `json:"secret_name"`
	ServiceAccounts []string `json:"service_accounts"`
	Project         string   `json:"project"`
	Registry        string   `json:"registry"`
	Status          string   `json:"status"`
	CreatedAt       string   `json:"created_at"`
	UpdatedAt       string   `json:"updated_at"`
}

// HarborRegistryBindingSpec identifies an existing tenant-owned cluster
// namespace. The API creates and stores the pull credential only in Kubernetes.
type HarborRegistryBindingSpec struct {
	EnvironmentID   string   `json:"environment_id"`
	ClusterID       string   `json:"cluster_id"`
	Namespace       string   `json:"namespace"`
	SecretName      string   `json:"secret_name"`
	ServiceAccounts []string `json:"service_accounts"`
}

// HarborRegistryBindingUpdate replaces the desired ServiceAccount set.
type HarborRegistryBindingUpdate struct {
	ServiceAccounts []string `json:"service_accounts"`
}

func harborRobotCollectionPath() string {
	return "/v1/registry/robot-accounts"
}

func harborRobotItemPath(id string) string {
	return harborRobotCollectionPath() + "/" + url.PathEscape(id)
}

func harborArtifactCollectionPath() string {
	return "/v1/registry/artifacts"
}

func harborArtifactItemPath(id string) string {
	return harborArtifactCollectionPath() + "/" + url.PathEscape(id)
}

func harborRegistryBindingCollectionPath() string {
	return "/v1/registry/bindings"
}

func harborRegistryBindingItemPath(id string) string {
	return harborRegistryBindingCollectionPath() + "/" + url.PathEscape(id)
}

func decodeHarborRobot(body []byte, operation string) (*HarborRobotAccount, error) {
	var robot HarborRobotAccount
	if err := json.Unmarshal(body, &robot); err != nil {
		return nil, fmt.Errorf("decoding Harbor robot account %s response: %w", operation, err)
	}
	if robot.ID == "" {
		return nil, fmt.Errorf("Harbor robot account %s response has no id", operation)
	}
	return &robot, nil
}

func decodeHarborArtifact(body []byte, operation string) (*HarborArtifact, error) {
	var artifact HarborArtifact
	if err := json.Unmarshal(body, &artifact); err != nil {
		return nil, fmt.Errorf("decoding Harbor artifact %s response: %w", operation, err)
	}
	if artifact.ID == "" {
		return nil, fmt.Errorf("Harbor artifact %s response has no id", operation)
	}
	if artifact.Digest == "" {
		return nil, fmt.Errorf("Harbor artifact %s response has no digest", operation)
	}
	return &artifact, nil
}

func decodeHarborRegistryBinding(
	body []byte,
	operation string,
) (*HarborRegistryBinding, error) {
	var binding HarborRegistryBinding
	if err := json.Unmarshal(body, &binding); err != nil {
		return nil, fmt.Errorf("decoding Harbor registry binding %s response: %w", operation, err)
	}
	if binding.ID == "" {
		return nil, fmt.Errorf("Harbor registry binding %s response has no id", operation)
	}
	return &binding, nil
}

// CreateHarborRobotAccount creates a project-scoped Robot Account. The
// returned Secret is one-time credential material and must be treated as
// sensitive by callers.
func (c *Client) CreateHarborRobotAccount(
	ctx context.Context,
	spec HarborRobotAccountSpec,
) (*HarborRobotAccount, error) {
	status, body, err := c.do(ctx, http.MethodPost, harborRobotCollectionPath(), spec)
	if err != nil {
		return nil, err
	}
	if status != http.StatusCreated {
		return nil, apiError(status, body)
	}
	robot, err := decodeHarborRobot(body, "create")
	if err != nil {
		return nil, err
	}
	if robot.Secret == "" {
		return nil, fmt.Errorf("Harbor robot account create response has no one-time secret")
	}
	return robot, nil
}

// GetHarborRobotAccount reads secret-free account metadata.
func (c *Client) GetHarborRobotAccount(
	ctx context.Context,
	id string,
) (*HarborRobotAccount, error) {
	status, body, err := c.do(ctx, http.MethodGet, harborRobotItemPath(id), nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, apiError(status, body)
	}
	return decodeHarborRobot(body, "read")
}

// UpdateHarborRobotAccount updates mutable account metadata and permissions
// in place. The response never contains credential material.
func (c *Client) UpdateHarborRobotAccount(
	ctx context.Context,
	id string,
	update HarborRobotAccountUpdate,
) (*HarborRobotAccount, error) {
	status, body, err := c.do(ctx, http.MethodPatch, harborRobotItemPath(id), update)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, apiError(status, body)
	}
	return decodeHarborRobot(body, "update")
}

// RotateHarborRobotAccount rotates the Secret and returns it once.
func (c *Client) RotateHarborRobotAccount(
	ctx context.Context,
	id string,
) (*HarborRobotAccount, error) {
	status, body, err := c.do(
		ctx,
		http.MethodPost,
		harborRobotItemPath(id)+"/rotations",
		nil,
	)
	if err != nil {
		return nil, err
	}
	if status != http.StatusCreated {
		return nil, apiError(status, body)
	}
	robot, err := decodeHarborRobot(body, "rotation")
	if err != nil {
		return nil, err
	}
	if robot.Secret == "" {
		return nil, fmt.Errorf("Harbor robot account rotation response has no one-time secret")
	}
	return robot, nil
}

// DeleteHarborRobotAccount revokes an account. 404 is idempotent because the
// resource is already absent from the tenant's active view.
func (c *Client) DeleteHarborRobotAccount(ctx context.Context, id string) error {
	status, body, err := c.do(ctx, http.MethodDelete, harborRobotItemPath(id), nil)
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

// CreateHarborArtifact registers an existing tag or digest. HTTP 200 is the
// idempotent response for a digest that was already registered.
func (c *Client) CreateHarborArtifact(
	ctx context.Context,
	spec HarborArtifactSpec,
) (*HarborArtifact, error) {
	status, body, err := c.do(ctx, http.MethodPost, harborArtifactCollectionPath(), spec)
	if err != nil {
		return nil, err
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return nil, apiError(status, body)
	}
	return decodeHarborArtifact(body, "create")
}

// GetHarborArtifact reads the registration and asks the API to verify that its
// digest is still available in the tenant project.
func (c *Client) GetHarborArtifact(
	ctx context.Context,
	id string,
) (*HarborArtifact, error) {
	status, body, err := c.do(ctx, http.MethodGet, harborArtifactItemPath(id), nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, apiError(status, body)
	}
	return decodeHarborArtifact(body, "read")
}

// DeleteHarborArtifact removes only the FCS registration. 404 is idempotent
// from Terraform's perspective because the managed registration is absent.
func (c *Client) DeleteHarborArtifact(ctx context.Context, id string) error {
	status, body, err := c.do(ctx, http.MethodDelete, harborArtifactItemPath(id), nil)
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

// CreateHarborRegistryBinding installs a server-managed pull credential. HTTP
// 200 is the idempotent response for an identical active binding.
func (c *Client) CreateHarborRegistryBinding(
	ctx context.Context,
	spec HarborRegistryBindingSpec,
) (*HarborRegistryBinding, error) {
	status, body, err := c.do(
		ctx,
		http.MethodPost,
		harborRegistryBindingCollectionPath(),
		spec,
	)
	if err != nil {
		return nil, err
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return nil, apiError(status, body)
	}
	return decodeHarborRegistryBinding(body, "create")
}

// GetHarborRegistryBinding reads secret-free binding metadata.
func (c *Client) GetHarborRegistryBinding(
	ctx context.Context,
	id string,
) (*HarborRegistryBinding, error) {
	status, body, err := c.do(
		ctx,
		http.MethodGet,
		harborRegistryBindingItemPath(id),
		nil,
	)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, apiError(status, body)
	}
	return decodeHarborRegistryBinding(body, "read")
}

// UpdateHarborRegistryBinding reconciles the desired ServiceAccount set.
func (c *Client) UpdateHarborRegistryBinding(
	ctx context.Context,
	id string,
	update HarborRegistryBindingUpdate,
) (*HarborRegistryBinding, error) {
	status, body, err := c.do(
		ctx,
		http.MethodPatch,
		harborRegistryBindingItemPath(id),
		update,
	)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, apiError(status, body)
	}
	return decodeHarborRegistryBinding(body, "update")
}

// RotateHarborRegistryBinding rotates the credential entirely server-side.
func (c *Client) RotateHarborRegistryBinding(
	ctx context.Context,
	id string,
) (*HarborRegistryBinding, error) {
	status, body, err := c.do(
		ctx,
		http.MethodPost,
		harborRegistryBindingItemPath(id)+"/rotations",
		nil,
	)
	if err != nil {
		return nil, err
	}
	if status != http.StatusCreated {
		return nil, apiError(status, body)
	}
	return decodeHarborRegistryBinding(body, "rotation")
}

// DeleteHarborRegistryBinding removes only resources owned by the binding.
func (c *Client) DeleteHarborRegistryBinding(ctx context.Context, id string) error {
	status, body, err := c.do(
		ctx,
		http.MethodDelete,
		harborRegistryBindingItemPath(id),
		nil,
	)
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
