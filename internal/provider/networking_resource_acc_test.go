package provider

// Acceptance tests (resource.Test) for fcs_environment_ingress and
// fcs_environment_egress against the httptest contract mock — no real FCS API.
// Gated on TF_ACC; requires a terraform CLI in PATH:
//
//	TF_ACC=1 go test ./internal/provider/ -run TestAcc -v
//	make testacc
//
// A live ingress/egress is always status=provisioning (no active state), so
// there is no poll-to-active; the tests assert that status and exercise the
// poll-to-gone on destroy.

import (
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"

	"github.com/focusnetcloud/terraform-provider-fcs/internal/mockapi"
)

// fastNetworkingMock returns a mock whose business clusters are active on the
// first GET (so the ingress precondition holds quickly) and whose ingress/
// egress rows are gone on the first GET after DELETE (no poll sleeps). The
// full-lifecycle test keeps the slower defaults to exercise real poll rounds.
func fastNetworkingMock(t *testing.T) *mockapi.Server {
	t.Helper()
	srv := mockapi.New(accToken)
	srv.ClusterReadyAfterGETs = 1
	srv.ClusterGoneAfterGETs = 0
	srv.IngressGoneAfterGETs = 0
	srv.EgressGoneAfterGETs = 0
	t.Cleanup(srv.Close)
	return srv
}

// TestAccIngressFullLifecycle drives create (no poll-to-active: status stays
// provisioning) -> re-plan idempotency -> destroy (gone-poll) with the mock's
// default status machine, i.e. the cluster really polls to active and the
// ingress destroy really polls to gone.
func TestAccIngressFullLifecycle(t *testing.T) {
	srv := mockapi.New(accToken)
	defer srv.Close()

	config := accProviderConfig(srv.URL, accToken) + `
resource "fcs_environment" "test" {
  name = "lab-ing"
}

resource "fcs_business_cluster" "test" {
  environment_id = fcs_environment.test.id
  size           = "S"
}

resource "fcs_environment_ingress" "test" {
  environment_id  = fcs_environment.test.id
  cluster_id      = fcs_business_cluster.test.id
  service         = "web"
  port            = 8080
  hostname_prefix = "alice"

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
					resource.TestCheckResourceAttrSet("fcs_environment_ingress.test", "id"),
					resource.TestCheckResourceAttr("fcs_environment_ingress.test", "status", "provisioning"),
					resource.TestCheckResourceAttr("fcs_environment_ingress.test", "tls", "auto"),
					// public_url is the real L4 endpoint
					// https://<public_ip>:<port> (port 8080 -> explicit), not
					// the non-resolving hostname URL.
					resource.TestMatchResourceAttr("fcs_environment_ingress.test", "public_url",
						regexp.MustCompile(`^https://203\.0\.113\.\d+:8080$`)),
					resource.TestCheckResourceAttrPair(
						"fcs_environment_ingress.test", "cluster_id",
						"fcs_business_cluster.test", "id",
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

	if srv.IngressCount() != 0 {
		t.Fatalf("expected destroy to remove the ingress, %d left", srv.IngressCount())
	}
	if srv.ClusterCount() != 0 {
		t.Fatalf("expected destroy to remove the cluster, %d left", srv.ClusterCount())
	}
	if srv.EnvironmentCount() != 0 {
		t.Fatalf("expected destroy to remove the environment, %d left", srv.EnvironmentCount())
	}
}

// TestAccEgressFullLifecycle: create -> re-plan idempotency -> destroy.
func TestAccEgressFullLifecycle(t *testing.T) {
	srv := fastNetworkingMock(t)

	config := accProviderConfig(srv.URL, accToken) + `
resource "fcs_environment" "test" {
  name = "lab-eg"
}

resource "fcs_business_cluster" "test" {
  environment_id = fcs_environment.test.id
  size           = "S"
}

resource "fcs_environment_egress" "test" {
  environment_id   = fcs_environment.test.id
  cluster_id       = fcs_business_cluster.test.id
  destination_cidr = "10.0.0.0/8"
  protocol         = "tcp"
  port_range       = "443"
}
`
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories(),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet("fcs_environment_egress.test", "id"),
					resource.TestCheckResourceAttr("fcs_environment_egress.test", "status", "provisioning"),
					resource.TestCheckResourceAttr("fcs_environment_egress.test", "protocol", "tcp"),
					resource.TestCheckResourceAttr("fcs_environment_egress.test", "destination_cidr", "10.0.0.0/8"),
					resource.TestCheckResourceAttr("fcs_environment_egress.test", "port_range", "443"),
				),
			},
			{
				Config:   config,
				PlanOnly: true,
			},
		},
	})

	if srv.EgressCount() != 0 {
		t.Fatalf("expected destroy to remove the egress, %d left", srv.EgressCount())
	}
}

