package provider

// Acceptance tests (resource.Test) for fcs_published_app against the httptest
// contract mock. Gated on TF_ACC; requires a terraform CLI in PATH.

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"

	"github.com/focusnetcloud/terraform-provider-fcs/internal/mockapi"
)

func fastPublishedAppMock(t *testing.T) *mockapi.Server {
	t.Helper()
	srv := mockapi.New(accToken)
	srv.ClusterReadyAfterGETs = 1
	srv.ClusterGoneAfterGETs = 0
	t.Cleanup(srv.Close)
	return srv
}

func TestAccPublishedAppNamespaceLifecycle(t *testing.T) {
	srv := fastPublishedAppMock(t)

	config := accProviderConfig(srv.URL, accToken) + `
resource "fcs_environment" "test" {
  name = "lab-pub-free"
}

resource "fcs_namespace" "test" {
  environment_id = fcs_environment.test.id
}

resource "fcs_published_app" "test" {
  namespace_id = fcs_namespace.test.id
  app_slug     = "shop"
  service_name = "web"
  service_port = 8080
}
`
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories(),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet("fcs_published_app.test", "id"),
					resource.TestCheckResourceAttr("fcs_published_app.test", "app_slug", "shop"),
					resource.TestCheckResourceAttr("fcs_published_app.test", "service_name", "web"),
					resource.TestCheckResourceAttr("fcs_published_app.test", "service_port", "8080"),
					resource.TestCheckResourceAttr("fcs_published_app.test", "vcluster_namespace", "default"),
					resource.TestCheckNoResourceAttr("fcs_published_app.test", "path_prefix"),
					resource.TestCheckResourceAttr("fcs_published_app.test", "hostname", "shop-t8646.free.k8s.focusnet.de"),
					resource.TestCheckResourceAttr("fcs_published_app.test", "tls_mode", "auto"),
					resource.TestCheckResourceAttrPair(
						"fcs_published_app.test", "namespace_id",
						"fcs_namespace.test", "id",
					),
				),
			},
			{
				Config:   config,
				PlanOnly: true,
			},
		},
	})

	if srv.PublishedAppCount() != 0 {
		t.Fatalf("expected destroy to remove the published app, %d left", srv.PublishedAppCount())
	}
}

func TestAccPublishedAppFlexLifecycle(t *testing.T) {
	srv := fastPublishedAppMock(t)

	config := accProviderConfig(srv.URL, accToken) + `
resource "fcs_environment" "test" {
  name = "lab-pub-flex"
}

resource "fcs_flex_cluster" "test" {
  environment_id = fcs_environment.test.id
  vcpu           = 2
  ram_gb         = 4
  storage_gb     = 20
}

resource "fcs_published_app" "test" {
  namespace_id        = fcs_flex_cluster.test.id
  app_slug            = "api"
  service_name        = "backend"
  service_port        = 8081
  vcluster_namespace  = "apps"
  path_prefix         = "/api"
}
`
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories(),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet("fcs_published_app.test", "id"),
					resource.TestCheckResourceAttr("fcs_published_app.test", "hostname", "api-t8646.flex.k8s.focusnet.de"),
					resource.TestCheckResourceAttr("fcs_published_app.test", "vcluster_namespace", "apps"),
					resource.TestCheckResourceAttr("fcs_published_app.test", "path_prefix", "/api"),
					resource.TestCheckResourceAttrPair(
						"fcs_published_app.test", "namespace_id",
						"fcs_flex_cluster.test", "id",
					),
				),
			},
			{
				Config:   config,
				PlanOnly: true,
			},
		},
	})

	if srv.PublishedAppCount() != 0 {
		t.Fatalf("expected destroy to remove the published app, %d left", srv.PublishedAppCount())
	}
}
