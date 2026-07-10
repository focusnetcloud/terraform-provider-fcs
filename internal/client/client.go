// Package client implements the HTTP client for the tenant-scoped FCS API.
//
// The client never logs or echoes the bearer token.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	defaultTimeout   = 30 * time.Second
	defaultUserAgent = "terraform-provider-fcs"
)

// Environment mirrors the stable API fields of the Environment schema.
// Read endpoints only return stable API fields.
type Environment struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Status       string `json:"status"`
	TTLExpiresAt string `json:"ttl_expires_at"`
	CreatedAt    string `json:"created_at"`
}

// EnvironmentSpec is the create payload (EnvironmentSpec schema).
type EnvironmentSpec struct {
	Name       string            `json:"name"`
	TTLSeconds int64             `json:"ttl_seconds,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
}

// apiErrorBody is the Error schema of the contract.
type apiErrorBody struct {
	Detail string `json:"detail"`
	Reason string `json:"reason"`
}

// APIError is returned for every non-success response. It carries the
// machine-readable contract fields so callers can build precise diagnostics.
type APIError struct {
	StatusCode int
	Detail     string
	Reason     string
}

func (e *APIError) Error() string {
	var b strings.Builder
	switch e.StatusCode {
	case http.StatusUnauthorized:
		b.WriteString("authentication failed (HTTP 401): the API rejected the bearer token; check the provider token / FCS_TOKEN")
	case http.StatusForbidden:
		b.WriteString("permission denied (HTTP 403): the token is valid but not allowed to perform this operation")
	case http.StatusNotFound:
		b.WriteString("not found (HTTP 404): the resource does not exist or belongs to another tenant")
	case http.StatusConflict:
		b.WriteString("conflict (HTTP 409): quota exhausted or conflicting resource")
	case http.StatusTooManyRequests:
		b.WriteString("rate limited (HTTP 429): environment create rate exceeded (create_rate token bucket), retry later")
	default:
		fmt.Fprintf(&b, "unexpected API response (HTTP %d)", e.StatusCode)
	}
	if e.Detail != "" {
		b.WriteString(": ")
		b.WriteString(e.Detail)
	}
	if e.Reason != "" {
		fmt.Fprintf(&b, " (reason: %s)", e.Reason)
	}
	return b.String()
}

// IsNotFound reports whether err is an APIError with HTTP status 404.
func IsNotFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
}

// Option customizes the client.
type Option func(*Client)

// WithHTTPClient replaces the underlying *http.Client (e.g. for tests).
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) { c.http = h }
}

// WithUserAgent sets the User-Agent header (e.g. including provider version).
func WithUserAgent(ua string) Option {
	return func(c *Client) { c.userAgent = ua }
}

// Client talks to the FCS API.
type Client struct {
	endpoint  string
	token     string
	userAgent string
	http      *http.Client
}

// New validates endpoint/token and returns a ready client.
func New(endpoint, token string, opts ...Option) (*Client, error) {
	endpoint = strings.TrimRight(strings.TrimSpace(endpoint), "/")
	if endpoint == "" {
		return nil, errors.New("endpoint must not be empty")
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, errors.New("endpoint is not a valid http(s) URL")
	}
	if u.User != nil {
		return nil, errors.New("endpoint must not contain embedded credentials")
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("endpoint is not a valid http(s) URL")
	}
	if strings.ContainsAny(endpoint, "?#") {
		return nil, errors.New("endpoint must not contain a query string or fragment")
	}
	if u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
		return nil, fmt.Errorf("endpoint %q uses insecure http; use https outside localhost/loopback tests", endpoint)
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, errors.New("token must not be empty")
	}
	c := &Client{
		endpoint:  endpoint,
		token:     token,
		userAgent: defaultUserAgent,
		http:      &http.Client{Timeout: defaultTimeout},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

func isLoopbackHost(host string) bool {
	host = strings.ToLower(strings.Trim(host, "[]"))
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// do performs a JSON request and returns status code and raw body.
func (c *Client) do(ctx context.Context, method, path string, payload any) (int, []byte, error) {
	var body io.Reader
	if payload != nil {
		buf, err := json.Marshal(payload)
		if err != nil {
			return 0, nil, fmt.Errorf("encoding request body: %w", err)
		}
		body = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint+path, body)
	if err != nil {
		return 0, nil, fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		// err may contain the URL but never the Authorization header.
		return 0, nil, fmt.Errorf("calling FCS API: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("reading API response: %w", err)
	}
	return resp.StatusCode, respBody, nil
}

// apiError builds an *APIError from a non-success response body.
func apiError(status int, body []byte) *APIError {
	e := &APIError{StatusCode: status}
	var parsed apiErrorBody
	if json.Unmarshal(body, &parsed) == nil {
		e.Detail = parsed.Detail
		e.Reason = parsed.Reason
	}
	return e
}

// CreateEnvironment POSTs /v1/environments. Both 202 (newly created; the
// server provisions environments synchronously today, so the status may
// already be active) and 200 (idempotent re-apply: the environment with
// this name already exists for the tenant) are success responses.
//
// The 200 response carries no declared content schema in the contract; the
// server is expected to return the existing Environment for an idempotent
// re-apply. If the body is empty or lacks an id, the client falls back
// to listing environments and matching by name.
func (c *Client) CreateEnvironment(ctx context.Context, spec EnvironmentSpec) (*Environment, error) {
	status, body, err := c.do(ctx, http.MethodPost, "/v1/environments", spec)
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusAccepted, http.StatusOK:
		var env Environment
		if len(body) > 0 {
			if err := json.Unmarshal(body, &env); err != nil {
				return nil, fmt.Errorf("decoding environment response (HTTP %d): %w", status, err)
			}
		}
		if env.ID != "" {
			return &env, nil
		}
		// Contract gap: 200 without body — resolve via list + name match.
		envs, err := c.ListEnvironments(ctx)
		if err != nil {
			return nil, fmt.Errorf("create returned HTTP %d without environment body, list fallback failed: %w", status, err)
		}
		for i := range envs {
			if envs[i].Name == spec.Name {
				return &envs[i], nil
			}
		}
		return nil, fmt.Errorf("create returned HTTP %d without environment body and no environment named %q is listed", status, spec.Name)
	default:
		return nil, apiError(status, body)
	}
}

// ListEnvironments GETs /v1/environments (tenant-scoped).
func (c *Client) ListEnvironments(ctx context.Context) ([]Environment, error) {
	status, body, err := c.do(ctx, http.MethodGet, "/v1/environments", nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, apiError(status, body)
	}
	var envs []Environment
	if err := json.Unmarshal(body, &envs); err != nil {
		return nil, fmt.Errorf("decoding environment list: %w", err)
	}
	return envs, nil
}

// GetEnvironment GETs /v1/environments/{id}. A 404 (unknown or foreign
// tenant) is returned as an APIError for which IsNotFound is true.
func (c *Client) GetEnvironment(ctx context.Context, id string) (*Environment, error) {
	status, body, err := c.do(ctx, http.MethodGet, "/v1/environments/"+url.PathEscape(id), nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, apiError(status, body)
	}
	var env Environment
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("decoding environment: %w", err)
	}
	return &env, nil
}

// DeleteEnvironment DELETEs /v1/environments/{id}. 202 (teardown started)
// and 404 (already gone — e.g. the TTL reaper was faster; terraform destroy
// is advisory) both count as success.
func (c *Client) DeleteEnvironment(ctx context.Context, id string) error {
	status, body, err := c.do(ctx, http.MethodDelete, "/v1/environments/"+url.PathEscape(id), nil)
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
