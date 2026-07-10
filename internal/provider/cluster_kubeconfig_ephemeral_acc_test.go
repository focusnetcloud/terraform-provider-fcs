package provider

// Acceptance tests for the fcs_cluster_kubeconfig EPHEMERAL resource against
// the httptest contract mock — no real FCS API. Gated on TF_ACC; requires a
// terraform CLI >= 1.10 in PATH (ephemeral resources):
//
//	TF_ACC=1 go test ./internal/provider/ -run TestAcc -v
//	make testacc
//
// Two complementary proofs:
//
//   - The echo provider (terraform-plugin-testing/echoprovider) copies the
//     ephemeral values into a throwaway test resource so the Open roundtrip
//     (field mapping through a real terraform CLI run) can be asserted.
//   - A config WITHOUT echo proves the no-persist guarantee: the mock's
//     mint counter shows Open really ran, while the final state contains
//     neither an fcs_cluster_kubeconfig entry nor any credential material.

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/echoprovider"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/statecheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
	"github.com/hashicorp/terraform-plugin-testing/tfversion"
)

// ephemeralVersionChecks gates ephemeral-resource tests on Terraform >= 1.10.
func ephemeralVersionChecks() []tfversion.TerraformVersionCheck {
	return []tfversion.TerraformVersionCheck{
		tfversion.SkipBelow(tfversion.Version1_10_0),
	}
}

// TestAccClusterKubeconfigEphemeralOpenRoundtrip drives the ephemeral
// resource through a real terraform run and asserts the minted credential
// fields via the echo test provider (the only sanctioned way to observe
// ephemeral values in an acceptance test).
func TestAccClusterKubeconfigEphemeralOpenRoundtrip(t *testing.T) {
	srv := fastClusterMock(t)

	factories := protoV6Factories()
	factories["echo"] = echoprovider.NewProviderServer()

	config := accProviderConfig(srv.URL, accToken) + `
resource "fcs_environment" "test" {
  name = "lab-kubeconfig"
}

resource "fcs_business_cluster" "test" {
  environment_id = fcs_environment.test.id
}

ephemeral "fcs_cluster_kubeconfig" "test" {
  environment_id = fcs_environment.test.id
  cluster_id     = fcs_business_cluster.test.id
}

provider "echo" {
  data = ephemeral.fcs_cluster_kubeconfig.test
}

resource "echo" "test" {}
`
	data := tfjsonpath.New("data")
	resource.Test(t, resource.TestCase{
		TerraformVersionChecks:   ephemeralVersionChecks(),
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{
				Config: config,
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue("echo.test", data.AtMapKey("api_server_url"),
						knownvalue.StringRegexp(regexp.MustCompile(`^https://.+:6443$`))),
					statecheck.ExpectKnownValue("echo.test", data.AtMapKey("sa_token"),
						knownvalue.StringRegexp(regexp.MustCompile(`^sa\..+`))),
					statecheck.ExpectKnownValue("echo.test", data.AtMapKey("expires_at"),
						knownvalue.StringRegexp(regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T`))),
					statecheck.ExpectKnownValue("echo.test", data.AtMapKey("kubeconfig"),
						knownvalue.StringFunc(func(v string) error {
							if !strings.Contains(v, "apiVersion: v1") || !strings.Contains(v, "kind: Config") {
								return fmt.Errorf("kubeconfig does not look like a kubeconfig document")
							}
							return nil
						})),
				},
			},
		},
	})

	if srv.KubeconfigMintCount() == 0 {
		t.Fatal("expected at least one kubeconfig mint on the mock")
	}
}

// TestAccClusterKubeconfigEphemeralNotPersisted is the P1 regression test:
// after a full apply that demonstrably minted credentials (mock counter),
// the state file must contain no fcs_cluster_kubeconfig entry and no
// credential material anywhere — this is the property the previous stateful
// data source violated.
func TestAccClusterKubeconfigEphemeralNotPersisted(t *testing.T) {
	srv := fastClusterMock(t)

	config := accProviderConfig(srv.URL, accToken) + `
resource "fcs_environment" "test" {
  name = "lab-kc-nopersist"
}

resource "fcs_business_cluster" "test" {
  environment_id = fcs_environment.test.id
}

ephemeral "fcs_cluster_kubeconfig" "test" {
  environment_id = fcs_environment.test.id
  cluster_id     = fcs_business_cluster.test.id
}
`
	resource.Test(t, resource.TestCase{
		TerraformVersionChecks:   ephemeralVersionChecks(),
		ProtoV6ProviderFactories: protoV6Factories(),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					func(s *terraform.State) error {
						if srv.KubeconfigMintCount() == 0 {
							return fmt.Errorf("ephemeral open never minted credentials — the no-persist assertion would be vacuous")
						}
						return assertNoCredentialsInState(s)
					},
				),
			},
		},
	})
}

// assertNoCredentialsInState walks every resource instance in the final
// state and fails if the ephemeral type leaked into it or any attribute
// value carries credential material (kubeconfig document markers or the
// mock's "sa." token prefix).
func assertNoCredentialsInState(s *terraform.State) error {
	for _, mod := range s.Modules {
		for addr, rs := range mod.Resources {
			if rs.Type == "fcs_cluster_kubeconfig" {
				return fmt.Errorf("state must not contain any fcs_cluster_kubeconfig entry, found %s", addr)
			}
			for _, inst := range append([]*terraform.InstanceState{rs.Primary}, rs.Deposed...) {
				if inst == nil {
					continue
				}
				for key, val := range inst.Attributes {
					if strings.Contains(val, "kind: Config") ||
						strings.Contains(val, "apiVersion: v1") ||
						strings.HasPrefix(val, "sa.") {
						return fmt.Errorf("state leaks credential material in %s.%s", addr, key)
					}
				}
			}
		}
		for name, out := range mod.Outputs {
			val := fmt.Sprintf("%v", out.Value)
			if strings.Contains(val, "kind: Config") || strings.HasPrefix(val, "sa.") {
				return fmt.Errorf("state output %q leaks credential material", name)
			}
		}
	}
	return nil
}
