package provider

// Acceptance tests (resource.Test) for the cluster resources against the
// httptest contract mock — no real FCS API. Gated on TF_ACC; requires a
// terraform CLI in PATH:
//
//	TF_ACC=1 go test ./internal/provider/ -run TestAcc -v
//	make testacc
//
// The fcs_cluster_kubeconfig ephemeral resource is covered in
// cluster_kubeconfig_ephemeral_acc_test.go.

import (
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"

	"github.com/focusnetcloud/terraform-provider-fcs/internal/mockapi"
)

// fastClusterMock returns a mock whose clusters are active on the first GET
// and gone on the first GET after DELETE, so the 5s poll interval causes no
// sleeps. The business full-lifecycle test keeps the slower defaults to
// exercise real poll rounds.
func fastClusterMock(t *testing.T) *mockapi.Server {
	t.Helper()
	srv := mockapi.New(accToken)
	srv.ClusterReadyAfterGETs = 1
	srv.ClusterGoneAfterGETs = 0
	t.Cleanup(srv.Close)
	return srv
}

// TestAccBusinessClusterFullLifecycle drives create -> poll-to-active ->
// re-plan idempotency -> destroy -> gone-poll with the mock's default
// status machine (provisioning visible on the first GET, destroyed
// readable after DELETE), i.e. the provider really polls.
func TestAccBusinessClusterFullLifecycle(t *testing.T) {
	srv := mockapi.New(accToken)
	defer srv.Close()

	config := accProviderConfig(srv.URL, accToken) + `
resource "fcs_environment" "test" {
  name = "lab-biz"
}

resource "fcs_business_cluster" "test" {
  environment_id = fcs_environment.test.id
  size           = "S"
  k8s_version    = "v1.35.3-k3s1"

  timeouts = {
    create = "5m"
    delete = "5m"
  }
}
`
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories(),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet("fcs_business_cluster.test", "id"),
					resource.TestCheckResourceAttr("fcs_business_cluster.test", "status", "active"),
					resource.TestCheckResourceAttr("fcs_business_cluster.test", "size", "S"),
					resource.TestCheckResourceAttr("fcs_business_cluster.test", "k8s_version", "v1.35.3-k3s1"),
					resource.TestCheckResourceAttrSet("fcs_business_cluster.test", "api_server_url"),
					resource.TestCheckResourceAttrSet("fcs_business_cluster.test", "cluster_cidr"),
					resource.TestCheckResourceAttrSet("fcs_business_cluster.test", "service_cidr"),
					resource.TestCheckResourceAttrPair(
						"fcs_business_cluster.test", "environment_id",
						"fcs_environment.test", "id",
					),
				),
			},
			{
				// Re-plan idempotency: same config must produce an empty plan.
				Config:   config,
				PlanOnly: true,
			},
		},
	})

	// Destroy must have torn down cluster (gone-poll) and environment.
	if srv.ClusterCount() != 0 {
		t.Fatalf("expected destroy to remove the cluster, %d left", srv.ClusterCount())
	}
	if srv.EnvironmentCount() != 0 {
		t.Fatalf("expected destroy to remove the environment, %d left", srv.EnvironmentCount())
	}
}

func TestAccFlexClusterLifecycle(t *testing.T) {
	srv := fastClusterMock(t)

	config := accProviderConfig(srv.URL, accToken) + `
resource "fcs_environment" "test" {
  name = "lab-flex"
}

resource "fcs_flex_cluster" "test" {
  environment_id = fcs_environment.test.id

  # custom sizing instead of size
  vcpu       = 4
  ram_gb     = 8
  storage_gb = 50
}
`
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories(),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet("fcs_flex_cluster.test", "id"),
					resource.TestCheckResourceAttr("fcs_flex_cluster.test", "status", "active"),
					resource.TestCheckResourceAttr("fcs_flex_cluster.test", "vcpu", "4"),
					resource.TestCheckResourceAttr("fcs_flex_cluster.test", "ram_gb", "8"),
					resource.TestCheckResourceAttr("fcs_flex_cluster.test", "storage_gb", "50"),
					resource.TestCheckNoResourceAttr("fcs_flex_cluster.test", "size"),
					resource.TestCheckResourceAttrSet("fcs_flex_cluster.test", "api_server_url"),
				),
			},
			{
				Config:   config,
				PlanOnly: true,
			},
		},
	})

	if srv.ClusterCount() != 0 {
		t.Fatalf("expected destroy to remove the cluster, %d left", srv.ClusterCount())
	}
}