// TestAccEgressDefaultProtocolAny: protocol defaults to "any" when unset.
func TestAccEgressDefaultProtocolAny(t *testing.T) {
	srv := fastNetworkingMock(t)

	config := accProviderConfig(srv.URL, accToken) + `
resource "fcs_environment" "test" {
  name = "lab-eg-any"
}

resource "fcs_business_cluster" "test" {
  environment_id = fcs_environment.test.id
}

resource "fcs_environment_egress" "test" {
  environment_id   = fcs_environment.test.id
  cluster_id       = fcs_business_cluster.test.id
  destination_cidr = "1.1.1.1/32"
}
`
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories(),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("fcs_environment_egress.test", "protocol", "any"),
					resource.TestCheckNoResourceAttr("fcs_environment_egress.test", "port_range"),
				),
			},
			{
				Config:   config,
				PlanOnly: true,
			},
		},
	})
}

// TestAccIngressDestroyedDrift: the ingress is torn down server-side and the
// row stays readable as status=destroyed; refresh must drop it from state and
// plan a re-create instead of carrying the dead row forever.
func TestAccIngressDestroyedDrift(t *testing.T) {
	srv := mockapi.New(accToken)
	defer srv.Close()
	srv.ClusterReadyAfterGETs = 1
	srv.ClusterGoneAfterGETs = 1 << 30
	srv.IngressGoneAfterGETs = 1 << 30 // destroyed row stays readable (like the server DB)

	config := accProviderConfig(srv.URL, accToken) + `
resource "fcs_environment" "test" {
  name = "lab-ing-drift"
}

resource "fcs_business_cluster" "test" {
  environment_id = fcs_environment.test.id
}

resource "fcs_environment_ingress" "test" {
  environment_id  = fcs_environment.test.id
  cluster_id      = fcs_business_cluster.test.id
  service         = "web"
  port            = 443
  hostname_prefix = "alice"
}
`
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories(),
		Steps: []resource.TestStep{
			{Config: config},
			{
				PreConfig: func() {
					if n := srv.DestroyByName("lab-ing-drift"); !n {
						t.Fatal("mock: expected to destroy the environment (cascading the ingress)")
					}
				},
				// The environment and its ingress are gone server-side; refresh
				// must drop both from state and plan a full re-create.
				RefreshState:       true,
				ExpectNonEmptyPlan: true,
			},
		},
	})
}

// TestAccIngressRejectsInvalidHostnamePrefix: the schema validator rejects an
// uppercase/underscore prefix before any API call
// (^[a-z0-9]([a-z0-9-]*[a-z0-9])?$).
func TestAccIngressRejectsInvalidHostnamePrefix(t *testing.T) {
	srv := fastNetworkingMock(t)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories(),
		Steps: []resource.TestStep{
			{
				Config: accProviderConfig(srv.URL, accToken) + `
resource "fcs_environment_ingress" "test" {
  environment_id  = "irrelevant"
  cluster_id      = "irrelevant"
  service         = "web"
  port            = 443
  hostname_prefix = "Alice_1"
}
`,
				ExpectError: regexp.MustCompile(`(?s)Invalid Attribute Value Match`),
			},
		},
	})
}

