package provider

// Acceptance tests (resource.Test) for fcs_dedicated_cluster against the
// httptest contract mock — no real FCS API. Gated on TF_ACC; requires a
// terraform CLI in PATH:
//
//	TF_ACC=1 go test ./internal/provider/ -run TestAccDedicated -v

import (
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"

	"github.com/focusnetcloud/terraform-provider-fcs/internal/mockapi"
)

// TestAccDedicatedClusterFullLifecycle drives create -> poll-to-active ->
// re-plan idempotency -> destroy -> gone-poll with the mock's default cluster
// status machine (provisioning visible on the first GET, destroyed readable
// after DELETE), i.e. the provider really polls.
func TestAccDedicatedClusterFullLifecycle(t *testing.T) {
	srv := mockapi.New(accToken)
	defer srv.Close()

	config := accProviderConfig(srv.URL, accToken) + `
resource "fcs_environment" "test" {
  name = "lab-ded"
}

resource "fcs_dedicated_cluster" "test" {
  environment_id = fcs_environment.test.id

  cp_nodes  = 3
  cp_vcpu   = 4
  cp_ram_gb = 8

  worker_nodes  = 2
  worker_vcpu   = 8
  worker_ram_gb = 16

  pvc_storage_gb = 200
  rke2_version   = "v1.31.5+rke2r1"

  timeouts = {
    create = "30m"
    delete = "20m"
  }
}
`
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories(),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet("fcs_dedicated_cluster.test", "id"),
					resource.TestCheckResourceAttr("fcs_dedicated_cluster.test", "status", "active"),
					resource.TestCheckResourceAttr("fcs_dedicated_cluster.test", "cp_nodes", "3"),
					resource.TestCheckResourceAttr("fcs_dedicated_cluster.test", "cp_vcpu", "4"),
					resource.TestCheckResourceAttr("fcs_dedicated_cluster.test", "cp_ram_gb", "8"),
					resource.TestCheckResourceAttr("fcs_dedicated_cluster.test", "worker_nodes", "2"),
					resource.TestCheckResourceAttr("fcs_dedicated_cluster.test", "worker_vcpu", "8"),
					resource.TestCheckResourceAttr("fcs_dedicated_cluster.test", "worker_ram_gb", "16"),
					resource.TestCheckResourceAttr("fcs_dedicated_cluster.test", "pvc_storage_gb", "200"),
					resource.TestCheckResourceAttr("fcs_dedicated_cluster.test", "rke2_version", "v1.31.5+rke2r1"),
					// Contract: api_server_url = https://<public_ip>:6443 once active.
					resource.TestMatchResourceAttr("fcs_dedicated_cluster.test", "api_server_url",
						regexp.MustCompile(`^https://[0-9.]+:6443$`)),
					resource.TestCheckResourceAttrSet("fcs_dedicated_cluster.test", "cluster_cidr"),
					resource.TestCheckResourceAttrSet("fcs_dedicated_cluster.test", "service_cidr"),
					resource.TestCheckResourceAttrPair(
						"fcs_dedicated_cluster.test", "environment_id",
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

// TestAccDedicatedClusterDefaults: the optional node-pool attributes default
// server-agnostically in the schema (cp_nodes=3, worker_*=0, pvc_storage_gb=100)
// so a minimal config (only the required cp_vcpu/cp_ram_gb) is stable and
// re-plans empty.
func TestAccDedicatedClusterDefaults(t *testing.T) {
	srv := mockapi.New(accToken)
	srv.ClusterReadyAfterGETs = 1
	srv.ClusterGoneAfterGETs = 0
	t.Cleanup(srv.Close)

	config := accProviderConfig(srv.URL, accToken) + `
resource "fcs_environment" "test" {
  name = "lab-ded-defaults"
}

resource "fcs_dedicated_cluster" "test" {
  environment_id = fcs_environment.test.id
  cp_vcpu        = 2
  cp_ram_gb      = 8
}
`
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories(),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("fcs_dedicated_cluster.test", "status", "active"),
					resource.TestCheckResourceAttr("fcs_dedicated_cluster.test", "cp_nodes", "3"),
					resource.TestCheckResourceAttr("fcs_dedicated_cluster.test", "worker_nodes", "0"),
					resource.TestCheckResourceAttr("fcs_dedicated_cluster.test", "worker_vcpu", "0"),
					resource.TestCheckResourceAttr("fcs_dedicated_cluster.test", "worker_ram_gb", "0"),
					resource.TestCheckResourceAttr("fcs_dedicated_cluster.test", "pvc_storage_gb", "100"),
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

// TestAccDedicatedClusterRequiresReplaceWorkerNodes: there is no in-place
// node-pool resize — changing worker_nodes forces a full recreation.
func TestAccDedicatedClusterRequiresReplaceWorkerNodes(t *testing.T) {
	srv := mockapi.New(accToken)
	srv.ClusterReadyAfterGETs = 1
	srv.ClusterGoneAfterGETs = 0
	t.Cleanup(srv.Close)

	base := accProviderConfig(srv.URL, accToken) + `
resource "fcs_environment" "test" {
  name = "lab-ded-replace"
}

resource "fcs_dedicated_cluster" "test" {
  environment_id = fcs_environment.test.id
  cp_vcpu        = 2
  cp_ram_gb      = 8
  worker_nodes   = 1
  worker_vcpu    = 4
  worker_ram_gb  = 8
}
`
	scaled := accProviderConfig(srv.URL, accToken) + `
resource "fcs_environment" "test" {
  name = "lab-ded-replace"
}

resource "fcs_dedicated_cluster" "test" {
  environment_id = fcs_environment.test.id
  cp_vcpu        = 2
  cp_ram_gb      = 8
  worker_nodes   = 3
  worker_vcpu    = 4
  worker_ram_gb  = 8
}
`
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories(),
		Steps: []resource.TestStep{
			{Config: base},
			{
				Config: scaled,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("fcs_dedicated_cluster.test", plancheck.ResourceActionReplace),
					},
				},
				Check: resource.TestCheckResourceAttr("fcs_dedicated_cluster.test", "worker_nodes", "3"),
			},
		},
	})

	if srv.ClusterCount() != 0 {
		t.Fatalf("expected destroy to remove the cluster, %d left", srv.ClusterCount())
	}
}

// TestAccDedicatedClusterRejectsInvalidCPNodes: cp_nodes accepts only 1 or 3
// (single-node / HA); any other value fails plan-time validation.
func TestAccDedicatedClusterRejectsInvalidCPNodes(t *testing.T) {
	srv := mockapi.New(accToken)
	srv.ClusterReadyAfterGETs = 1
	t.Cleanup(srv.Close)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories(),
		Steps: []resource.TestStep{
			{
				Config: accProviderConfig(srv.URL, accToken) + `
resource "fcs_dedicated_cluster" "test" {
  environment_id = "irrelevant"
  cp_nodes       = 2
  cp_vcpu        = 2
  cp_ram_gb      = 8
}
`,
				ExpectError: regexp.MustCompile(`(?s)Invalid Attribute Value Match`),
			},
		},
	})
}
