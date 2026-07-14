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
	"context"
	"fmt"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/focusnetcloud/terraform-provider-fcs/internal/client"
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

func clusterCompositeImportID(resourceName string) resource.ImportStateIdFunc {
	return func(state *terraform.State) (string, error) {
		res, ok := state.RootModule().Resources[resourceName]
		if !ok || res.Primary == nil {
			return "", fmt.Errorf("resource %s not found in state", resourceName)
		}
		envID := res.Primary.Attributes["environment_id"]
		clusterID := res.Primary.Attributes["id"]
		if envID == "" || clusterID == "" {
			return "", fmt.Errorf("resource %s has incomplete composite identity", resourceName)
		}
		return envID + "/" + clusterID, nil
	}
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

func TestAccBusinessClusterImportReadsSizingWithoutReplacement(t *testing.T) {
	srv := fastClusterMock(t)
	c, err := client.New(srv.URL, accToken)
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	env, err := c.CreateEnvironment(context.Background(), client.EnvironmentSpec{Name: "lab-biz-import"})
	if err != nil {
		t.Fatalf("create environment: %v", err)
	}
	cluster, err := c.CreateCluster(context.Background(), env.ID, client.ClusterSpec{
		Kind: "business", Size: "S", K8sVersion: "v1.35.3-k3s1",
	})
	if err != nil {
		t.Fatalf("create business cluster: %v", err)
	}

	config := accProviderConfig(srv.URL, accToken) + fmt.Sprintf(`
resource "fcs_business_cluster" "test" {
  environment_id = %q
  vcpu           = 4
  ram_gb         = 8
  storage_gb     = 100
}
`, env.ID)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories(),
		Steps: []resource.TestStep{
			{
				Config:             config,
				ResourceName:       "fcs_business_cluster.test",
				ImportState:        true,
				ImportStateId:      env.ID + "/" + cluster.ID,
				ImportStatePersist: true,
			},
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("fcs_business_cluster.test", "id", cluster.ID),
					resource.TestCheckResourceAttr("fcs_business_cluster.test", "vcpu", "4"),
					resource.TestCheckResourceAttr("fcs_business_cluster.test", "ram_gb", "8"),
					resource.TestCheckResourceAttr("fcs_business_cluster.test", "storage_gb", "100"),
				),
			},
			{Config: config, PlanOnly: true},
		},
	})
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
				ResourceName:      "fcs_namespace.test",
				ImportState:       true,
				ImportStateIdFunc: clusterCompositeImportID("fcs_namespace.test"),
				ImportStateVerify: true,
				ImportStateVerifyIgnore: []string{
					"timeouts",
				},
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

func TestAccBusinessClusterResizesInPlace(t *testing.T) {
	srv := fastClusterMock(t)
	srv.ClusterProvisioningDiagnostics = "initial provisioning diagnostics"
	srv.ClusterResizeProvisioningDiagnostics = "resize completed diagnostics"
	var clusterID string
	checkIdentity := func(state *terraform.State) error {
		res, ok := state.RootModule().Resources["fcs_business_cluster.test"]
		if !ok || res.Primary == nil {
			return fmt.Errorf("business cluster missing from state")
		}
		got := res.Primary.Attributes["id"]
		if clusterID == "" {
			clusterID = got
		} else if got != clusterID {
			return fmt.Errorf("cluster ID changed during resize: %s -> %s", clusterID, got)
		}
		if srv.ClusterCount() != 1 {
			return fmt.Errorf("resize must keep exactly one cluster, got %d", srv.ClusterCount())
		}
		return nil
	}

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
				Check: checkIdentity,
			},
			{
				// A size change patches the same cluster and waits for the new size.
				Config: accProviderConfig(srv.URL, accToken) + `
resource "fcs_environment" "test" {
  name = "lab-replace"
}

resource "fcs_business_cluster" "test" {
  environment_id = fcs_environment.test.id
  size           = "M"
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					checkIdentity,
					resource.TestCheckResourceAttr("fcs_business_cluster.test", "size", "M"),
					resource.TestCheckResourceAttr("fcs_business_cluster.test", "status", "active"),
					resource.TestCheckResourceAttr("fcs_business_cluster.test", "provisioning_diagnostics", "resize completed diagnostics"),
				),
			},
		},
	})

	if srv.ClusterCount() != 0 {
		t.Fatalf("expected destroy to remove the cluster, %d left", srv.ClusterCount())
	}
}

func TestAccBusinessClusterSwitchesSizingModesInPlace(t *testing.T) {
	srv := fastClusterMock(t)
	var clusterID string
	checkIdentity := func(state *terraform.State) error {
		res := state.RootModule().Resources["fcs_business_cluster.test"]
		if res == nil || res.Primary == nil {
			return fmt.Errorf("business cluster missing from state")
		}
		if clusterID == "" {
			clusterID = res.Primary.Attributes["id"]
		} else if got := res.Primary.Attributes["id"]; got != clusterID {
			return fmt.Errorf("cluster ID changed during sizing mode switch: %s -> %s", clusterID, got)
		}
		return nil
	}
	config := func(sizing string) string {
		return accProviderConfig(srv.URL, accToken) + `
resource "fcs_environment" "test" {
  name = "lab-mode-switch"
}
resource "fcs_business_cluster" "test" {
  environment_id = fcs_environment.test.id
` + sizing + "\n}\n"
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories(),
		Steps: []resource.TestStep{
			{
				Config: config("  vcpu = 4\n  ram_gb = 8\n  storage_gb = 100"),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkIdentity,
					resource.TestCheckResourceAttr("fcs_business_cluster.test", "vcpu", "4"),
				),
			},
			{
				Config: config(`  size = "M"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkIdentity,
					resource.TestCheckResourceAttr("fcs_business_cluster.test", "size", "M"),
					resource.TestCheckNoResourceAttr("fcs_business_cluster.test", "vcpu"),
				),
			},
			{
				Config: config("  vcpu = 6\n  ram_gb = 12\n  storage_gb = 300"),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkIdentity,
					resource.TestCheckResourceAttr("fcs_business_cluster.test", "vcpu", "6"),
					resource.TestCheckResourceAttr("fcs_business_cluster.test", "storage_gb", "300"),
				),
			},
		},
	})
}

