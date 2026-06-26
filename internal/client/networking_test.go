package client_test

// Tests for the Ingress/Egress sub-API client and the mock's status machine:
//   - ingress: 202 + public_url; idempotent over (cluster, hostname_prefix);
//     409 when the cluster is not business/active/public_ip; 404 for an
//     unknown cluster; DELETE -> destroyed -> 404 (tolerant)
//   - egress: 202; idempotent over the natural key; 422 for an invalid CIDR;
//     404 for an unknown cluster; DELETE -> destroyed -> 404 (tolerant)
//   - environment DELETE cascades ingress/egress

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"testing"

	"github.com/focusnetcloud/terraform-provider-fcs/internal/client"
	"github.com/focusnetcloud/terraform-provider-fcs/internal/mockapi"
)

// newEnvAndBusinessCluster creates an environment plus an active business
// cluster (the ingress precondition) and returns their IDs. The mock turns a
// business cluster active — and assigns it a public IP — after one GET.
func newEnvAndBusinessCluster(t *testing.T, c *client.Client, envName string) (string, string) {
	t.Helper()
	env, err := c.CreateEnvironment(context.Background(), client.EnvironmentSpec{Name: envName})
	if err != nil {
		t.Fatalf("create environment: %v", err)
	}
	cl, err := c.CreateCluster(context.Background(), env.ID, client.ClusterSpec{Kind: "business"})
	if err != nil {
		t.Fatalf("create business cluster: %v", err)
	}
	// One GET drives provisioning -> active (ClusterReadyAfterGETs=1 set by
	// the caller's mock) and allocates the public IP.
	got, err := c.GetCluster(context.Background(), env.ID, cl.ID)
	if err != nil {
		t.Fatalf("GetCluster: %v", err)
	}
	if got.Status != "active" {
		t.Fatalf("expected business cluster active, got %q", got.Status)
	}
	return env.ID, cl.ID
}

func TestCreateIngressNew(t *testing.T) {
	srv := mockapi.New(testToken)
	srv.ClusterReadyAfterGETs = 1
	defer srv.Close()
	c := newTestClient(t, srv.URL, testToken)
	envID, clusterID := newEnvAndBusinessCluster(t, c, "lab-ing-new")

	ing, err := c.CreateIngress(context.Background(), envID, client.IngressSpec{
		ClusterID:      clusterID,
		Service:        "web",
		Port:           8080,
		TLS:            "auto",
		HostnamePrefix: "alice",
	})
	if err != nil {
		t.Fatalf("CreateIngress: %v", err)
	}
	if ing.ID == "" {
		t.Fatal("expected non-empty ingress id")
	}
	if ing.Status != "provisioning" {
		t.Fatalf("expected status provisioning, got %q", ing.Status)
	}
	// public_url is the real L4 endpoint https://<public_ip>:<port>.
	if !regexp.MustCompile(`^https://203\.0\.113\.\d+:8080$`).MatchString(ing.PublicURL) {
		t.Fatalf("unexpected public_url %q", ing.PublicURL)
	}
	if srv.IngressCount() != 1 {
		t.Fatalf("expected 1 ingress, got %d", srv.IngressCount())
	}
}

