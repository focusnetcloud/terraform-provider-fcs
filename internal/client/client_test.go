package client_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/focusnetcloud/terraform-provider-fcs/internal/client"
	"github.com/focusnetcloud/terraform-provider-fcs/internal/mockapi"
)

const testToken = "fcs_test_token"

func newTestClient(t *testing.T, endpoint, token string) *client.Client {
	t.Helper()
	c, err := client.New(endpoint, token)
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	return c
}

func TestNewValidation(t *testing.T) {
	cases := []struct {
		name     string
		endpoint string
		token    string
	}{
		{"empty endpoint", "", testToken},
		{"invalid scheme", "ftp://api.example.com", testToken},
		{"no host", "https://", testToken},
		{"empty token", "https://api.example.com", ""},
		{"whitespace token", "https://api.example.com", "   "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := client.New(tc.endpoint, tc.token); err == nil {
				t.Fatalf("expected error for endpoint=%q token=%q", tc.endpoint, tc.token)
			}
		})
	}
}

func TestNewRejectsInsecureNonLoopbackHTTP(t *testing.T) {
	for _, endpoint := range []string{
		"http://api.example.com",
		"http://192.0.2.10:8080",
		"http://10.0.0.5",
		"http://127.evil.com",
		"http://127notanip.example.com",
		"http://[2001:db8::1]:8080",
	} {
		t.Run(endpoint, func(t *testing.T) {
			if _, err := client.New(endpoint, testToken); err == nil {
				t.Fatal("expected http endpoint outside loopback to be rejected")
			}
		})
	}
}

func TestNewAllowsLoopbackHTTPForTests(t *testing.T) {
	for _, endpoint := range []string{
		"http://localhost:8080",
		"http://127.0.0.1:8080",
		"http://127.10.20.30:8080",
		"http://[::1]:8080",
		"http://[0:0:0:0:0:0:0:1]:8080",
	} {
		t.Run(endpoint, func(t *testing.T) {
			if _, err := client.New(endpoint, testToken); err != nil {
				t.Fatalf("expected loopback http endpoint to be accepted: %v", err)
			}
		})
	}
}

func TestNewTrimsBearerToken(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/environments", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+testToken {
			t.Fatalf("unexpected Authorization header %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := newTestClient(t, srv.URL, "  "+testToken+" \n")

	if _, err := c.ListEnvironments(context.Background()); err != nil {
		t.Fatalf("ListEnvironments: %v", err)
	}
}

func TestCreateEnvironmentNew(t *testing.T) {
	srv := mockapi.New(testToken)
	defer srv.Close()
	c := newTestClient(t, srv.URL, testToken)

	env, err := c.CreateEnvironment(context.Background(), client.EnvironmentSpec{
		Name:       "lab-alice",
		TTLSeconds: 3600,
		Labels:     map[string]string{"workshop": "k8s"},
	})
	if err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}
	if env.ID == "" {
		t.Fatal("expected non-empty id")
	}
	if env.Name != "lab-alice" {
		t.Fatalf("expected name lab-alice, got %q", env.Name)
	}
	// Environments are provisioned synchronously: active right away.
	if env.Status != "active" {
		t.Fatalf("expected status active, got %q", env.Status)
	}
	if env.TTLExpiresAt == "" || env.CreatedAt == "" {
		t.Fatalf("expected ttl_expires_at and created_at to be set, got %+v", env)
	}
}

func TestCreateEnvironmentDecodesNullTTLExpiresAt(t *testing.T) {
	// A persistent environment: the server returns ttl_expires_at: null (no ttl).
	// The client must decode JSON null into the empty string so the provider's
	// applyEnvironment maps it to a null state attribute.
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/environments", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"id":"11111111-2222-4333-8444-555555555555","name":"prod","status":"active","ttl_expires_at":null,"created_at":"2026-06-15T10:00:00Z"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := newTestClient(t, srv.URL, testToken)

	env, err := c.CreateEnvironment(context.Background(), client.EnvironmentSpec{Name: "prod"})
	if err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}
	if env.TTLExpiresAt != "" {
		t.Fatalf("ttl_expires_at: null must decode to empty string, got %q", env.TTLExpiresAt)
	}
}

