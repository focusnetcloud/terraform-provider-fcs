package provider

// Acceptance coverage for the standalone IaaS product path against the
// httptest contract mock: Terraform creates a vDC, realizes a VM network, and
// then deploys a VM bound to that network.

import (
	"fmt"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/focusnetcloud/terraform-provider-fcs/internal/mockapi"
)

func TestAccIaasVdcNetworkVmFullLifecycle(t *testing.T) {
	srv := mockapi.New(accToken)
	defer srv.Close()
	srv.IaasNetworkReadyAfterGETs = 1
	srv.VmReadyAfterGETs = 1
	srv.VmGoneAfterGETs = 0

	const envName = "lab-iaas-vm"
	const vmName = "web01"

	config := accProviderConfig(srv.URL, accToken) + `
resource "fcs_environment" "test" {
  name = "lab-iaas-vm"
}

resource "fcs_iaas_vdc" "test" {
  environment_id = fcs_environment.test.id
  name           = "prod"
}

resource "fcs_iaas_network" "web" {
  environment_id = fcs_environment.test.id
  vdc_id         = fcs_iaas_vdc.test.id
  name           = "web"
  cidr           = "10.80.1.0/24"
  policy_default = "private"

  timeouts = {
    create = "5m"
    delete = "5m"
  }
}

resource "fcs_vm" "web01" {
  environment_id = fcs_environment.test.id
  image          = "ubuntu-22.04"
  name           = "web01"
  nic_network    = "iaas"
  vdc_id         = fcs_iaas_vdc.test.id
  network_id     = fcs_iaas_network.web.id

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
					resource.TestCheckResourceAttr("fcs_iaas_vdc.test", "status", "planned"),
					resource.TestCheckResourceAttr("fcs_iaas_vdc.test", "gateway_scope", "iaas_vdc"),
					resource.TestCheckResourceAttrSet("fcs_iaas_vdc.test", "scope_key"),
					resource.TestCheckResourceAttrSet("fcs_iaas_vdc.test", "gateway_vpc_name"),
					resource.TestCheckResourceAttrSet("fcs_iaas_vdc.test", "gateway_name"),
					resource.TestCheckResourceAttr("fcs_iaas_network.web", "status", "active"),
					resource.TestCheckResourceAttr("fcs_iaas_network.web", "policy_default", "private"),
					resource.TestCheckResourceAttrSet("fcs_iaas_network.web", "harvester_namespace"),
					resource.TestCheckResourceAttrSet("fcs_iaas_network.web", "harvester_nad_name"),
					resource.TestCheckResourceAttrSet("fcs_iaas_network.web", "kubeovn_subnet_name"),
					resource.TestCheckResourceAttrSet("fcs_iaas_network.web", "default_gateway_ip"),
					resource.TestCheckResourceAttr("fcs_vm.web01", "status", "active"),
					resource.TestCheckResourceAttr("fcs_vm.web01", "name", vmName),
					resource.TestCheckResourceAttr("fcs_vm.web01", "nic_network", "iaas"),
					resource.TestCheckResourceAttrPair("fcs_vm.web01", "vdc_id", "fcs_iaas_vdc.test", "id"),
					resource.TestCheckResourceAttrPair("fcs_vm.web01", "network_id", "fcs_iaas_network.web", "id"),
					resource.TestCheckResourceAttrSet("fcs_vm.web01", "vm_ip"),
					testCheckIaasVmBindingPayload(
						srv,
						envName,
						vmName,
						"fcs_iaas_vdc.test",
						"fcs_iaas_network.web",
					),
				),
			},
			{
				Config:   config,
				PlanOnly: true,
			},
		},
	})

	if srv.VmCount() != 0 {
		t.Fatalf("expected destroy to remove the VM, %d left", srv.VmCount())
	}
	if srv.IaasNetworkCount() != 0 {
		t.Fatalf("expected destroy to remove the IaaS network, %d left", srv.IaasNetworkCount())
	}
	if srv.IaasVdcCount() != 0 {
		t.Fatalf("expected destroy to remove the IaaS-vDC, %d left", srv.IaasVdcCount())
	}
	if srv.EnvironmentCount() != 0 {
		t.Fatalf("expected destroy to remove the environment, %d left", srv.EnvironmentCount())
	}
}

func testCheckIaasVmBindingPayload(
	srv *mockapi.Server,
	envName string,
	vmName string,
	vdcAddress string,
	networkAddress string,
) resource.TestCheckFunc {
	return func(state *terraform.State) error {
		spec, ok := srv.VmSpecByName(envName, vmName)
		if !ok {
			return fmt.Errorf("expected VM %s/%s create payload in mock", envName, vmName)
		}
		if spec.NICNetwork != "iaas" {
			return fmt.Errorf("expected nic_network=iaas, got %q", spec.NICNetwork)
		}
		vdcID, err := stateResourceID(state, vdcAddress)
		if err != nil {
			return err
		}
		if spec.VdcID != vdcID {
			return fmt.Errorf("expected vdc_id %q in VM create payload, got %q", vdcID, spec.VdcID)
		}
		networkID, err := stateResourceID(state, networkAddress)
		if err != nil {
			return err
		}
		if spec.NetworkID != networkID {
			return fmt.Errorf("expected network_id %q in VM create payload, got %q", networkID, spec.NetworkID)
		}
		return nil
	}
}

func stateResourceID(state *terraform.State, address string) (string, error) {
	resourceState, ok := state.RootModule().Resources[address]
	if !ok || resourceState == nil || resourceState.Primary == nil {
		return "", fmt.Errorf("expected Terraform state for %s", address)
	}
	id := resourceState.Primary.Attributes["id"]
	if id == "" {
		return "", fmt.Errorf("expected non-empty id for %s", address)
	}
	return id, nil
}
