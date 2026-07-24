package provider

// Acceptance tests (resource.Test) for fcs_vm and the fcs_quota /
// fcs_images data sources against the httptest contract mock — no real
// FCS API. Gated on TF_ACC; requires a terraform CLI in PATH:
//
//	TF_ACC=1 go test ./internal/provider/ -run TestAcc -v
//	make testacc

import (
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"

	"github.com/focusnetcloud/terraform-provider-fcs/internal/mockapi"
)

// fastVmMock returns a mock whose VMs are ready on the first GET and gone
// on the first GET after DELETE, so the 5s poll interval causes no sleeps.
// The full-lifecycle test keeps the slower defaults to exercise real poll
// rounds.
func fastVmMock(t *testing.T) *mockapi.Server {
	t.Helper()
	srv := mockapi.New(accToken)
	srv.VmReadyAfterGETs = 1
	srv.VmGoneAfterGETs = 0
	t.Cleanup(srv.Close)
	return srv
}

// TestAccVmFullLifecycle drives create -> poll-to-active -> re-plan
// idempotency -> running toggle (in-place power update, NOT replace) ->
// destroy -> gone-poll with the mock's default status machine
// (provisioning visible on the first GET), i.e. the provider really polls.
func TestAccVmFullLifecycle(t *testing.T) {
	srv := mockapi.New(accToken)
	defer srv.Close()

	vmConfig := func(running string) string {
		return accProviderConfig(srv.URL, accToken) + `
resource "fcs_environment" "test" {
  name = "lab-vm"
}

resource "fcs_vm" "test" {
  environment_id      = fcs_environment.test.id
  image               = "ubuntu-22.04"
  name                = "target"
  cloud_init_userdata = "#cloud-config\n{}"
  running             = ` + running + `

  timeouts = {
    create = "5m"
    delete = "5m"
  }
}
`
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories(),
		Steps: []resource.TestStep{
			{
				Config: vmConfig("true"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet("fcs_vm.test", "id"),
					resource.TestCheckResourceAttr("fcs_vm.test", "status", "active"),
					resource.TestCheckResourceAttr("fcs_vm.test", "name", "target"),
					resource.TestCheckResourceAttrSet("fcs_vm.test", "vm_ip"),
					// Server defaults materialized by the schema defaults.
					resource.TestCheckResourceAttr("fcs_vm.test", "cpu_cores", "2"),
					resource.TestCheckResourceAttr("fcs_vm.test", "memory_gb", "4"),
					resource.TestCheckResourceAttr("fcs_vm.test", "disk_gb", "20"),
					resource.TestCheckResourceAttr("fcs_vm.test", "nic_network", "tenant"),
					resource.TestCheckResourceAttr("fcs_vm.test", "running", "true"),
					resource.TestCheckResourceAttrPair(
						"fcs_vm.test", "environment_id",
						"fcs_environment.test", "id",
					),
				),
			},
			{
				// Re-plan idempotency: same config must produce an empty plan.
				Config:   vmConfig("true"),
				PlanOnly: true,
			},
			{
				// running=false is the ONLY in-place update (power stop) —
				// the plan must be an Update, never a Replace.
				Config: vmConfig("false"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("fcs_vm.test", plancheck.ResourceActionUpdate),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("fcs_vm.test", "status", "stopped"),
					resource.TestCheckResourceAttr("fcs_vm.test", "running", "false"),
				),
			},
			{
				// Toggle back: power start -> active.
				Config: vmConfig("true"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("fcs_vm.test", plancheck.ResourceActionUpdate),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("fcs_vm.test", "status", "active"),
					resource.TestCheckResourceAttr("fcs_vm.test", "running", "true"),
				),
			},
		},
	})

	// Destroy must have torn down the VM (gone-poll) and the environment.
	if srv.VmCount() != 0 {
		t.Fatalf("expected destroy to remove the vm, %d left", srv.VmCount())
	}
	if srv.EnvironmentCount() != 0 {
		t.Fatalf("expected destroy to remove the environment, %d left", srv.EnvironmentCount())
	}
}

// TestAccVmGeneratedNameStable: without a configured name the server
// generates one; UseStateForUnknown must keep the follow-up plan empty.
func TestAccVmGeneratedNameStable(t *testing.T) {
	srv := fastVmMock(t)

	config := accProviderConfig(srv.URL, accToken) + `
resource "fcs_environment" "test" {
  name = "lab-vm-gen"
}

resource "fcs_vm" "test" {
  environment_id = fcs_environment.test.id
  image          = "ubuntu-22.04"
}
`
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories(),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet("fcs_vm.test", "name"),
					resource.TestCheckResourceAttr("fcs_vm.test", "status", "active"),
				),
			},
			{
				Config:   config,
				PlanOnly: true,
			},
			{
				ResourceName:      "fcs_vm.test",
				Config:            config,
				ImportState:       true,
				ImportStateIdFunc: clusterCompositeImportID("fcs_vm.test"),
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
}