func TestCreateEnvironmentIdempotentReapply(t *testing.T) {
	srv := mockapi.New(testToken)
	defer srv.Close()
	c := newTestClient(t, srv.URL, testToken)

	first, err := c.CreateEnvironment(context.Background(), client.EnvironmentSpec{Name: "lab-bob"})
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	second, err := c.CreateEnvironment(context.Background(), client.EnvironmentSpec{Name: "lab-bob"})
	if err != nil {
		t.Fatalf("idempotent re-apply must succeed (HTTP 200), got: %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("idempotent create returned different ids: %s vs %s", first.ID, second.ID)
	}
	if srv.EnvironmentCount() != 1 {
		t.Fatalf("expected exactly 1 environment, got %d", srv.EnvironmentCount())
	}
}

// TestCreateEnvironmentIdempotent200WithoutBody covers the contract gap:
// the 200 response declares no content schema, so an empty body must be
// resolved via the list endpoint.
func TestCreateEnvironmentIdempotent200WithoutBody(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/environments", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK) // empty body
	})
	mux.HandleFunc("GET /v1/environments", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"11111111-2222-4333-8444-555555555555","name":"lab-eve","status":"active","ttl_expires_at":"2030-01-01T18:00:00Z","created_at":"2030-01-01T14:00:00Z"}]`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := newTestClient(t, srv.URL, testToken)

	env, err := c.CreateEnvironment(context.Background(), client.EnvironmentSpec{Name: "lab-eve"})
	if err != nil {
		t.Fatalf("CreateEnvironment with empty 200 body: %v", err)
	}
	if env.ID != "11111111-2222-4333-8444-555555555555" {
		t.Fatalf("expected id from list fallback, got %q", env.ID)
	}
}

func TestCreateEnvironmentQuota409(t *testing.T) {
	srv := mockapi.New(testToken)
	defer srv.Close()
	srv.MaxEnvs = 1
	c := newTestClient(t, srv.URL, testToken)

	if _, err := c.CreateEnvironment(context.Background(), client.EnvironmentSpec{Name: "lab-1"}); err != nil {
		t.Fatalf("first create: %v", err)
	}
	_, err := c.CreateEnvironment(context.Background(), client.EnvironmentSpec{Name: "lab-2"})
	if err == nil {
		t.Fatal("expected 409 quota error")
	}
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusConflict {
		t.Fatalf("expected APIError 409, got %v", err)
	}
	if apiErr.Reason != "QuotaExceeded" {
		t.Fatalf("expected reason QuotaExceeded, got %q", apiErr.Reason)
	}
	msg := err.Error()
	if !strings.Contains(msg, "409") || !strings.Contains(msg, "max_concurrent_environments") || !strings.Contains(msg, "QuotaExceeded") {
		t.Fatalf("error message must contain status, detail and reason, got: %s", msg)
	}
}

func TestUnauthorized401(t *testing.T) {
	srv := mockapi.New(testToken)
	defer srv.Close()
	c := newTestClient(t, srv.URL, "fcs_wrong_token")

	_, err := c.CreateEnvironment(context.Background(), client.EnvironmentSpec{Name: "lab-x"})
	if err == nil {
		t.Fatal("expected 401 error")
	}
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected APIError 401, got %v", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "401") || !strings.Contains(msg, "invalid or missing bearer token") {
		t.Fatalf("expected speaking 401 message with detail, got: %s", msg)
	}
	// The token must never leak into error messages.
	if strings.Contains(msg, "fcs_wrong_token") {
		t.Fatalf("error message leaks the token: %s", msg)
	}
}

func TestGetEnvironment(t *testing.T) {
	srv := mockapi.New(testToken)
	defer srv.Close()
	c := newTestClient(t, srv.URL, testToken)

	created, err := c.CreateEnvironment(context.Background(), client.EnvironmentSpec{Name: "lab-get"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	env, err := c.GetEnvironment(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("GetEnvironment: %v", err)
	}
	if env.ID != created.ID || env.Name != "lab-get" {
		t.Fatalf("unexpected environment: %+v", env)
	}
	if env.Status != "active" {
		t.Fatalf("environments are synchronously active, got %q", env.Status)
	}
}

func TestGetEnvironmentNotFound(t *testing.T) {
	srv := mockapi.New(testToken)
	defer srv.Close()
	c := newTestClient(t, srv.URL, testToken)

	_, err := c.GetEnvironment(context.Background(), "00000000-0000-4000-8000-000000000000")
	if err == nil {
		t.Fatal("expected 404 error")
	}
	if !client.IsNotFound(err) {
		t.Fatalf("IsNotFound must be true for 404, got %v", err)
	}
}

func TestDeleteEnvironment(t *testing.T) {
	srv := mockapi.New(testToken)
	defer srv.Close()
	c := newTestClient(t, srv.URL, testToken)

	created, err := c.CreateEnvironment(context.Background(), client.EnvironmentSpec{Name: "lab-del"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := c.DeleteEnvironment(context.Background(), created.ID); err != nil {
		t.Fatalf("delete (202): %v", err)
	}
	// First GET after DELETE: the row is still readable as destroyed …
	env, err := c.GetEnvironment(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("get after delete: %v", err)
	}
	if env.Status != "destroyed" {
		t.Fatalf("expected destroyed after DELETE, got %q", env.Status)
	}
	// … a repeated DELETE stays idempotent (202) …
	if err := c.DeleteEnvironment(context.Background(), created.ID); err != nil {
		t.Fatalf("repeated delete must be idempotent, got: %v", err)
	}
	// … and after the gone window the GET turns into a 404 …
	if _, err := c.GetEnvironment(context.Background(), created.ID); !client.IsNotFound(err) {
		t.Fatalf("expected 404 after gone window, got %v", err)
	}
	// … which DELETE tolerates as well (reaper-was-faster case).
	if err := c.DeleteEnvironment(context.Background(), created.ID); err != nil {
		t.Fatalf("delete must tolerate 404, got: %v", err)
	}
	if srv.EnvironmentCount() != 0 {
		t.Fatalf("expected 0 live environments, got %d", srv.EnvironmentCount())
	}
}

func TestRateLimit429Mapping(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"detail":"create rate exceeded","reason":"RateLimited"}`))
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL, testToken)

	_, err := c.CreateEnvironment(context.Background(), client.EnvironmentSpec{Name: "lab-rl"})
	if err == nil {
		t.Fatal("expected 429 error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "429") || !strings.Contains(msg, "create rate exceeded") || !strings.Contains(msg, "RateLimited") {
		t.Fatalf("expected speaking 429 message with detail+reason, got: %s", msg)
	}
}

func TestForbidden403Mapping(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"detail":"token lacks scope","reason":"Forbidden"}`))
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL, testToken)

	_, err := c.GetEnvironment(context.Background(), "some-id")
	if err == nil {
		t.Fatal("expected 403 error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "403") || !strings.Contains(msg, "token lacks scope") {
		t.Fatalf("expected speaking 403 message with detail, got: %s", msg)
	}
}

func TestAuthorizationHeaderSent(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","name":"n","status":"active","ttl_expires_at":"","created_at":""}`))
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL, testToken)

	if _, err := c.GetEnvironment(context.Background(), "x"); err != nil {
		t.Fatalf("get: %v", err)
	}
	if gotAuth != "Bearer "+testToken {
		t.Fatalf("expected bearer header, got %q", gotAuth)
	}
}