func TestAccNamespaceLifecycle(t *testing.T) {
	srv := fastClusterMock(t)

	config := accProviderConfig(srv.URL, accToken) + `
resource "fcs_environment" "test" {
  name = "lab-ns"
}

resource "fcs_namespace" "test" {
  environment_id = fcs_environment.test.id
}
`
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories(),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet("fcs_namespace.test", "id"),
					resource.TestCheckResourceAttr("fcs_namespace.test", "status", "active"),
					resource.TestCheckResourceAttrSet("fcs_namespace.test", "api_server_url"),
					resource.TestCheckResourceAttrSet("fcs_namespace.test", "cluster_cidr"),
					resource.TestCheckResourceAttrSet("fcs_namespace.test", "service_cidr"),
				),
			},
			{
				Config:   config,
				PlanOnly: true,
			},
		},
	})

	if srv.ClusterCount() != 0 {
		t.Fatalf("expected destroy to remove the namespace, %d left", srv.ClusterCount())
	}
}

// TestAccBusinessClusterDestroyedDrift: the cluster is torn down server-side
// (reaper/offboarding) and the row stays readable as status=destroyed;
// refresh must drop it from state and plan a re-create instead of carrying
// the dead row forever.
func TestAccBusinessClusterDestroyedDrift(t *testing.T) {
	srv := mockapi.New(accToken)
	defer srv.Close()
	srv.ClusterReadyAfterGETs = 1
	srv.ClusterGoneAfterGETs = 1 << 30 // destroyed row stays readable (like the server DB)

	config := accProviderConfig(srv.URL, accToken) + `
resource "fcs_environment" "test" {
  name = "lab-cl-drift"
}

resource "fcs_business_cluster" "test" {
  environment_id = fcs_environment.test.id
}
`
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories(),
		Steps: []resource.TestStep{
			{Config: config},
			{
				PreConfig: func() {
					if n := srv.DestroyClustersOfEnv("lab-cl-drift"); n != 1 {
						t.Fatalf("mock: expected to destroy 1 cluster, got %d", n)
					}
				},
				RefreshState:       true,
				ExpectNonEmptyPlan: true,
			},
		},
	})
}

// TestAccNamespaceRejectsSizing: fcs_namespace has no sizing attributes —
// the product tier fixes them.
func TestAccNamespaceRejectsSizing(t *testing.T) {
	srv := fastClusterMock(t)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories(),
		Steps: []resource.TestStep{
			{
				Config: accProviderConfig(srv.URL, accToken) + `
resource "fcs_namespace" "test" {
  environment_id = "irrelevant"
  size           = "S"
}
`,
				ExpectError: regexp.MustCompile(`(?s)Unsupported argument`),
			},
		},
	})
}

func TestAccBusinessClusterRequiresReplaceSize(t *testing.T) {
	srv := fastClusterMock(t)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories(),
		Steps: []resource.TestStep{
			{
				Config: accProviderConfig(srv.URL, accToken) + `
resource "fcs_environment" "test" {
  name = "lab-replace"
}

resource "fcs_business_cluster" "test" {
  environment_id = fcs_environment.test.id
  size           = "S"
}
`,
			},
			{
				// size change forces replacement (no resize path exists).
				Config: accProviderConfig(srv.URL, accToken) + `
resource "fcs_environment" "test" {
  name = "lab-replace"
}

resource "fcs_business_cluster" "test" {
  environment_id = fcs_environment.test.id
  size           = "M"
}
`,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("fcs_business_cluster.test", plancheck.ResourceActionReplace),
					},
				},
				Check: resource.TestCheckResourceAttr("fcs_business_cluster.test", "size", "M"),
			},
		},
	})

	if srv.ClusterCount() != 0 {
		t.Fatalf("expected destroy to remove the cluster, %d left", srv.ClusterCount())
	}
}

// TestAccBusinessClusterSizeConflictsWithCustom: size and custom sizing are
// mutually exclusive — validation must fail before any API call.
func TestAccBusinessClusterSizeConflictsWithCustom(t *testing.T) {
	srv := fastClusterMock(t)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories(),
		Steps: []resource.TestStep{
			{
				Config: accProviderConfig(srv.URL, accToken) + `
resource "fcs_business_cluster" "test" {
  environment_id = "irrelevant"
  size           = "S"
  vcpu           = 4
}
`,
				ExpectError: regexp.MustCompile(`(?s)Invalid Attribute Combination`),
			},
		},
	})
}
