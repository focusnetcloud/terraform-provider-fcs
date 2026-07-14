package client_test

// Tests for the cluster sub-API client and the mock's cluster status
// machine (provisioning -> active after N GETs; DELETE -> destroyed ->
// 404; idempotent re-POST over (environment, kind)).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/focusnetcloud/terraform-provider-fcs/internal/client"
	"github.com/focusnetcloud/terraform-provider-fcs/internal/mockapi"
)

func newEnvForClusters(t *testing.T, c *client.Client, name string) string {
	t.Helper()
	env, err := c.CreateEnvironment(context.Background(), client.EnvironmentSpec{Name: name})
	if err != nil {
		t.Fatalf("create environment: %v", err)
	}
	return env.ID
}

func TestCreateClusterNew(t *testing.T) {
	srv := mockapi.New(testToken)
	defer srv.Close()
	c := newTestClient(t, srv.URL, testToken)
	envID := newEnvForClusters(t, c, "lab-cl-new")

	cl, err := c.CreateCluster(context.Background(), envID, client.ClusterSpec{Kind: "business", Size: "S"})
	if err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	if cl.ID == "" {
		t.Fatal("expected non-empty cluster id")
	}
	if cl.Kind != "business" {
		t.Fatalf("expected kind echo business, got %q", cl.Kind)
	}
	if cl.Status != "provisioning" {
		t.Fatalf("expected status provisioning, got %q", cl.Status)
	}
	if cl.APIServerURL == "" || cl.ClusterCIDR == "" || cl.ServiceCIDR == "" {
		t.Fatalf("expected server-allocated api_server_url and CIDRs, got %+v", cl)
	}
	if srv.ClusterCount() != 1 {
		t.Fatalf("expected 1 cluster, got %d", srv.ClusterCount())
	}
}

func TestCreateClusterIdempotentReapply(t *testing.T) {
	srv := mockapi.New(testToken)
	defer srv.Close()
	c := newTestClient(t, srv.URL, testToken)
	envID := newEnvForClusters(t, c, "lab-cl-idem")
	spec := client.ClusterSpec{Kind: "flex", VCPU: 4, RAMGB: 8, StorageGB: 50}

	first, err := c.CreateCluster(context.Background(), envID, spec)
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	second, err := c.CreateCluster(context.Background(), envID, spec)
	if err != nil {
		t.Fatalf("idempotent re-POST (HTTP 200) must succeed: %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("idempotent re-POST returned different ids: %s vs %s", first.ID, second.ID)
	}
	if srv.ClusterCount() != 1 {
		t.Fatalf("expected exactly 1 cluster, got %d", srv.ClusterCount())
	}
}

func TestResizeClusterKeepsIdentityAndReturnsDesiredSizing(t *testing.T) {
	srv := mockapi.New(testToken)
	defer srv.Close()
	c := newTestClient(t, srv.URL, testToken)
	envID := newEnvForClusters(t, c, "lab-cl-resize")
	created, err := c.CreateCluster(context.Background(), envID, client.ClusterSpec{
		Kind: "business",
		Size: "S",
	})
	if err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}

	resized, err := c.ResizeCluster(context.Background(), envID, created.ID, client.ClusterResizeSpec{
		Size: "M",
	})
	if err != nil {
		t.Fatalf("ResizeCluster: %v", err)
	}
	if resized.ID != created.ID {
		t.Fatalf("resize changed cluster id: got %q, want %q", resized.ID, created.ID)
	}
	if resized.Status != "resizing" {
		t.Fatalf("status = %q, want resizing", resized.Status)
	}
	if resized.DesiredSpec == nil || resized.DesiredSpec.VCPU != 8 || resized.DesiredSpec.RAMGB != 16 || resized.DesiredSpec.StorageGB != 250 {
		t.Fatalf("resize response must expose the API-resolved desired sizing, got %+v", resized.DesiredSpec)
	}
	if srv.ClusterCount() != 1 {
		t.Fatalf("resize must not create a second cluster, got %d", srv.ClusterCount())
	}
}

