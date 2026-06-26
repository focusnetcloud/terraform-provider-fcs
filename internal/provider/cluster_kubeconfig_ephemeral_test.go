package provider

// Unit tests for the fcs_cluster_kubeconfig ephemeral resource (Open invoked
// directly — no terraform CLI, no TF_ACC). The credential mapping and the
// error paths (404 gone cluster, 409 conflict, empty response) are covered
// here; the never-persisted-to-state guarantee is proven in the acceptance
// tests (cluster_kubeconfig_ephemeral_acc_test.go).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/ephemeral"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/focusnetcloud/terraform-provider-fcs/internal/client"
	"github.com/focusnetcloud/terraform-provider-fcs/internal/mockapi"
)

// openKubeconfigEphemeral configures the ephemeral resource against baseURL
// and invokes Open for (envID, clusterID), mirroring how the framework
// drives it.
func openKubeconfigEphemeral(t *testing.T, baseURL, envID, clusterID string) ephemeral.OpenResponse {
	t.Helper()
	c, err := client.New(baseURL, unitToken)
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}

	r := NewClusterKubeconfigEphemeralResource().(*clusterKubeconfigEphemeralResource)
	cfgResp := ephemeral.ConfigureResponse{}
	r.Configure(context.Background(), ephemeral.ConfigureRequest{ProviderData: c}, &cfgResp)
	if cfgResp.Diagnostics.HasError() {
		t.Fatalf("configure: %v", cfgResp.Diagnostics)
	}

	schemaResp := ephemeral.SchemaResponse{}
	r.Schema(context.Background(), ephemeral.SchemaRequest{}, &schemaResp)
	if schemaResp.Diagnostics.HasError() {
		t.Fatalf("schema: %v", schemaResp.Diagnostics)
	}
	s := schemaResp.Schema
	objType := s.Type().TerraformType(context.Background()).(tftypes.Object)
	configRaw := tftypes.NewValue(objType, map[string]tftypes.Value{
		"environment_id": tftypes.NewValue(tftypes.String, envID),
		"cluster_id":     tftypes.NewValue(tftypes.String, clusterID),
		"api_server_url": tftypes.NewValue(tftypes.String, nil),
		"kubeconfig":     tftypes.NewValue(tftypes.String, nil),
		"sa_token":       tftypes.NewValue(tftypes.String, nil),
		"expires_at":     tftypes.NewValue(tftypes.String, nil),
	})

	resp := ephemeral.OpenResponse{
		Result: tfsdk.EphemeralResultData{Schema: s, Raw: configRaw},
	}
	r.Open(context.Background(), ephemeral.OpenRequest{
		Config: tfsdk.Config{Schema: s, Raw: configRaw},
	}, &resp)
	return resp
}

// newClusterForKubeconfig provisions environment + business cluster on the
// mock and returns (envID, cluster).
func newClusterForKubeconfig(t *testing.T, srv *mockapi.Server, name string) (string, *client.Cluster) {
	t.Helper()
	c, err := client.New(srv.URL, unitToken)
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	env, err := c.CreateEnvironment(context.Background(), client.EnvironmentSpec{Name: name})
	if err != nil {
		t.Fatalf("create environment: %v", err)
	}
	cl, err := c.CreateCluster(context.Background(), env.ID, client.ClusterSpec{Kind: "business"})
	if err != nil {
		t.Fatalf("create cluster: %v", err)
	}
	return env.ID, cl
}

