# Full small-sizing smoke setup for the FCS Terraform provider.
#
# It exercises every FCS resource family once:
# - environment envelope
# - VM
# - namespace
# - flex cluster
# - business cluster
# - dedicated cluster
# - ingress and egress on the business cluster
#
# Use a unique run_id for real DEV smoke runs so resource names remain
# idempotent for retries but do not collide with other operators.

terraform {
  required_version = ">= 1.10"

  required_providers {
    fcs = {
      source  = "focusnetcloud/fcs"
      version = "~> 0.12"
    }
  }
}

variable "fcs_token" {
  type      = string
  sensitive = true
  default   = null # falls back to the FCS_TOKEN environment variable
}

variable "endpoint" {
  type        = string
  description = "FCS API endpoint, for example https://api.focusnet.de. Required for full-smoke runs because this config provisions real resources."

  validation {
    condition     = can(regex("^https://", var.endpoint))
    error_message = "endpoint must be an HTTPS URL, for example https://api.focusnet.de."
  }
}

variable "run_id" {
  type        = string
  description = "Lowercase DNS-safe suffix for idempotent smoke resources."
  default     = "manual"

  validation {
    condition     = can(regex("^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$", var.run_id))
    error_message = "run_id must be lowercase DNS-safe, max 32 chars, and must not start or end with a hyphen."
  }
}

variable "vm_image" {
  type        = string
  description = "Image catalog name for the smoke VM. Check data.fcs_images.catalog for tenant-specific availability."
  default     = "coriolis-worker-ubuntu2204-qga"
}

provider "fcs" {
  endpoint = var.endpoint
  token    = var.fcs_token
}

locals {
  name = "tf-smoke-${var.run_id}"
}

data "fcs_images" "catalog" {}
data "fcs_quota" "tenant" {}

resource "fcs_environment" "smoke" {
  name        = local.name
  ttl_seconds = 14400

  labels = {
    purpose = "tf-smoke"
    run_id  = var.run_id
  }
}

resource "fcs_vm" "small" {
  environment_id = fcs_environment.smoke.id
  name           = "vm-${var.run_id}"
  image          = var.vm_image
  cpu_cores      = 1
  memory_gb      = 2
  disk_gb        = 20
  nic_network    = "tenant"
  running        = true
  depends_on     = [fcs_dedicated_cluster.small]

  timeouts = {
    create = "15m"
    delete = "15m"
  }
}

resource "fcs_namespace" "small" {
  environment_id = fcs_environment.smoke.id

  timeouts = {
    create = "10m"
    delete = "15m"
  }
}

resource "fcs_flex_cluster" "small" {
  environment_id = fcs_environment.smoke.id
  vcpu           = 1
  ram_gb         = 2
  storage_gb     = 20

  timeouts = {
    create = "25m"
    delete = "15m"
  }
}

resource "fcs_business_cluster" "small" {
  environment_id = fcs_environment.smoke.id
  size           = "S"

  timeouts = {
    create = "20m"
    delete = "15m"
  }
}

resource "fcs_dedicated_cluster" "small" {
  environment_id = fcs_environment.smoke.id

  cp_nodes       = 1
  cp_vcpu        = 4
  cp_ram_gb      = 8
  worker_nodes   = 0
  worker_vcpu    = 0
  worker_ram_gb  = 0
  pvc_storage_gb = 50

  timeouts = {
    create = "30m"
    delete = "20m"
  }
}

resource "fcs_environment_ingress" "business_https" {
  environment_id  = fcs_environment.smoke.id
  cluster_id      = fcs_business_cluster.small.id
  service         = "frontend"
  port            = 443
  tls             = "auto"
  hostname_prefix = "tf-${var.run_id}"

  timeouts = {
    create = "5m"
    delete = "15m"
  }
}

resource "fcs_environment_egress" "business_dns" {
  environment_id   = fcs_environment.smoke.id
  cluster_id       = fcs_business_cluster.small.id
  destination_cidr = "1.1.1.1/32"
  protocol         = "udp"
  port_range       = "53"

  timeouts = {
    create = "5m"
    delete = "15m"
  }
}

output "environment_id" {
  value = fcs_environment.smoke.id
}

output "vm_ip" {
  value = fcs_vm.small.vm_ip
}

output "business_cluster_api" {
  value = fcs_business_cluster.small.api_server_url
}

output "dedicated_cluster_api" {
  value = fcs_dedicated_cluster.small.api_server_url
}

output "ingress_url" {
  value = fcs_environment_ingress.business_https.public_url
}

output "quota_summary" {
  value = {
    environments = "${data.fcs_quota.tenant.used_environments}/${data.fcs_quota.tenant.max_concurrent_environments}"
    vms          = "${data.fcs_quota.tenant.used_vms}/${data.fcs_quota.tenant.max_vms}"
    vcpu         = "${data.fcs_quota.tenant.used_vcpu}/${data.fcs_quota.tenant.max_vcpu}"
    ram_gb       = "${data.fcs_quota.tenant.used_ram_gb}/${data.fcs_quota.tenant.max_ram_gb}"
    public_ips   = "${data.fcs_quota.tenant.used_public_ips}/${data.fcs_quota.tenant.max_public_ips}"
  }
}