func TestResizeClusterSerializesZeroWorkerNodes(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode resize body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"id":"cluster-1","kind":"dedicated","status":"resizing","desired_spec":{"worker_nodes":0}}`))
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL, testToken)
	zero := int64(0)

	resized, err := c.ResizeCluster(context.Background(), "env-1", "cluster-1", client.ClusterResizeSpec{WorkerNodes: &zero})
	if err != nil {
		t.Fatalf("ResizeCluster: %v", err)
	}
	value, present := body["worker_nodes"]
	if !present || value != float64(0) {
		t.Fatalf("worker_nodes=0 must be present in PATCH body, got %#v", body)
	}
	if resized.DesiredSpec == nil || resized.DesiredSpec.WorkerNodes != 0 {
		t.Fatalf("expected desired worker_nodes=0, got %+v", resized.DesiredSpec)
	}
}

func TestResizeClusterRejectsStorageShrink(t *testing.T) {
	srv := mockapi.New(testToken)
	defer srv.Close()
	c := newTestClient(t, srv.URL, testToken)
	envID := newEnvForClusters(t, c, "lab-cl-shrink")
	created, err := c.CreateCluster(context.Background(), envID, client.ClusterSpec{Kind: "business", VCPU: 4, RAMGB: 8, StorageGB: 100})
	if err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}

	_, err = c.ResizeCluster(context.Background(), envID, created.ID, client.ClusterResizeSpec{VCPU: 4, RAMGB: 8, StorageGB: 50})
	if err == nil || !strings.Contains(err.Error(), "HTTP 409") {
		t.Fatalf("storage shrink must return HTTP 409, got %v", err)
	}
}

// TestCreateClusterIdempotentSameKindDifferentSpec: the server is idempotent
// over (environment, kind) — a re-POST with a *different* spec still returns
// the existing non-terminal cluster (200) instead of a 409; 409 is reserved
// for real quota cases.
func TestCreateClusterIdempotentSameKindDifferentSpec(t *testing.T) {
	srv := mockapi.New(testToken)
	defer srv.Close()
	c := newTestClient(t, srv.URL, testToken)
	envID := newEnvForClusters(t, c, "lab-cl-kind-idem")

	first, err := c.CreateCluster(context.Background(), envID, client.ClusterSpec{Kind: "business", Size: "S"})
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	second, err := c.CreateCluster(context.Background(), envID, client.ClusterSpec{Kind: "business", Size: "M"})
	if err != nil {
		t.Fatalf("same-kind re-POST with a different spec must return the existing cluster (200): %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("expected the existing cluster on same-kind re-POST, got %s != %s", second.ID, first.ID)
	}
	if srv.ClusterCount() != 1 {
		t.Fatalf("expected exactly 1 cluster, got %d", srv.ClusterCount())
	}
	// A different kind in the same environment creates a new cluster.
	other, err := c.CreateCluster(context.Background(), envID, client.ClusterSpec{Kind: "namespace"})
	if err != nil {
		t.Fatalf("different kind must not conflict: %v", err)
	}
	if other.ID == first.ID {
		t.Fatal("different kind must create a new cluster")
	}
	if srv.ClusterCount() != 2 {
		t.Fatalf("expected 2 clusters, got %d", srv.ClusterCount())
	}
}

func TestCreateClusterInvalidKind(t *testing.T) {
	srv := mockapi.New(testToken)
	defer srv.Close()
	c := newTestClient(t, srv.URL, testToken)
	envID := newEnvForClusters(t, c, "lab-cl-badkind")

	// "enterprise" is not a supported cluster kind.
	_, err := c.CreateCluster(context.Background(), envID, client.ClusterSpec{Kind: "enterprise"})
	if err == nil {
		t.Fatal("expected 400 for unsupported cluster kind")
	}
}

// TestCreateClusterDedicated verifies that kind=dedicated accepts explicit
// node-pool fields.
func TestCreateClusterDedicated(t *testing.T) {
	srv := mockapi.New(testToken)
	defer srv.Close()
	c := newTestClient(t, srv.URL, testToken)
	envID := newEnvForClusters(t, c, "lab-cl-dedicated")

	cl, err := c.CreateCluster(context.Background(), envID, client.ClusterSpec{
		Kind:         "dedicated",
		CPNodes:      3,
		CPVcpu:       4,
		CPRamGB:      8,
		WorkerNodes:  2,
		WorkerVcpu:   8,
		WorkerRamGB:  16,
		PVCStorageGB: 200,
		RKE2Version:  "v1.31.5+rke2r1",
	})
	if err != nil {
		t.Fatalf("CreateCluster (dedicated): %v", err)
	}
	if cl.ID == "" {
		t.Fatal("expected non-empty cluster id")
	}
	if cl.Kind != "dedicated" {
		t.Fatalf("expected kind echo dedicated, got %q", cl.Kind)
	}
	if cl.Status != "provisioning" {
		t.Fatalf("expected status provisioning, got %q", cl.Status)
	}
	if srv.ClusterCount() != 1 {
		t.Fatalf("expected 1 cluster, got %d", srv.ClusterCount())
	}
}

// TestCreateClusterDedicatedIdempotent: re-POST over (environment, kind) for a
// dedicated cluster returns the existing one (200), even with a different spec.
func TestCreateClusterDedicatedIdempotent(t *testing.T) {
	srv := mockapi.New(testToken)
	defer srv.Close()
	c := newTestClient(t, srv.URL, testToken)
	envID := newEnvForClusters(t, c, "lab-cl-dedicated-idem")

	first, err := c.CreateCluster(context.Background(), envID, client.ClusterSpec{
		Kind: "dedicated", CPNodes: 1, CPVcpu: 2, CPRamGB: 4,
	})
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	second, err := c.CreateCluster(context.Background(), envID, client.ClusterSpec{
		Kind: "dedicated", CPNodes: 3, CPVcpu: 8, CPRamGB: 16, WorkerNodes: 3,
	})
	if err != nil {
		t.Fatalf("idempotent re-POST with a different spec must succeed (200): %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("idempotent re-POST returned different ids: %s vs %s", first.ID, second.ID)
	}
	if srv.ClusterCount() != 1 {
		t.Fatalf("expected exactly 1 cluster, got %d", srv.ClusterCount())
	}

	// A dedicated cluster coexists with the other kinds in the same environment.
	other, err := c.CreateCluster(context.Background(), envID, client.ClusterSpec{Kind: "business", Size: "S"})
	if err != nil {
		t.Fatalf("different kind must not conflict: %v", err)
	}
	if other.ID == first.ID {
		t.Fatal("a different kind must create a new cluster")
	}
	if srv.ClusterCount() != 2 {
		t.Fatalf("expected 2 clusters, got %d", srv.ClusterCount())
	}
}

// TestDedicatedClusterReadyAfterGETs: kind=dedicated honours its own slower
// provisioning knob and exposes https://<public_ip>:6443 once active.
func TestDedicatedClusterReadyAfterGETs(t *testing.T) {
	srv := mockapi.New(testToken)
	defer srv.Close()
	srv.ClusterReadyAfterGETs = 1          // business/flex would be active immediately
	srv.DedicatedClusterReadyAfterGETs = 3 // dedicated provisions slower
	c := newTestClient(t, srv.URL, testToken)
	envID := newEnvForClusters(t, c, "lab-cl-dedicated-ready")

	created, err := c.CreateCluster(context.Background(), envID, client.ClusterSpec{
		Kind: "dedicated", CPNodes: 3, CPVcpu: 4, CPRamGB: 8,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	for i := 1; i <= 2; i++ {
		cl, err := c.GetCluster(context.Background(), envID, created.ID)
		if err != nil {
			t.Fatalf("GET #%d: %v", i, err)
		}
		if cl.Status != "provisioning" {
			t.Fatalf("GET #%d: expected provisioning (dedicated knob=3), got %q", i, cl.Status)
		}
	}
	cl, err := c.GetCluster(context.Background(), envID, created.ID)
	if err != nil {
		t.Fatalf("GET #3: %v", err)
	}
	if cl.Status != "active" {
		t.Fatalf("GET #3: expected active after DedicatedClusterReadyAfterGETs, got %q", cl.Status)
	}
	if !strings.HasPrefix(cl.APIServerURL, "https://") || !strings.HasSuffix(cl.APIServerURL, ":6443") {
		t.Fatalf("expected dedicated api_server_url https://<ip>:6443, got %q", cl.APIServerURL)
	}
}

func TestCreateClusterUnknownEnvironment(t *testing.T) {
	srv := mockapi.New(testToken)
	defer srv.Close()
	c := newTestClient(t, srv.URL, testToken)

	_, err := c.CreateCluster(context.Background(), "00000000-0000-4000-8000-000000000000", client.ClusterSpec{Kind: "business"})
	if !client.IsNotFound(err) {
		t.Fatalf("expected 404 for unknown environment, got %v", err)
	}
}

func TestClusterStatusMachineReadyAfterGETs(t *testing.T) {
	srv := mockapi.New(testToken)
	defer srv.Close()
	srv.ClusterReadyAfterGETs = 3
	c := newTestClient(t, srv.URL, testToken)
	envID := newEnvForClusters(t, c, "lab-cl-ready")

	created, err := c.CreateCluster(context.Background(), envID, client.ClusterSpec{Kind: "business"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	for i := 1; i <= 2; i++ {
		cl, err := c.GetCluster(context.Background(), envID, created.ID)
		if err != nil {
			t.Fatalf("GET #%d: %v", i, err)
		}
		if cl.Status != "provisioning" {
			t.Fatalf("GET #%d: expected provisioning, got %q", i, cl.Status)
		}
	}
	cl, err := c.GetCluster(context.Background(), envID, created.ID)
	if err != nil {
		t.Fatalf("GET #3: %v", err)
	}
	if cl.Status != "active" {
		t.Fatalf("GET #3: expected active after ClusterReadyAfterGETs, got %q", cl.Status)
	}
}

func TestClusterStatusMachineErrorPath(t *testing.T) {
	srv := mockapi.New(testToken)
	defer srv.Close()
	srv.ClusterReadyAfterGETs = 1
	srv.FailClusterProvisioning = true
	c := newTestClient(t, srv.URL, testToken)
	envID := newEnvForClusters(t, c, "lab-cl-error")

	created, err := c.CreateCluster(context.Background(), envID, client.ClusterSpec{Kind: "flex"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	cl, err := c.GetCluster(context.Background(), envID, created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if cl.Status != "error" {
		t.Fatalf("expected status error with FailClusterProvisioning, got %q", cl.Status)
	}
}

func TestDeleteClusterDestroyedThenGone(t *testing.T) {
	srv := mockapi.New(testToken)
	defer srv.Close()
	srv.ClusterGoneAfterGETs = 1
	c := newTestClient(t, srv.URL, testToken)
	envID := newEnvForClusters(t, c, "lab-cl-del")

	created, err := c.CreateCluster(context.Background(), envID, client.ClusterSpec{Kind: "namespace"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := c.DeleteCluster(context.Background(), envID, created.ID); err != nil {
		t.Fatalf("delete (202): %v", err)
	}
	// First GET after DELETE: the row is still readable as destroyed.
	cl, err := c.GetCluster(context.Background(), envID, created.ID)
	if err != nil {
		t.Fatalf("get after delete: %v", err)
	}
	if cl.Status != "destroyed" {
		t.Fatalf("expected destroyed after DELETE, got %q", cl.Status)
	}
	// Repeated DELETE on a destroyed cluster stays a 202 (idempotent).
	if err := c.DeleteCluster(context.Background(), envID, created.ID); err != nil {
		t.Fatalf("repeated delete must be idempotent: %v", err)
	}
	// Next GET: gone (404 — the second gone signal).
	if _, err := c.GetCluster(context.Background(), envID, created.ID); !client.IsNotFound(err) {
		t.Fatalf("expected 404 after gone window, got %v", err)
	}
	// DELETE on a gone cluster is tolerated by the client (404).
	if err := c.DeleteCluster(context.Background(), envID, created.ID); err != nil {
		t.Fatalf("delete on gone cluster must tolerate 404: %v", err)
	}
}

func TestEnvironmentDeleteCascadesClusters(t *testing.T) {
	srv := mockapi.New(testToken)
	defer srv.Close()
	c := newTestClient(t, srv.URL, testToken)
	envID := newEnvForClusters(t, c, "lab-cl-cascade")

	if _, err := c.CreateCluster(context.Background(), envID, client.ClusterSpec{Kind: "business"}); err != nil {
		t.Fatalf("create cluster: %v", err)
	}
	if err := c.DeleteEnvironment(context.Background(), envID); err != nil {
		t.Fatalf("delete environment: %v", err)
	}
	if srv.ClusterCount() != 0 {
		t.Fatalf("environment teardown must cascade clusters, %d left", srv.ClusterCount())
	}
}

func TestMintKubeconfig(t *testing.T) {
	srv := mockapi.New(testToken)
	defer srv.Close()
	c := newTestClient(t, srv.URL, testToken)
	envID := newEnvForClusters(t, c, "lab-cl-kc")

	created, err := c.CreateCluster(context.Background(), envID, client.ClusterSpec{Kind: "business"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	creds, err := c.MintKubeconfig(context.Background(), envID, created.ID)
	if err != nil {
		t.Fatalf("MintKubeconfig: %v", err)
	}
	if creds.APIServerURL != created.APIServerURL {
		t.Fatalf("expected api_server_url %q, got %q", created.APIServerURL, creds.APIServerURL)
	}
	if creds.SAToken == "" || creds.ExpiresAt == "" {
		t.Fatalf("expected sa_token and expires_at, got %+v", creds)
	}
	if !strings.Contains(creds.Kubeconfig, creds.APIServerURL) || !strings.Contains(creds.Kubeconfig, creds.SAToken) {
		t.Fatal("kubeconfig must embed server URL and token")
	}
}

func TestMintKubeconfigWithoutSAToken(t *testing.T) {
	srv := mockapi.New(testToken)
	defer srv.Close()
	srv.OmitSAToken = true
	c := newTestClient(t, srv.URL, testToken)
	envID := newEnvForClusters(t, c, "lab-cl-kc-nosa")

	created, err := c.CreateCluster(context.Background(), envID, client.ClusterSpec{Kind: "flex"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	creds, err := c.MintKubeconfig(context.Background(), envID, created.ID)
	if err != nil {
		t.Fatalf("MintKubeconfig: %v", err)
	}
	if creds.SAToken != "" {
		t.Fatalf("expected empty sa_token with OmitSAToken, got %q", creds.SAToken)
	}
	if creds.Kubeconfig == "" {
		t.Fatal("kubeconfig must still be returned")
	}
}

func TestMintKubeconfigNamespaceUnsupported(t *testing.T) {
	srv := mockapi.New(testToken)
	defer srv.Close()
	c := newTestClient(t, srv.URL, testToken)
	envID := newEnvForClusters(t, c, "lab-cl-kc-namespace")

	created, err := c.CreateCluster(context.Background(), envID, client.ClusterSpec{Kind: "namespace"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	_, err = c.MintKubeconfig(context.Background(), envID, created.ID)
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusConflict {
		t.Fatalf("expected APIError 409 for a shared namespace, got %v", err)
	}
	if apiErr.Reason != "KubeconfigUnsupported" {
		t.Fatalf("expected reason KubeconfigUnsupported, got %q", apiErr.Reason)
	}
	if srv.KubeconfigMintCount() != 0 {
		t.Fatal("unsupported namespace request must not mint credentials")
	}
}

func TestMintKubeconfigGoneCluster(t *testing.T) {
	srv := mockapi.New(testToken)
	defer srv.Close()
	c := newTestClient(t, srv.URL, testToken)
	envID := newEnvForClusters(t, c, "lab-cl-kc-gone")

	created, err := c.CreateCluster(context.Background(), envID, client.ClusterSpec{Kind: "namespace"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := c.DeleteCluster(context.Background(), envID, created.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := c.MintKubeconfig(context.Background(), envID, created.ID); !client.IsNotFound(err) {
		t.Fatalf("expected 404 minting for an offboarding cluster, got %v", err)
	}
}
