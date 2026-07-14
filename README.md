# terraform-provider-fcs

Terraform provider for the FCS platform, built with
[terraform-plugin-framework](https://github.com/hashicorp/terraform-plugin-framework).
It manages tenant-scoped FCS resources through the public FCS API.

Current scope:

- provider configuration with `FCS_TOKEN` fallback
- `fcs_environment` TTL envelope
- `fcs_vm`
- `fcs_namespace`
- `fcs_flex_cluster`
- `fcs_business_cluster`
- `fcs_dedicated_cluster`
- `fcs_iaas_vdc`
- `fcs_iaas_network`
- `fcs_published_app`
- `fcs_environment_ingress`
- `fcs_environment_egress`
- `fcs_quota` and `fcs_images` data sources
- ephemeral `fcs_cluster_kubeconfig` credentials for Terraform >= 1.10

> Distributed from `github.com/focusnetcloud/terraform-provider-fcs` and
> published to the public Terraform Registry as `focusnetcloud/fcs`
> (`registry.terraform.io/focusnetcloud/fcs`).

Release and Terraform Registry publication are documented in
[`RELEASE.md`](RELEASE.md).

## Build

Requires Go >= 1.26.5.

```sh
make build      # go build ./...
make vet        # go vet ./...
make fmtcheck   # fail if gofmt is needed
```

## Tests (no real API needed)

All tests run against an in-process `httptest` mock that implements the
contract semantics (idempotent POST over name, 404 after delete, 409 on
quota, 401 on bad token):

```sh
make test       # unit tests: client + resource CRUD logic
make testacc    # TF_ACC=1 acceptance tests (needs a terraform CLI in PATH)
```

The repository also carries HCL examples under `examples/`. The
`examples/full-smoke` configuration is the canonical small-sizing smoke setup
for an explicitly approved live run: it provisions one environment, one VM, one
namespace, one flex cluster, one business cluster, one dedicated cluster, plus
ingress and egress on the business cluster.

The `examples/iaas-vdc` configuration covers the standalone IaaS-vDC intent
path: one persistent environment, one service-scoped IaaS-vDC, and one VM
network. VM placement into these networks is intentionally not shown until the
VM broker accepts an explicit IaaS network reference.

The `examples/published-app` configuration covers the customer-facing Free/Flex
app publishing path. Terraform manages the public app intent; Kubernetes
Deployment and Service objects are still managed inside the namespace or
vcluster.

## Provider configuration

| Attribute  | Type   | Notes |
|------------|--------|-------|
| `endpoint` | string | Required. Base URL, e.g. `https://api.focusnet.de`. |
| `token`    | string | Sensitive. Tenant-scoped `fcs_...` token; falls back to the `FCS_TOKEN` environment variable. One of attribute or env var is required. |

## Minimal Environment Example

```hcl
terraform {
  required_providers {
    fcs = { source = "focusnetcloud/fcs", version = "~> 0.12" }
  }
}

provider "fcs" {
  endpoint = "https://api.focusnet.de"
  token    = var.fcs_token # or export FCS_TOKEN
}

# Persistent environment (DEFAULT): omit ttl_seconds -> no expiry, never reaped.
# This is what a normal customer with long-lived resources wants.
resource "fcs_environment" "prod" {
  name = "prod"
  labels = {
    team = "platform"
  }
}

# Ephemeral environment (opt-in): set ttl_seconds -> the server-side reaper tears
# it down after expiry (terraform destroy is advisory).
resource "fcs_environment" "attendee" {
  for_each    = toset(var.attendees)
  name        = "lab-${each.key}" # idempotency key per tenant — re-apply safe
  ttl_seconds = 14400             # 4h (300..604800) — omit for a persistent env

  labels = {
    workshop = "k8s-intro"
  }
}

output "environment_ids" {
  value = { for k, env in fcs_environment.attendee : k => env.id }
}
```

`name`, `ttl_seconds` and `labels` are immutable on the API — every change
forces a replacement. Computed attributes: `id`, `status`
(`provisioning|active|error|offboarding|destroyed`), `ttl_expires_at`
(null for persistent environments), `created_at`.

> **Upgrade note (persistent-by-default):** earlier provider versions defaulted
> `ttl_seconds` to `14400` (4h), so a config that OMITTED `ttl_seconds` produced an
> *ephemeral* 4h environment. With persistent-by-default, omitting `ttl_seconds`
> now means *persistent* — and because `ttl_seconds` is `RequiresReplace`, such a
> config will plan a **replacement** (destroy + recreate) on upgrade. To keep an
> existing environment ephemeral, add `ttl_seconds = 14400` explicitly before
> upgrading. There is no backfill: persistence is opt-in going forward.

## IaaS-vDC Intent Example

```hcl
resource "fcs_environment" "prod" {
  name = "prod"
}

resource "fcs_iaas_vdc" "prod" {
  environment_id = fcs_environment.prod.id
  name           = "prod"
}

resource "fcs_iaas_network" "web" {
  environment_id = fcs_environment.prod.id
  vdc_id         = fcs_iaas_vdc.prod.id
  name           = "web"
  cidr           = "10.80.1.0/24"

  # private: no default east-west allow policy between service networks.
  # allow: default allow inside the vDC. Fine-grained policy is platform-owned.
  policy_default = "private"
}

resource "fcs_vm" "web01" {
  environment_id = fcs_environment.prod.id
  image          = "coriolis-worker-ubuntu2204-qga"
  name           = "web01"
  nic_network    = "iaas"
  vdc_id         = fcs_iaas_vdc.prod.id
  network_id     = fcs_iaas_network.web.id
}

output "iaas_gateway" {
  value = {
    scope_key = fcs_iaas_vdc.prod.scope_key
    vpc       = fcs_iaas_vdc.prod.gateway_vpc_name
    gateway   = fcs_iaas_vdc.prod.gateway_name
  }
}
```

`fcs_iaas_vdc` is the service-scoped NAT gateway container for standalone IaaS
VMs. `fcs_iaas_network` is a network intent inside that vDC, and `fcs_vm` binds
to the active network via `vdc_id` and `network_id`. The backend owns
Harvester/KubeOVN realization; Terraform persists the stable intent, waits for
the network/VM lifecycle, and reads computed fields such as `vlan_id`,
`harvester_nad_name`, `kubeovn_subnet_name`, `default_gateway_ip`, and `vm_ip`.

## Published App Example

```hcl
resource "fcs_environment" "app" {
  name = "app-prod"
}

resource "fcs_namespace" "app" {
  environment_id = fcs_environment.app.id
}

resource "fcs_published_app" "frontend" {
  namespace_id = fcs_namespace.app.id
  app_slug     = "frontend"
  service_name = "frontend"
  service_port = 8080
}

output "frontend_url" {
  value = "https://${fcs_published_app.frontend.hostname}"
}
```

`fcs_published_app` targets `fcs_namespace` and `fcs_flex_cluster` IDs and
returns a gateway hostname such as `frontend-t1234.free.k8s.focusnet.de` or
`api-t1234.flex.k8s.focusnet.de`. All configurable attributes are immutable and
force replacement.

## Cluster timeouts and recovery

Cluster `timeouts` are an object attribute, not a nested Terraform block. Use
an equals sign:

```hcl
resource "fcs_flex_cluster" "app" {
  environment_id = fcs_environment.app.id

  timeouts = {
    create = "30m"
    update = "30m"
    delete = "20m"
  }
}
```

`timeouts { ... }` is invalid for these resources. The create/update/delete
duration is the overall wait deadline. Individual transient polling failures
such as a request timeout, HTTP 429, or HTTP 5xx are retried inside that
deadline; authentication and other permanent HTTP 4xx errors fail immediately.

If a create operation reaches its overall deadline, first inspect the cluster
through the FCS API. Do not apply a tainted resource blindly: Terraform or
OpenTofu will plan a replacement. If the tracked cluster is healthy, use
`terraform untaint <resource-address>` (or `tofu untaint`) before planning. If
the cluster is no longer in state, import it with its environment-scoped ID:

```sh
terraform import fcs_flex_cluster.app \
  '<environment_id>/<cluster_id>'
```

The same import format is supported by `fcs_namespace`,
`fcs_business_cluster`, and `fcs_dedicated_cluster`. The provider reads the
current sizing from the API during import. Business and Flex imports use the
equivalent numeric `vcpu`, `ram_gb`, and `storage_gb` configuration because the
API does not preserve which T-shirt alias originally selected those values.
The API also does not expose the originally selected `k8s_version` or
`rke2_version`. Leave that replacement-only attribute unset while adopting an
existing cluster; adding a pinned version afterwards intentionally plans a
replacement because Terraform cannot prove that it matches the running
cluster.

Sizing changes use `PATCH` and keep the same cluster ID and Kubernetes API.
Increasing or decreasing a Dedicated worker count scales its existing machine
pool. Changes to per-node CPU or RAM roll the affected pool. Storage is
grow-only; shrink requests fail with HTTP 409. Environment and Kubernetes/RKE2
version changes remain replacement-only.
Removing `size` from an existing resource without adding custom sizing keeps
the currently observed sizing; it does not reset the cluster to `S`. Set
`size = "S"` explicitly to request that preset. The request still follows the
grow-only storage rule and can therefore return HTTP 409 after a larger preset.
When `size` is configured, the API does not expose enough information to map
an out-of-band numeric resize back to the original T-shirt alias. The provider
therefore keeps the configured alias in state; use custom numeric sizing when
Terraform must detect and reconcile sizing drift.

## Full Small-Smoke Example

For a complete product-path smoke run. This uses the public FCS API endpoint and
creates real resources; run it only with explicit live-smoke approval and destroy
the resources afterwards:

```sh
cd examples/full-smoke
export FCS_TOKEN=...
terraform init
terraform apply \
  -var 'endpoint=https://api.focusnet.de' \
  -var 'run_id=dev-001'
terraform destroy \
  -var 'endpoint=https://api.focusnet.de' \
  -var 'run_id=dev-001'
```

Use a unique lowercase `run_id` per run. The config intentionally keeps sizing
small:

- VM: 1 vCPU, 2 GiB RAM, 20 GiB disk
- VM image: configurable via `vm_image`; default matches the current catalog
  smoke image `coriolis-worker-ubuntu2204-qga`
- namespace: fixed free-tier sizing
- flex cluster: 1 vCPU, 2 GiB RAM, 20 GiB storage
- business cluster: `size = "S"`
- dedicated cluster: one control-plane node, 4 vCPU, 8 GiB RAM, 50 GiB PVC
- ingress and egress: attached to the business cluster, because flex/namespace
  do not own a dedicated workload CIDR for egress

## Local development override

To test a locally built provider without a registry, add to `~/.terraformrc`:

```hcl
provider_installation {
  dev_overrides {
    "focusnetcloud/fcs" = "/path/to/your/GOBIN"
  }
  direct {}
}
```

Then `go install .` and run `terraform plan` in a config using the provider.