// TestAccIngressRejectsLeadingTrailingHyphenPrefix: P2-#4 (provider/server
// regex parity). The provider previously used the laxer ^[a-z0-9-]+$, which
// accepted leading/trailing hyphens the API rejects at apply
// (^[a-z0-9]([a-z0-9-]*[a-z0-9])?$). The schema validator now matches the
// server and rejects them in `plan`, before any API call.
func TestAccIngressRejectsLeadingTrailingHyphenPrefix(t *testing.T) {
	for _, bad := range []string{"-alice", "alice-", "-"} {
		bad := bad
		t.Run(bad, func(t *testing.T) {
			srv := fastNetworkingMock(t)
			resource.Test(t, resource.TestCase{
				ProtoV6ProviderFactories: protoV6Factories(),
				Steps: []resource.TestStep{
					{
						Config: accProviderConfig(srv.URL, accToken) + `
resource "fcs_environment_ingress" "test" {
  environment_id  = "irrelevant"
  cluster_id      = "irrelevant"
  service         = "web"
  port            = 443
  hostname_prefix = "` + bad + `"
}
`,
						ExpectError: regexp.MustCompile(`(?s)Invalid Attribute Value Match`),
					},
				},
			})
		})
	}
}

// TestAccEgressRejectsInvalidPortRange: the provider rejects malformed
// port_range and port_range+protocol=any|icmp at plan time (ValidateConfig),
// mirroring the server's 422, instead of failing at apply.
func TestAccEgressRejectsInvalidPortRange(t *testing.T) {
	cases := []struct {
		name     string
		protocol string
		portRng  string
		errRe    string
	}{
		{"any_with_port", "any", "443", `port_range not allowed for this protocol`},
		{"icmp_with_port", "icmp", "443", `port_range not allowed for this protocol`},
		{"default_proto_with_port", "", "443", `port_range not allowed for this protocol`},
		{"descending", "tcp", "100-50", `port_range must be ascending`},
		{"non_numeric", "tcp", "foo", `Invalid port_range`},
		{"out_of_range", "udp", "70000", `port_range out of range`},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			srv := fastNetworkingMock(t)
			protoLine := ""
			if c.protocol != "" {
				protoLine = `  protocol         = "` + c.protocol + `"` + "\n"
			}
			resource.Test(t, resource.TestCase{
				ProtoV6ProviderFactories: protoV6Factories(),
				Steps: []resource.TestStep{
					{
						Config: accProviderConfig(srv.URL, accToken) + `
resource "fcs_environment_egress" "test" {
  environment_id   = "irrelevant"
  cluster_id       = "irrelevant"
  destination_cidr = "8.8.8.8/32"
` + protoLine + `  port_range       = "` + c.portRng + `"
}
`,
						ExpectError: regexp.MustCompile(`(?s)` + c.errRe),
					},
				},
			})
		})
	}
}

// TestAccIngressRequiresReplaceOnPortChange: every attribute forces replacement
// (no in-place update path exists).
// TestAccIngressRejectsTLSOff: only tls=auto is currently supported; the provider
// rejects "off" at plan (OneOf("auto")), before any API call.
func TestAccIngressRejectsTLSOff(t *testing.T) {
	srv := fastNetworkingMock(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories(),
		Steps: []resource.TestStep{
			{
				Config: accProviderConfig(srv.URL, accToken) + `
resource "fcs_environment_ingress" "test" {
  environment_id  = "irrelevant"
  cluster_id      = "irrelevant"
  service         = "web"
  port            = 443
  tls             = "off"
  hostname_prefix = "alice"
}
`,
				ExpectError: regexp.MustCompile(`(?s)Invalid Attribute Value Match`),
			},
		},
	})
}

func TestAccIngressRequiresReplaceOnPortChange(t *testing.T) {
	srv := fastNetworkingMock(t)

	step := func(port string) string {
		return accProviderConfig(srv.URL, accToken) + `
resource "fcs_environment" "test" {
  name = "lab-ing-replace"
}

resource "fcs_business_cluster" "test" {
  environment_id = fcs_environment.test.id
}

resource "fcs_environment_ingress" "test" {
  environment_id  = fcs_environment.test.id
  cluster_id      = fcs_business_cluster.test.id
  service         = "web"
  port            = ` + port + `
  hostname_prefix = "alice"
}
`
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6Factories(),
		Steps: []resource.TestStep{
			{Config: step("8080")},
			{
				Config: step("9090"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("fcs_environment_ingress.test", plancheck.ResourceActionReplace),
					},
				},
				Check: resource.TestCheckResourceAttr("fcs_environment_ingress.test", "port", "9090"),
			},
		},
	})

	if srv.IngressCount() != 0 {
		t.Fatalf("expected destroy to remove the ingress, %d left", srv.IngressCount())
	}
}