func TestAccVmHarborArtifactLifecycle(t *testing.T) {
	srv := fastVmMock(t)
	artifactID := "3e8e9a70-1657-47c8-a067-e6a0cf9ac797"
	config := accProviderConfig(srv.URL, accToken) + `
resource "fcs_environment" "test" {
  name = "lab-vm-harbor"
}

resource "fcs_vm" "test" {
  environment_id     = fcs_environment.test.id
  harbor_artifact_id = "` + artifactID + `"
  name               = "harbor-target"
}
`

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories(),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr(
						"fcs_vm.test", "image",
					),
					resource.TestCheckResourceAttr(
						"fcs_vm.test", "harbor_artifact_id", artifactID,
					),
					resource.TestCheckResourceAttr(
						"fcs_vm.test", "status", "active",
					),
				),
			},
			{
				Config:   config,
				PlanOnly: true,
			},
			{
				ResourceName:      "fcs_vm.test",
				Config:            config,
				ImportState:       true,
				ImportStateIdFunc: clusterCompositeImportID("fcs_vm.test"),
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
}

func TestAccVmRequiresReplaceImage(t *testing.T) {
	srv := fastVmMock(t)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories(),
		Steps: []resource.TestStep{
			{
				Config: accProviderConfig(srv.URL, accToken) + `
resource "fcs_environment" "test" {
  name = "lab-vm-replace"
}

resource "fcs_vm" "test" {
  environment_id = fcs_environment.test.id
  image          = "ubuntu-22.04"
}
`,
			},
			{
				// image change forces replacement (no in-place reimage).
				Config: accProviderConfig(srv.URL, accToken) + `
resource "fcs_environment" "test" {
  name = "lab-vm-replace"
}

resource "fcs_vm" "test" {
  environment_id = fcs_environment.test.id
  image          = "lab-base"
}
`,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("fcs_vm.test", plancheck.ResourceActionReplace),
					},
				},
				Check: resource.TestCheckResourceAttr("fcs_vm.test", "image", "lab-base"),
			},
		},
	})

	if srv.VmCount() != 0 {
		t.Fatalf("expected destroy to remove the vm, %d left", srv.VmCount())
	}
}

// TestAccVmUnknownImage422: the server validates the image against the
// catalog before any quota/insert work — the provider surfaces the 422.
func TestAccVmUnknownImage422(t *testing.T) {
	srv := fastVmMock(t)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories(),
		Steps: []resource.TestStep{
			{
				Config: accProviderConfig(srv.URL, accToken) + `
resource "fcs_environment" "test" {
  name = "lab-vm-422"
}

resource "fcs_vm" "test" {
  environment_id = fcs_environment.test.id
  image          = "nope"
}
`,
				ExpectError: regexp.MustCompile(`(?s)HTTP 422.*ImageNotAllowed`),
			},
		},
	})
}

func TestAccQuotaDataSource(t *testing.T) {
	srv := fastVmMock(t)
	srv.QuotaMaxVMs = 7

	config := accProviderConfig(srv.URL, accToken) + `
resource "fcs_environment" "test" {
  name = "lab-quota-ds"
}

resource "fcs_vm" "test" {
  environment_id = fcs_environment.test.id
  image          = "ubuntu-22.04"
  cpu_cores      = 4
  memory_gb      = 8
}

data "fcs_quota" "test" {
  depends_on = [fcs_vm.test]
}
`
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories(),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("data.fcs_quota.test", "max_vms", "7"),
					resource.TestCheckResourceAttr("data.fcs_quota.test", "used_vms", "1"),
					resource.TestCheckResourceAttr("data.fcs_quota.test", "used_environments", "1"),
					resource.TestCheckResourceAttr("data.fcs_quota.test", "used_vcpu", "4"),
					resource.TestCheckResourceAttr("data.fcs_quota.test", "used_ram_gb", "8"),
					resource.TestCheckResourceAttr("data.fcs_quota.test", "max_concurrent_environments", "25"),
					resource.TestCheckResourceAttr("data.fcs_quota.test", "max_public_ips", "2"),
					resource.TestCheckResourceAttr("data.fcs_quota.test", "used_public_ips", "0"),
				),
			},
		},
	})
}

func TestAccImagesDataSource(t *testing.T) {
	srv := fastVmMock(t)
	srv.Images = []mockapi.Image{
		{Name: "ubuntu-22.04", DisplayName: "Ubuntu 22.04 LTS", Source: "catalog"},
		{Name: "lab-base", Source: "env"},
	}

	config := accProviderConfig(srv.URL, accToken) + `
data "fcs_images" "test" {}
`
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories(),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("data.fcs_images.test", "images.#", "2"),
					resource.TestCheckResourceAttr("data.fcs_images.test", "images.0.name", "ubuntu-22.04"),
					resource.TestCheckResourceAttr("data.fcs_images.test", "images.0.display_name", "Ubuntu 22.04 LTS"),
					resource.TestCheckResourceAttr("data.fcs_images.test", "images.0.source", "catalog"),
					resource.TestCheckResourceAttr("data.fcs_images.test", "images.1.name", "lab-base"),
					resource.TestCheckResourceAttr("data.fcs_images.test", "images.1.source", "env"),
					resource.TestCheckNoResourceAttr("data.fcs_images.test", "images.1.display_name"),
				),
			},
		},
	})
}