func TestUnitKubeconfigEphemeralOpen(t *testing.T) {
	srv := mockapi.New(unitToken)
	defer srv.Close()
	envID, cl := newClusterForKubeconfig(t, srv, "lab-eph-kc")

	resp := openKubeconfigEphemeral(t, srv.URL, envID, cl.ID)
	if resp.Diagnostics.HasError() {
		t.Fatalf("open: %v", resp.Diagnostics)
	}

	var model clusterKubeconfigEphemeralModel
	if diags := resp.Result.Get(context.Background(), &model); diags.HasError() {
		t.Fatalf("result get: %v", diags)
	}
	if got := model.APIServerURL.ValueString(); got != cl.APIServerURL {
		t.Fatalf("api_server_url: got %q, want %q", got, cl.APIServerURL)
	}
	if model.SAToken.IsNull() || model.SAToken.ValueString() == "" {
		t.Fatal("expected a non-empty sa_token")
	}
	if model.ExpiresAt.IsNull() || model.ExpiresAt.ValueString() == "" {
		t.Fatal("expected a non-empty expires_at")
	}
	kc := model.Kubeconfig.ValueString()
	if !strings.Contains(kc, "apiVersion: v1") || !strings.Contains(kc, "kind: Config") {
		t.Fatal("kubeconfig does not look like a kubeconfig document")
	}
	if !strings.Contains(kc, model.SAToken.ValueString()) {
		t.Fatal("kubeconfig must embed the minted sa_token")
	}
	if got := model.EnvironmentID.ValueString(); got != envID {
		t.Fatalf("environment_id must round-trip, got %q", got)
	}
	if got := model.ClusterID.ValueString(); got != cl.ID {
		t.Fatalf("cluster_id must round-trip, got %q", got)
	}
}

func TestUnitKubeconfigEphemeralOpenWithoutSAToken(t *testing.T) {
	srv := mockapi.New(unitToken)
	defer srv.Close()
	srv.OmitSAToken = true
	envID, cl := newClusterForKubeconfig(t, srv, "lab-eph-kc-nosa")

	resp := openKubeconfigEphemeral(t, srv.URL, envID, cl.ID)
	if resp.Diagnostics.HasError() {
		t.Fatalf("open: %v", resp.Diagnostics)
	}

	var model clusterKubeconfigEphemeralModel
	if diags := resp.Result.Get(context.Background(), &model); diags.HasError() {
		t.Fatalf("result get: %v", diags)
	}
	if !model.SAToken.IsNull() {
		t.Fatalf("expected null sa_token with OmitSAToken, got %v", model.SAToken)
	}
	if model.Kubeconfig.IsNull() || model.Kubeconfig.ValueString() == "" {
		t.Fatal("kubeconfig must still be returned")
	}
}

func TestUnitKubeconfigEphemeralOpenGoneCluster(t *testing.T) {
	srv := mockapi.New(unitToken)
	defer srv.Close()
	envID, cl := newClusterForKubeconfig(t, srv, "lab-eph-kc-gone")

	c, err := client.New(srv.URL, unitToken)
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	if err := c.DeleteCluster(context.Background(), envID, cl.ID); err != nil {
		t.Fatalf("delete cluster: %v", err)
	}

	resp := openKubeconfigEphemeral(t, srv.URL, envID, cl.ID)
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected an error minting for an offboarded cluster (404)")
	}
	if errs := resp.Diagnostics.Errors(); !strings.Contains(errs[0].Detail(), "HTTP 404") {
		t.Fatalf("expected a 404 diagnostic, got: %s", errs[0].Detail())
	}
}

func TestUnitKubeconfigEphemeralOpenConflict(t *testing.T) {
	// The contract mock has no 409 path on the kubeconfig endpoint; a bare
	// handler stands in for a conflicting server response.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"detail":"kubeconfig mint already in progress","reason":"Conflict"}`))
	}))
	defer srv.Close()

	resp := openKubeconfigEphemeral(t, srv.URL, "env-x", "cl-x")
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected an error on HTTP 409")
	}
	detail := resp.Diagnostics.Errors()[0].Detail()
	if !strings.Contains(detail, "HTTP 409") || !strings.Contains(detail, "kubeconfig mint already in progress") {
		t.Fatalf("expected a 409 diagnostic with the server detail, got: %s", detail)
	}
}

func TestUnitKubeconfigEphemeralOpenEmptyResponse(t *testing.T) {
	// 201 with neither kubeconfig nor sa_token must fail loudly instead of
	// handing an unusable null credential pair to dependent providers.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"api_server_url":"https://x:6443","expires_at":"2030-01-01T00:00:00Z"}`))
	}))
	defer srv.Close()

	resp := openKubeconfigEphemeral(t, srv.URL, "env-x", "cl-x")
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected an error for an empty credential response")
	}
	if sum := resp.Diagnostics.Errors()[0].Summary(); !strings.Contains(sum, "Empty fcs_cluster_kubeconfig response") {
		t.Fatalf("expected the empty-response diagnostic, got: %s", sum)
	}
}
