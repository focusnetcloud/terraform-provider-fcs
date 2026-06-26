package client_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/focusnetcloud/terraform-provider-fcs/internal/client"
	"github.com/focusnetcloud/terraform-provider-fcs/internal/mockapi"
)

func newEnvAndK8sNamespace(t *testing.T, c *client.Client, envName, kind string) (string, string) {
	t.Helper()
	env, err := c.CreateEnvironment(context.Background(), client.EnvironmentSpec{Name: envName})
	if err != nil {
		t.Fatalf("create environment: %v", err)
	}
	cl, err := c.CreateCluster(context.Background(), env.ID, client.ClusterSpec{Kind: kind})
	if err != nil {
		t.Fatalf("create %s cluster: %v", kind, err)
	}
	got, err := c.GetCluster(context.Background(), env.ID, cl.ID)
	if err != nil {
		t.Fatalf("GetCluster: %v", err)
	}
	if got.Status != "active" {
		t.Fatalf("expected %s cluster active, got %q", kind, got.Status)
	}
	return env.ID, cl.ID
}

func TestCreatePublishedAppNamespace(t *testing.T) {
	srv := mockapi.New(testToken)
	srv.ClusterReadyAfterGETs = 1
	defer srv.Close()
	c := newTestClient(t, srv.URL, testToken)
	_, namespaceID := newEnvAndK8sNamespace(t, c, "lab-pub-free", "namespace")

	app, err := c.CreatePublishedApp(context.Background(), namespaceID, client.PublishedAppSpec{
		AppSlug:     "shop",
		ServiceName: "web",
		ServicePort: 8080,
	})
	if err != nil {
		t.Fatalf("CreatePublishedApp: %v", err)
	}
	if app.ID == 0 {
		t.Fatal("expected non-zero published app id")
	}
	if app.Hostname != "shop-t8646.free.k8s.focusnet.de" {
		t.Fatalf("unexpected hostname %q", app.Hostname)
	}
	if app.VclusterNamespace != "default" {
		t.Fatalf("expected default vcluster namespace, got %q", app.VclusterNamespace)
	}
	if app.TLSMode != "auto" {
		t.Fatalf("expected tls_mode auto, got %q", app.TLSMode)
	}
	if srv.PublishedAppCount() != 1 {
		t.Fatalf("expected 1 published app, got %d", srv.PublishedAppCount())
	}
}

func TestCreatePublishedAppFlexWithPathPrefix(t *testing.T) {
	srv := mockapi.New(testToken)
	srv.ClusterReadyAfterGETs = 1
	defer srv.Close()
	c := newTestClient(t, srv.URL, testToken)
	_, namespaceID := newEnvAndK8sNamespace(t, c, "lab-pub-flex", "flex")
	pathPrefix := "/api"

	app, err := c.CreatePublishedApp(context.Background(), namespaceID, client.PublishedAppSpec{
		AppSlug:           "api",
		ServiceName:       "backend",
		ServicePort:       8081,
		VclusterNamespace: "apps",
		PathPrefix:        &pathPrefix,
	})
	if err != nil {
		t.Fatalf("CreatePublishedApp: %v", err)
	}
	if app.Hostname != "api-t8646.flex.k8s.focusnet.de" {
		t.Fatalf("unexpected hostname %q", app.Hostname)
	}
	if app.PathPrefix != "/api" {
		t.Fatalf("expected path_prefix /api, got %q", app.PathPrefix)
	}
}

