package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newRegistryTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := New(server.URL, "fcs_registry_test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return client
}

func TestHarborRobotClientLifecycle(t *testing.T) {
	t.Parallel()
	var calls []string
	client := newRegistryTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fcs_registry_test" {
			t.Fatalf("missing tenant bearer token")
		}
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/registry/robot-accounts":
			var payload HarborRobotAccountSpec
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode create: %v", err)
			}
			if payload.Name != "pipeline" || payload.PermissionProfile != "push_pull" ||
				payload.ExpiresInDays != 90 || payload.Disabled {
				t.Fatalf("unexpected create payload: %#v", payload)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{
				"id":"11111111-2222-4333-8444-555555555555",
				"name":"pipeline",
				"description":"CI",
				"permission_profile":"push_pull",
				"expires_in_days":90,
				"disabled":false,
				"status":"active",
				"project":"t-8012",
				"registry":"harbor.cloud.focusnet.de",
				"username":"robot$t-8012+fcs-pipeline-11111111",
				"secret":"one-time-secret",
				"expires_at":"2026-10-22T08:00:00Z",
				"created_at":"2026-07-24T08:00:00Z",
				"updated_at":"2026-07-24T08:00:00Z"
			}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/registry/robot-accounts/11111111-2222-4333-8444-555555555555":
			_, _ = w.Write([]byte(`{
				"id":"11111111-2222-4333-8444-555555555555",
				"name":"pipeline",
				"description":"CI",
				"permission_profile":"push_pull",
				"expires_in_days":90,
				"disabled":false,
				"status":"active",
				"project":"t-8012",
				"registry":"harbor.cloud.focusnet.de",
				"username":"robot$t-8012+fcs-pipeline-11111111"
			}`))
		case r.Method == http.MethodPatch && r.URL.Path == "/v1/registry/robot-accounts/11111111-2222-4333-8444-555555555555":
			var payload HarborRobotAccountUpdate
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode update: %v", err)
			}
			if payload.Description != "Deploy" || payload.PermissionProfile != "pull" ||
				payload.ExpiresInDays != 30 || !payload.Disabled {
				t.Fatalf("unexpected update payload: %#v", payload)
			}
			_, _ = w.Write([]byte(`{
				"id":"11111111-2222-4333-8444-555555555555",
				"name":"pipeline",
				"description":"Deploy",
				"permission_profile":"pull",
				"expires_in_days":30,
				"disabled":true,
				"status":"active",
				"project":"t-8012",
				"registry":"harbor.cloud.focusnet.de",
				"username":"robot$t-8012+fcs-pipeline-11111111"
			}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/registry/robot-accounts/11111111-2222-4333-8444-555555555555/rotations":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{
				"id":"11111111-2222-4333-8444-555555555555",
				"name":"pipeline",
				"permission_profile":"pull",
				"expires_in_days":30,
				"disabled":true,
				"status":"active",
				"project":"t-8012",
				"registry":"harbor.cloud.focusnet.de",
				"username":"robot$t-8012+fcs-pipeline-11111111",
				"secret":"rotated-secret"
			}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/registry/robot-accounts/11111111-2222-4333-8444-555555555555":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})

	ctx := context.Background()
	created, err := client.CreateHarborRobotAccount(ctx, HarborRobotAccountSpec{
		Name:              "pipeline",
		Description:       "CI",
		PermissionProfile: "push_pull",
		ExpiresInDays:     90,
	})
	if err != nil {
		t.Fatalf("CreateHarborRobotAccount: %v", err)
	}
	if created.Secret != "one-time-secret" {
		t.Fatalf("create secret = %q", created.Secret)
	}

	read, err := client.GetHarborRobotAccount(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetHarborRobotAccount: %v", err)
	}
	if read.Secret != "" {
		t.Fatalf("GET must not return a secret, got %q", read.Secret)
	}

	updated, err := client.UpdateHarborRobotAccount(ctx, created.ID, HarborRobotAccountUpdate{
		Description:       "Deploy",
		PermissionProfile: "pull",
		ExpiresInDays:     30,
		Disabled:          true,
	})
	if err != nil {
		t.Fatalf("UpdateHarborRobotAccount: %v", err)
	}
	if updated.PermissionProfile != "pull" {
		t.Fatalf("updated profile = %q", updated.PermissionProfile)
	}

	rotated, err := client.RotateHarborRobotAccount(ctx, created.ID)
	if err != nil {
		t.Fatalf("RotateHarborRobotAccount: %v", err)
	}
	if rotated.Secret != "rotated-secret" {
		t.Fatalf("rotation secret = %q", rotated.Secret)
	}

	if err := client.DeleteHarborRobotAccount(ctx, created.ID); err != nil {
		t.Fatalf("DeleteHarborRobotAccount: %v", err)
	}
	if len(calls) != 5 {
		t.Fatalf("calls = %v", calls)
	}
}

func TestHarborRobotClientEscapesIDAndToleratesDelete404(t *testing.T) {
	t.Parallel()
	client := newRegistryTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.RequestURI, "/robot-accounts/robot%2Fid") {
			t.Fatalf("ID is not path escaped: %q", r.RequestURI)
		}
		w.WriteHeader(http.StatusNotFound)
	})

	if err := client.DeleteHarborRobotAccount(context.Background(), "robot/id"); err != nil {
		t.Fatalf("DeleteHarborRobotAccount 404: %v", err)
	}
}

func TestHarborRobotClientRejectsCreateWithoutSecret(t *testing.T) {
	t.Parallel()
	client := newRegistryTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"id","name":"pipeline"}`))
	})

	_, err := client.CreateHarborRobotAccount(context.Background(), HarborRobotAccountSpec{
		Name:              "pipeline",
		PermissionProfile: "pull",
		ExpiresInDays:     90,
	})
	if err == nil || !strings.Contains(err.Error(), "secret") {
		t.Fatalf("expected missing-secret error, got %v", err)
	}
}

