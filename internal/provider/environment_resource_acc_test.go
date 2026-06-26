package provider

// Framework acceptance tests (resource.Test) against the httptest contract
// mock — no real FCS API. Gated on TF_ACC by terraform-plugin-testing;
// requires a terraform CLI in PATH:
//
//	TF_ACC=1 go test ./internal/provider/ -v
//	make testacc

import (
	"fmt"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"

	"github.com/focusnetcloud/terraform-provider-fcs/internal/mockapi"
)

const accToken = "fcs_acc_token"

func protoV6Factories() map[string]func() (tfprotov6.ProviderServer, error) {
	return map[string]func() (tfprotov6.ProviderServer, error){
		"fcs": providerserver.NewProtocol6WithError(New("test")()),
	}
}

func accProviderConfig(endpoint, token string) string {
	return fmt.Sprintf(`
provider "fcs" {
  endpoint = %q
  token    = %q
}
`, endpoint, token)
}

func TestAccEnvironmentResourceBasic(t *testing.T) {
	srv := mockapi.New(accToken)
	defer srv.Close()

	config := accProviderConfig(srv.URL, accToken) + `
resource "fcs_environment" "test" {
  name        = "lab-acc"
  ttl_seconds = 3600

  labels = {
    workshop = "k8s-intro"
  }
}
`
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories(),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet("fcs_environment.test", "id"),
					resource.TestCheckResourceAttr("fcs_environment.test", "name", "lab-acc"),
					resource.TestCheckResourceAttr("fcs_environment.test", "ttl_seconds", "3600"),
					resource.TestCheckResourceAttr("fcs_environment.test", "labels.workshop", "k8s-intro"),
					resource.TestCheckResourceAttr("fcs_environment.test", "status", "active"),
					resource.TestCheckResourceAttrSet("fcs_environment.test", "ttl_expires_at"),
					resource.TestCheckResourceAttrSet("fcs_environment.test", "created_at"),
				),
			},
			{
				// Idempotent re-apply: same config must produce an empty plan.
				Config:   config,
				PlanOnly: true,
			},
		},
	})

	if srv.EnvironmentCount() != 0 {
		t.Fatalf("expected destroy to remove the environment, %d left", srv.EnvironmentCount())
	}
}

func TestAccEnvironmentResourcePersistentByDefault(t *testing.T) {
	srv := mockapi.New(accToken)
	defer srv.Close()

	// No ttl_seconds -> PERSISTENT: ttl_seconds stays unset and ttl_expires_at is
	// null (the provider invents no default, the server sets no expiry, the reaper
	// never fires).
	config := accProviderConfig(srv.URL, accToken) + `
resource "fcs_environment" "test" {
  name = "lab-persistent"
}
`
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories(),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr("fcs_environment.test", "ttl_seconds"),
					resource.TestCheckNoResourceAttr("fcs_environment.test", "ttl_expires_at"),
					resource.TestCheckResourceAttr("fcs_environment.test", "status", "active"),
				),
			},
			{
				// Re-apply with the same persistent config must produce an EMPTY
				// plan — proves the null ttl_seconds/ttl_expires_at is stable (no
				// "known after apply", no perpetual diff).
				Config:   config,
				PlanOnly: true,
			},
		},
	})
}

func TestAccEnvironmentResourceRequiresReplace(t *testing.T) {
	srv := mockapi.New(accToken)
	defer srv.Close()

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories(),
		Steps: []resource.TestStep{
			{
				Config: accProviderConfig(srv.URL, accToken) + `
resource "fcs_environment" "test" {
  name        = "lab-replace-a"
  ttl_seconds = 3600
}
`,
			},
			{
				// name change forces replacement
				Config: accProviderConfig(srv.URL, accToken) + `
resource "fcs_environment" "test" {
  name        = "lab-replace-b"
  ttl_seconds = 3600
}
`,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("fcs_environment.test", plancheck.ResourceActionReplace),
					},
				},
			},
			{
				// ttl change forces replacement
				Config: accProviderConfig(srv.URL, accToken) + `
resource "fcs_environment" "test" {
  name        = "lab-replace-b"
  ttl_seconds = 7200
}
`,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("fcs_environment.test", plancheck.ResourceActionReplace),
					},
				},
			},
		},
	})
}

// TestAccEnvironmentResourceDriftRemoved: the TTL reaper (simulated) removes
// the environment server-side; refresh must drop it from state and plan a
// re-create instead of erroring.
func TestAccEnvironmentResourceDriftRemoved(t *testing.T) {
	srv := mockapi.New(accToken)
	defer srv.Close()

	config := accProviderConfig(srv.URL, accToken) + `
resource "fcs_environment" "test" {
  name = "lab-drift"
}
`
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories(),
		Steps: []resource.TestStep{
			{Config: config},
			{
				PreConfig: func() {
					if !srv.DeleteByName("lab-drift") {
						t.Fatal("mock: lab-drift not found")
					}
				},
				RefreshState:       true,
				ExpectNonEmptyPlan: true,
			},
		},
	})
}

// TestAccEnvironmentResourceDestroyedDrift: the TTL reaper soft-destroys
// the environment server-side and the row stays readable as
// status=destroyed; refresh must drop it from state and plan a re-create
// instead of carrying the dead row forever.
func TestAccEnvironmentResourceDestroyedDrift(t *testing.T) {
	srv := mockapi.New(accToken)
	defer srv.Close()
	srv.EnvGoneAfterGETs = 1 << 30 // destroyed row stays readable (like the server DB)

	config := accProviderConfig(srv.URL, accToken) + `
resource "fcs_environment" "test" {
  name = "lab-destroyed-drift"
}
`
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories(),
		Steps: []resource.TestStep{
			{Config: config},
			{
				PreConfig: func() {
					if !srv.DestroyByName("lab-destroyed-drift") {
						t.Fatal("mock: lab-destroyed-drift not found")
					}
				},
				RefreshState:       true,
				ExpectNonEmptyPlan: true,
			},
		},
	})
}

func TestAccEnvironmentResourceQuotaExceeded(t *testing.T) {
	srv := mockapi.New(accToken)
	defer srv.Close()
	srv.MaxEnvs = 1

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories(),
		Steps: []resource.TestStep{
			{
				Config: accProviderConfig(srv.URL, accToken) + `
resource "fcs_environment" "one" {
  name = "lab-quota-1"
}

resource "fcs_environment" "two" {
  name       = "lab-quota-2"
  depends_on = [fcs_environment.one]
}
`,
				ExpectError: regexp.MustCompile(`(?s)HTTP 409.*QuotaExceeded`),
			},
		},
	})
}

func TestAccEnvironmentResourceUnauthorized(t *testing.T) {
	srv := mockapi.New(accToken)
	defer srv.Close()

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories(),
		Steps: []resource.TestStep{
			{
				Config: accProviderConfig(srv.URL, "fcs_wrong") + `
resource "fcs_environment" "test" {
  name = "lab-unauthorized"
}
`,
				ExpectError: regexp.MustCompile(`(?s)HTTP 401.*bearer token`),
			},
		},
	})
}