func TestCreateIngressIdempotentReapply(t *testing.T) {
	srv := mockapi.New(testToken)
	srv.ClusterReadyAfterGETs = 1
	defer srv.Close()
	c := newTestClient(t, srv.URL, testToken)
	envID, clusterID := newEnvAndBusinessCluster(t, c, "lab-ing-idem")
	spec := client.IngressSpec{
		ClusterID:      clusterID,
		Service:        "web",
		Port:           443,
		TLS:            "auto",
		HostnamePrefix: "alice",
	}

	first, err := c.CreateIngress(context.Background(), envID, spec)
	if err != nil {
		t.Fatalf("first CreateIngress: %v", err)
	}
	second, err := c.CreateIngress(context.Background(), envID, spec)
	if err != nil {
		t.Fatalf("idempotent re-POST: %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("expected the existing ingress on re-POST, got %s != %s", first.ID, second.ID)
	}
	if srv.IngressCount() != 1 {
		t.Fatalf("expected 1 ingress after re-POST, got %d", srv.IngressCount())
	}
}

func TestCreateIngressNonBusinessCluster409(t *testing.T) {
	srv := mockapi.New(testToken)
	srv.ClusterReadyAfterGETs = 1
	defer srv.Close()
	c := newTestClient(t, srv.URL, testToken)

	env, err := c.CreateEnvironment(context.Background(), client.EnvironmentSpec{Name: "lab-ing-409"})
	if err != nil {
		t.Fatalf("create environment: %v", err)
	}
	cl, err := c.CreateCluster(context.Background(), env.ID, client.ClusterSpec{Kind: "flex"})
	if err != nil {
		t.Fatalf("create flex cluster: %v", err)
	}
	if _, err := c.GetCluster(context.Background(), env.ID, cl.ID); err != nil { // -> active, no public IP
		t.Fatalf("GetCluster: %v", err)
	}

	_, err = c.CreateIngress(context.Background(), env.ID, client.IngressSpec{
		ClusterID:      cl.ID,
		Service:        "web",
		Port:           80,
		TLS:            "auto",
		HostnamePrefix: "alice",
	})
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusConflict {
		t.Fatalf("expected APIError 409 for non-business cluster, got %v", err)
	}
	if srv.IngressCount() != 0 {
		t.Fatalf("expected no ingress after 409, got %d", srv.IngressCount())
	}
}

// TestCreateEgressFlexCluster422: egress is cluster-scoped and needs a
// business cluster's own workload CIDR; flex/namespace (host-VPC, no dedicated
// cluster_cidr) are rejected with 422, mirroring the server.
func TestCreateEgressFlexCluster422(t *testing.T) {
	srv := mockapi.New(testToken)
	srv.ClusterReadyAfterGETs = 1
	defer srv.Close()
	c := newTestClient(t, srv.URL, testToken)

	env, err := c.CreateEnvironment(context.Background(), client.EnvironmentSpec{Name: "lab-eg-422"})
	if err != nil {
		t.Fatalf("create environment: %v", err)
	}
	cl, err := c.CreateCluster(context.Background(), env.ID, client.ClusterSpec{Kind: "flex"})
	if err != nil {
		t.Fatalf("create flex cluster: %v", err)
	}
	if _, err := c.GetCluster(context.Background(), env.ID, cl.ID); err != nil { // -> active
		t.Fatalf("GetCluster: %v", err)
	}

	pr := "443"
	_, err = c.CreateEgress(context.Background(), env.ID, client.EgressSpec{
		ClusterID:       cl.ID,
		DestinationCIDR: "8.8.8.8/32",
		Protocol:        "tcp",
		PortRange:       &pr,
	})
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("expected APIError 422 for flex cluster egress, got %v", err)
	}
	if srv.EgressCount() != 0 {
		t.Fatalf("expected no egress after 422, got %d", srv.EgressCount())
	}
}

func TestCreateIngressUnknownCluster404(t *testing.T) {
	srv := mockapi.New(testToken)
	defer srv.Close()
	c := newTestClient(t, srv.URL, testToken)
	env, err := c.CreateEnvironment(context.Background(), client.EnvironmentSpec{Name: "lab-ing-404"})
	if err != nil {
		t.Fatalf("create environment: %v", err)
	}

	_, err = c.CreateIngress(context.Background(), env.ID, client.IngressSpec{
		ClusterID:      "00000000-0000-4000-8000-000000000000",
		Service:        "web",
		Port:           80,
		TLS:            "auto",
		HostnamePrefix: "alice",
	})
	if !client.IsNotFound(err) {
		t.Fatalf("expected IsNotFound for unknown cluster, got %v", err)
	}
}