func TestHarborArtifactClientLifecycle(t *testing.T) {
	t.Parallel()
	digest := "sha256:" + strings.Repeat("a", 64)
	var calls []string
	client := newRegistryTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/registry/artifacts":
			var payload HarborArtifactSpec
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode create: %v", err)
			}
			if payload.Repository != "vm-images/windows" ||
				payload.Reference != "2026-07" || payload.Kind != "vm_disk" {
				t.Fatalf("unexpected create payload: %#v", payload)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprintf(w, `{
				"id":"66666666-7777-4888-8999-aaaaaaaaaaaa",
				"repository":"vm-images/windows",
				"reference":"2026-07",
				"kind":"vm_disk",
				"project":"t-8012",
				"registry":"harbor.cloud.focusnet.de",
				"digest":%q,
				"canonical_reference":%q,
				"media_type":"application/vnd.oci.image.manifest.v1+json",
				"size_bytes":8589934592,
				"status":"active"
			}`, digest, "harbor.cloud.focusnet.de/t-8012/vm-images/windows@"+digest)
		case r.Method == http.MethodGet &&
			r.URL.Path == "/v1/registry/artifacts/66666666-7777-4888-8999-aaaaaaaaaaaa":
			_, _ = fmt.Fprintf(w, `{
				"id":"66666666-7777-4888-8999-aaaaaaaaaaaa",
				"repository":"vm-images/windows",
				"reference":"2026-07",
				"kind":"vm_disk",
				"project":"t-8012",
				"registry":"harbor.cloud.focusnet.de",
				"digest":%q,
				"canonical_reference":%q,
				"media_type":"application/vnd.oci.image.manifest.v1+json",
				"size_bytes":8589934592,
				"status":"active"
			}`, digest, "harbor.cloud.focusnet.de/t-8012/vm-images/windows@"+digest)
		case r.Method == http.MethodDelete &&
			r.URL.Path == "/v1/registry/artifacts/66666666-7777-4888-8999-aaaaaaaaaaaa":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})

	ctx := context.Background()
	created, err := client.CreateHarborArtifact(ctx, HarborArtifactSpec{
		Repository: "vm-images/windows",
		Reference:  "2026-07",
		Kind:       "vm_disk",
	})
	if err != nil {
		t.Fatalf("CreateHarborArtifact: %v", err)
	}
	if created.Digest != digest || created.SizeBytes != 8_589_934_592 {
		t.Fatalf("unexpected created artifact: %#v", created)
	}

	read, err := client.GetHarborArtifact(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetHarborArtifact: %v", err)
	}
	if read.CanonicalReference !=
		"harbor.cloud.focusnet.de/t-8012/vm-images/windows@"+digest {
		t.Fatalf("canonical reference = %q", read.CanonicalReference)
	}
	if err := client.DeleteHarborArtifact(ctx, created.ID); err != nil {
		t.Fatalf("DeleteHarborArtifact: %v", err)
	}
	if len(calls) != 3 {
		t.Fatalf("calls = %v", calls)
	}
}