func TestCreatePublishedAppIdempotentReapply(t *testing.T) {
	srv := mockapi.New(testToken)
	srv.ClusterReadyAfterGETs = 1
	defer srv.Close()
	c := newTestClient(t, srv.URL, testToken)
	_, namespaceID := newEnvAndK8sNamespace(t, c, "lab-pub-idem", "namespace")
	spec := client.PublishedAppSpec{
		AppSlug:     "shop",
		ServiceName: "web",
		ServicePort: 443,
	}

	first, err := c.CreatePublishedApp(context.Background(), namespaceID, spec)
	if err != nil {
		t.Fatalf("first CreatePublishedApp: %v", err)
	}
	second, err := c.CreatePublishedApp(context.Background(), namespaceID, spec)
	if err != nil {
		t.Fatalf("idempotent re-POST: %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("expected existing published app on re-POST, got %d != %d", first.ID, second.ID)
	}
	if srv.PublishedAppCount() != 1 {
		t.Fatalf("expected 1 published app after re-POST, got %d", srv.PublishedAppCount())
	}
}

func TestCreatePublishedAppHostnameConflict(t *testing.T) {
	srv := mockapi.New(testToken)
	srv.ClusterReadyAfterGETs = 1
	defer srv.Close()
	c := newTestClient(t, srv.URL, testToken)
	_, namespaceID := newEnvAndK8sNamespace(t, c, "lab-pub-conflict", "namespace")

	_, err := c.CreatePublishedApp(context.Background(), namespaceID, client.PublishedAppSpec{
		AppSlug:     "shop",
		ServiceName: "web",
		ServicePort: 8080,
	})
	if err != nil {
		t.Fatalf("CreatePublishedApp: %v", err)
	}
	_, err = c.CreatePublishedApp(context.Background(), namespaceID, client.PublishedAppSpec{
		AppSlug:     "shop",
		ServiceName: "api",
		ServicePort: 8080,
	})
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusConflict {
		t.Fatalf("expected APIError 409 for hostname conflict, got %v", err)
	}
	if srv.PublishedAppCount() != 1 {
		t.Fatalf("expected one published app after conflict, got %d", srv.PublishedAppCount())
	}
}

func TestDeletePublishedApp(t *testing.T) {
	srv := mockapi.New(testToken)
	srv.ClusterReadyAfterGETs = 1
	defer srv.Close()
	c := newTestClient(t, srv.URL, testToken)
	_, namespaceID := newEnvAndK8sNamespace(t, c, "lab-pub-del", "namespace")

	app, err := c.CreatePublishedApp(context.Background(), namespaceID, client.PublishedAppSpec{
		AppSlug:     "shop",
		ServiceName: "web",
		ServicePort: 443,
	})
	if err != nil {
		t.Fatalf("CreatePublishedApp: %v", err)
	}
	if err := c.DeletePublishedApp(context.Background(), namespaceID, app.ID); err != nil {
		t.Fatalf("DeletePublishedApp: %v", err)
	}
	if _, err := c.GetPublishedApp(context.Background(), namespaceID, app.ID); !client.IsNotFound(err) {
		t.Fatalf("expected IsNotFound after delete, got %v", err)
	}
	if err := c.DeletePublishedApp(context.Background(), namespaceID, app.ID); err != nil {
		t.Fatalf("repeated DeletePublishedApp must tolerate 404: %v", err)
	}
	if srv.PublishedAppCount() != 0 {
		t.Fatalf("expected no published apps, got %d", srv.PublishedAppCount())
	}
}

func TestClusterDeleteCascadesPublishedApps(t *testing.T) {
	srv := mockapi.New(testToken)
	srv.ClusterReadyAfterGETs = 1
	defer srv.Close()
	c := newTestClient(t, srv.URL, testToken)
	envID, namespaceID := newEnvAndK8sNamespace(t, c, "lab-pub-cascade", "namespace")

	if _, err := c.CreatePublishedApp(context.Background(), namespaceID, client.PublishedAppSpec{
		AppSlug:     "shop",
		ServiceName: "web",
		ServicePort: 443,
	}); err != nil {
		t.Fatalf("CreatePublishedApp: %v", err)
	}
	if err := c.DeleteCluster(context.Background(), envID, namespaceID); err != nil {
		t.Fatalf("DeleteCluster: %v", err)
	}
	if srv.PublishedAppCount() != 0 {
		t.Fatalf("expected cluster delete to remove published apps, got %d", srv.PublishedAppCount())
	}
}