func TestDeleteIngressDestroyedThenGone(t *testing.T) {
	srv := mockapi.New(testToken)
	srv.ClusterReadyAfterGETs = 1
	srv.IngressGoneAfterGETs = 1
	defer srv.Close()
	c := newTestClient(t, srv.URL, testToken)
	envID, clusterID := newEnvAndBusinessCluster(t, c, "lab-ing-del")

	ing, err := c.CreateIngress(context.Background(), envID, client.IngressSpec{
		ClusterID:      clusterID,
		Service:        "web",
		Port:           443,
		HostnamePrefix: "alice",
	})
	if err != nil {
		t.Fatalf("CreateIngress: %v", err)
	}
	if err := c.DeleteIngress(context.Background(), envID, ing.ID); err != nil {
		t.Fatalf("DeleteIngress: %v", err)
	}
	got, err := c.GetIngress(context.Background(), envID, ing.ID)
	if err != nil {
		t.Fatalf("GetIngress after delete: %v", err)
	}
	if got.Status != "destroyed" {
		t.Fatalf("expected destroyed after DELETE, got %q", got.Status)
	}
	if _, err := c.GetIngress(context.Background(), envID, ing.ID); !client.IsNotFound(err) {
		t.Fatalf("expected 404 after gone window, got %v", err)
	}
	if err := c.DeleteIngress(context.Background(), envID, ing.ID); err != nil {
		t.Fatalf("repeated DeleteIngress must tolerate 404: %v", err)
	}
	if srv.IngressCount() != 0 {
		t.Fatalf("expected 0 live ingresses, got %d", srv.IngressCount())
	}
}

func TestGetIngressUnknown404(t *testing.T) {
	srv := mockapi.New(testToken)
	defer srv.Close()
	c := newTestClient(t, srv.URL, testToken)
	env, err := c.CreateEnvironment(context.Background(), client.EnvironmentSpec{Name: "lab-ing-get404"})
	if err != nil {
		t.Fatalf("create environment: %v", err)
	}
	_, err = c.GetIngress(context.Background(), env.ID, "00000000-0000-4000-8000-000000000000")
	if !client.IsNotFound(err) {
		t.Fatalf("expected IsNotFound, got %v", err)
	}
}

func TestCreateEgressNew(t *testing.T) {
	srv := mockapi.New(testToken)
	srv.ClusterReadyAfterGETs = 1
	defer srv.Close()
	c := newTestClient(t, srv.URL, testToken)
	envID, clusterID := newEnvAndBusinessCluster(t, c, "lab-eg-new")

	pr := "443"
	eg, err := c.CreateEgress(context.Background(), envID, client.EgressSpec{
		ClusterID:       clusterID,
		DestinationCIDR: "10.0.0.0/8",
		Protocol:        "tcp",
		PortRange:       &pr,
	})
	if err != nil {
		t.Fatalf("CreateEgress: %v", err)
	}
	if eg.ID == "" {
		t.Fatal("expected non-empty egress id")
	}
	if eg.Status != "provisioning" {
		t.Fatalf("expected status provisioning, got %q", eg.Status)
	}
	if srv.EgressCount() != 1 {
		t.Fatalf("expected 1 egress, got %d", srv.EgressCount())
	}
}

func TestCreateEgressIdempotentReapply(t *testing.T) {
	srv := mockapi.New(testToken)
	srv.ClusterReadyAfterGETs = 1
	defer srv.Close()
	c := newTestClient(t, srv.URL, testToken)
	envID, clusterID := newEnvAndBusinessCluster(t, c, "lab-eg-idem")
	spec := client.EgressSpec{
		ClusterID:       clusterID,
		DestinationCIDR: "1.1.1.1/32",
		Protocol:        "any",
	}

	first, err := c.CreateEgress(context.Background(), envID, spec)
	if err != nil {
		t.Fatalf("first CreateEgress: %v", err)
	}
	second, err := c.CreateEgress(context.Background(), envID, spec)
	if err != nil {
		t.Fatalf("idempotent re-POST: %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("expected the existing egress on re-POST, got %s != %s", first.ID, second.ID)
	}
	if srv.EgressCount() != 1 {
		t.Fatalf("expected 1 egress after re-POST, got %d", srv.EgressCount())
	}
}

func TestCreateEgressInvalidCIDR422(t *testing.T) {
	srv := mockapi.New(testToken)
	srv.ClusterReadyAfterGETs = 1
	defer srv.Close()
	c := newTestClient(t, srv.URL, testToken)
	envID, clusterID := newEnvAndBusinessCluster(t, c, "lab-eg-422")

	_, err := c.CreateEgress(context.Background(), envID, client.EgressSpec{
		ClusterID:       clusterID,
		DestinationCIDR: "not-a-cidr",
	})
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("expected APIError 422 for invalid CIDR, got %v", err)
	}
	if srv.EgressCount() != 0 {
		t.Fatalf("expected no egress after 422, got %d", srv.EgressCount())
	}
}