func TestHarborArtifactCreateAcceptsIdempotent200(t *testing.T) {
	t.Parallel()
	client := newRegistryTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{
			"id":"66666666-7777-4888-8999-aaaaaaaaaaaa",
			"digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		}`))
	})

	if _, err := client.CreateHarborArtifact(context.Background(), HarborArtifactSpec{
		Repository: "apps/web",
		Reference:  "latest",
		Kind:       "container",
	}); err != nil {
		t.Fatalf("idempotent create: %v", err)
	}
}

func TestHarborArtifactClientEscapesIDAndToleratesDelete404(t *testing.T) {
	t.Parallel()
	client := newRegistryTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.RequestURI, "/artifacts/artifact%2Fid") {
			t.Fatalf("ID is not path escaped: %q", r.RequestURI)
		}
		w.WriteHeader(http.StatusNotFound)
	})

	if err := client.DeleteHarborArtifact(context.Background(), "artifact/id"); err != nil {
		t.Fatalf("DeleteHarborArtifact 404: %v", err)
	}
}

func TestHarborRegistryBindingClientLifecycle(t *testing.T) {
	t.Parallel()
	var calls []string
	client := newRegistryTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/registry/bindings":
			var payload HarborRegistryBindingSpec
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode create: %v", err)
			}
			if payload.EnvironmentID != "11111111-2222-4333-8444-555555555555" ||
				payload.ClusterID != "22222222-3333-4444-8555-666666666666" ||
				payload.Namespace != "applications" ||
				payload.SecretName != "fcs-harbor-pull" ||
				len(payload.ServiceAccounts) != 1 || payload.ServiceAccounts[0] != "default" {
				t.Fatalf("unexpected create payload: %#v", payload)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{
				"id":"33333333-4444-4555-8666-777777777777",
				"environment_id":"11111111-2222-4333-8444-555555555555",
				"cluster_id":"22222222-3333-4444-8555-666666666666",
				"namespace":"applications",
				"secret_name":"fcs-harbor-pull",
				"service_accounts":["default"],
				"project":"t-8012",
				"registry":"harbor.cloud.focusnet.de",
				"status":"active"
			}`))
		case r.Method == http.MethodGet &&
			r.URL.Path == "/v1/registry/bindings/33333333-4444-4555-8666-777777777777":
			_, _ = w.Write([]byte(`{
				"id":"33333333-4444-4555-8666-777777777777",
				"environment_id":"11111111-2222-4333-8444-555555555555",
				"cluster_id":"22222222-3333-4444-8555-666666666666",
				"namespace":"applications",
				"secret_name":"fcs-harbor-pull",
				"service_accounts":["default"],
				"project":"t-8012",
				"registry":"harbor.cloud.focusnet.de",
				"status":"active"
			}`))
		case r.Method == http.MethodPatch &&
			r.URL.Path == "/v1/registry/bindings/33333333-4444-4555-8666-777777777777":
			var payload HarborRegistryBindingUpdate
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode update: %v", err)
			}
			if len(payload.ServiceAccounts) != 2 || payload.ServiceAccounts[1] != "builder" {
				t.Fatalf("unexpected update payload: %#v", payload)
			}
			_, _ = w.Write([]byte(`{
				"id":"33333333-4444-4555-8666-777777777777",
				"environment_id":"11111111-2222-4333-8444-555555555555",
				"cluster_id":"22222222-3333-4444-8555-666666666666",
				"namespace":"applications",
				"secret_name":"fcs-harbor-pull",
				"service_accounts":["default","builder"],
				"project":"t-8012",
				"registry":"harbor.cloud.focusnet.de",
				"status":"active"
			}`))
		case r.Method == http.MethodPost &&
			r.URL.Path == "/v1/registry/bindings/33333333-4444-4555-8666-777777777777/rotations":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{
				"id":"33333333-4444-4555-8666-777777777777",
				"environment_id":"11111111-2222-4333-8444-555555555555",
				"cluster_id":"22222222-3333-4444-8555-666666666666",
				"namespace":"applications",
				"secret_name":"fcs-harbor-pull",
				"service_accounts":["default","builder"],
				"project":"t-8012",
				"registry":"harbor.cloud.focusnet.de",
				"status":"active"
			}`))
		case r.Method == http.MethodDelete &&
			r.URL.Path == "/v1/registry/bindings/33333333-4444-4555-8666-777777777777":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})

	ctx := context.Background()
	created, err := client.CreateHarborRegistryBinding(ctx, HarborRegistryBindingSpec{
		EnvironmentID:   "11111111-2222-4333-8444-555555555555",
		ClusterID:       "22222222-3333-4444-8555-666666666666",
		Namespace:       "applications",
		SecretName:      "fcs-harbor-pull",
		ServiceAccounts: []string{"default"},
	})
	if err != nil {
		t.Fatalf("CreateHarborRegistryBinding: %v", err)
	}
	if created.Project != "t-8012" {
		t.Fatalf("project = %q", created.Project)
	}
	if _, err := client.GetHarborRegistryBinding(ctx, created.ID); err != nil {
		t.Fatalf("GetHarborRegistryBinding: %v", err)
	}
	updated, err := client.UpdateHarborRegistryBinding(
		ctx,
		created.ID,
		HarborRegistryBindingUpdate{ServiceAccounts: []string{"default", "builder"}},
	)
	if err != nil {
		t.Fatalf("UpdateHarborRegistryBinding: %v", err)
	}
	if len(updated.ServiceAccounts) != 2 {
		t.Fatalf("updated service accounts = %v", updated.ServiceAccounts)
	}
	if _, err := client.RotateHarborRegistryBinding(ctx, created.ID); err != nil {
		t.Fatalf("RotateHarborRegistryBinding: %v", err)
	}
	if err := client.DeleteHarborRegistryBinding(ctx, created.ID); err != nil {
		t.Fatalf("DeleteHarborRegistryBinding: %v", err)
	}
	if len(calls) != 5 {
		t.Fatalf("calls = %v", calls)
	}
}

func TestHarborRegistryBindingClientEscapesIDAndToleratesDelete404(t *testing.T) {
	t.Parallel()
	client := newRegistryTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.RequestURI, "/bindings/binding%2Fid") {
			t.Fatalf("ID is not path escaped: %q", r.RequestURI)
		}
		w.WriteHeader(http.StatusNotFound)
	})
	if err := client.DeleteHarborRegistryBinding(context.Background(), "binding/id"); err != nil {
		t.Fatalf("DeleteHarborRegistryBinding 404: %v", err)
	}
}