func TestAccBusinessClusterRemovingSizeDoesNotSendEmptyPatch(t *testing.T) {
	srv := fastClusterMock(t)
	withSize := accProviderConfig(srv.URL, accToken) + `
resource "fcs_environment" "test" { name = "lab-remove-size" }
resource "fcs_business_cluster" "test" {
  environment_id = fcs_environment.test.id
  size = "S"
}
`
	withoutSizing := accProviderConfig(srv.URL, accToken) + `
resource "fcs_environment" "test" { name = "lab-remove-size" }
resource "fcs_business_cluster" "test" {
  environment_id = fcs_environment.test.id
}
`

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories(),
		Steps: []resource.TestStep{
			{Config: withSize},
			{
				Config: withoutSizing,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("fcs_business_cluster.test", "vcpu", "4"),
					resource.TestCheckResourceAttr("fcs_business_cluster.test", "ram_gb", "8"),
					resource.TestCheckResourceAttr("fcs_business_cluster.test", "storage_gb", "100"),
				),
			},
		},
	})
	if got := srv.ClusterPatchCount(); got != 0 {
		t.Fatalf("removing size without replacement sizing must refresh state, not PATCH an empty body; got %d requests", got)
	}
}

func TestAccNamespaceTimeoutOnlyUpdateDoesNotResize(t *testing.T) {
	srv := fastClusterMock(t)
	config := func(updateTimeout string) string {
		return accProviderConfig(srv.URL, accToken) + `
resource "fcs_environment" "test" {
  name = "lab-timeout-only"
}
resource "fcs_namespace" "test" {
  environment_id = fcs_environment.test.id
  timeouts = {
    update = "` + updateTimeout + `"
  }
}
`
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories(),
		Steps: []resource.TestStep{
			{Config: config("10m")},
			{Config: config("11m")},
		},
	})
	if got := srv.ClusterPatchCount(); got != 0 {
		t.Fatalf("timeout-only update must not PATCH a cluster, got %d requests", got)
	}
}

func TestAccBusinessClusterTimeoutOnlyUpdateDoesNotResize(t *testing.T) {
	srv := fastClusterMock(t)
	config := func(updateTimeout string) string {
		return accProviderConfig(srv.URL, accToken) + `
resource "fcs_environment" "test" { name = "lab-business-timeout-only" }
resource "fcs_business_cluster" "test" {
  environment_id = fcs_environment.test.id
  size = "S"
  timeouts = { update = "` + updateTimeout + `" }
}
`
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories(),
		Steps: []resource.TestStep{
			{Config: config("20m")},
			{Config: config("21m")},
		},
	})
	if got := srv.ClusterPatchCount(); got != 0 {
		t.Fatalf("timeout-only update must not PATCH a business cluster, got %d requests", got)
	}
}

func TestAccBusinessClusterOlderAPIWithoutSizingResolvesComputedValues(t *testing.T) {
	srv := fastClusterMock(t)
	srv.OmitClusterSizing = true
	config := func(updateTimeout string) string {
		return accProviderConfig(srv.URL, accToken) + `
resource "fcs_environment" "test" { name = "lab-old-api" }
resource "fcs_business_cluster" "test" {
  environment_id = fcs_environment.test.id
  timeouts = { update = "` + updateTimeout + `" }
}
`
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories(),
		Steps: []resource.TestStep{
			{
				Config: config("20m"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr("fcs_business_cluster.test", "vcpu"),
					resource.TestCheckNoResourceAttr("fcs_business_cluster.test", "ram_gb"),
					resource.TestCheckNoResourceAttr("fcs_business_cluster.test", "storage_gb"),
				),
			},
			{Config: config("21m")},
		},
	})
	if got := srv.ClusterPatchCount(); got != 0 {
		t.Fatalf("timeout-only update against an older API must not PATCH, got %d requests", got)
	}
}

func TestAccBusinessClusterRejectsStorageShrink(t *testing.T) {
	srv := fastClusterMock(t)
	config := func(storage int) string {
		return fmt.Sprintf(`%s
resource "fcs_environment" "test" { name = "lab-shrink" }
resource "fcs_business_cluster" "test" {
  environment_id = fcs_environment.test.id
  vcpu = 4
  ram_gb = 8
  storage_gb = %d
}
`, accProviderConfig(srv.URL, accToken), storage)
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories(),
		Steps: []resource.TestStep{
			{Config: config(100)},
			{Config: config(50), ExpectError: regexp.MustCompile(`(?s)HTTP 409.*StorageShrinkNotSupported`)},
		},
	})
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