func TestCreateEgressUnknownCluster404(t *testing.T) {
	srv := mockapi.New(testToken)
	defer srv.Close()
	c := newTestClient(t, srv.URL, testToken)
	env, err := c.CreateEnvironment(context.Background(), client.EnvironmentSpec{Name: "lab-eg-404"})
	if err != nil {
		t.Fatalf("create environment: %v", err)
	}

	_, err = c.CreateEgress(context.Background(), env.ID, client.EgressSpec{
		ClusterID:       "00000000-0000-4000-8000-000000000000",
		DestinationCIDR: "10.0.0.0/8",
	})
	if !client.IsNotFound(err) {
		t.Fatalf("expected IsNotFound for unknown cluster, got %v", err)
	}
}

func TestDeleteEgressDestroyedThenGone(t *testing.T) {
	srv := mockapi.New(testToken)
	srv.ClusterReadyAfterGETs = 1
	srv.EgressGoneAfterGETs = 1
	defer srv.Close()
	c := newTestClient(t, srv.URL, testToken)
	envID, clusterID := newEnvAndBusinessCluster(t, c, "lab-eg-del")

	eg, err := c.CreateEgress(context.Background(), envID, client.EgressSpec{
		ClusterID:       clusterID,
		DestinationCIDR: "10.0.0.0/8",
	})
	if err != nil {
		t.Fatalf("CreateEgress: %v", err)
	}
	if err := c.DeleteEgress(context.Background(), envID, eg.ID); err != nil {
		t.Fatalf("DeleteEgress: %v", err)
	}
	got, err := c.GetEgress(context.Background(), envID, eg.ID)
	if err != nil {
		t.Fatalf("GetEgress after delete: %v", err)
	}
	if got.Status != "destroyed" {
		t.Fatalf("expected destroyed after DELETE, got %q", got.Status)
	}
	if _, err := c.GetEgress(context.Background(), envID, eg.ID); !client.IsNotFound(err) {
		t.Fatalf("expected 404 after gone window, got %v", err)
	}
	if err := c.DeleteEgress(context.Background(), envID, eg.ID); err != nil {
		t.Fatalf("repeated DeleteEgress must tolerate 404: %v", err)
	}
	if srv.EgressCount() != 0 {
		t.Fatalf("expected 0 live egresses, got %d", srv.EgressCount())
	}
}

func TestEnvironmentDeleteCascadesNetworking(t *testing.T) {
	srv := mockapi.New(testToken)
	srv.ClusterReadyAfterGETs = 1
	defer srv.Close()
	c := newTestClient(t, srv.URL, testToken)
	envID, clusterID := newEnvAndBusinessCluster(t, c, "lab-net-cascade")

	if _, err := c.CreateIngress(context.Background(), envID, client.IngressSpec{
		ClusterID: clusterID, Service: "web", Port: 443, HostnamePrefix: "alice",
	}); err != nil {
		t.Fatalf("CreateIngress: %v", err)
	}
	if _, err := c.CreateEgress(context.Background(), envID, client.EgressSpec{
		ClusterID: clusterID, DestinationCIDR: "10.0.0.0/8",
	}); err != nil {
		t.Fatalf("CreateEgress: %v", err)
	}
	if err := c.DeleteEnvironment(context.Background(), envID); err != nil {
		t.Fatalf("DeleteEnvironment: %v", err)
	}
	if srv.IngressCount() != 0 {
		t.Fatalf("expected cascade to remove ingresses, got %d", srv.IngressCount())
	}
	if srv.EgressCount() != 0 {
		t.Fatalf("expected cascade to remove egresses, got %d", srv.EgressCount())
	}
}
