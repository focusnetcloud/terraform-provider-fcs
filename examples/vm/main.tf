# Example: a standalone VM per attendee in the persistent tenant network.

terraform {
  required_providers {
    fcs = {
      source  = "focusnetcloud/fcs"
      version = "~> 0.10"
    }
  }
}

variable "fcs_token" {
  type      = string
  sensitive = true
  default   = null # falls back to the FCS_TOKEN environment variable
}

variable "attendees" {
  type    = list(string)
  default = ["alice", "bob"]
}

variable "userdata" {
  type      = map(string)
  sensitive = true
  default   = {}
}

provider "fcs" {
  endpoint = "https://api.focusnet.de"
  token    = var.fcs_token
}

# The image catalog approved for environments — every listed name is a
# valid fcs_vm image.
data "fcs_images" "catalog" {}

# Usage vs. limits (GCP quota-page parity); handy for preflight checks.
data "fcs_quota" "tenant" {}

# TTL envelope: one environment per attendee (server-side reaper is
# authoritative; terraform destroy is advisory).
resource "fcs_environment" "attendee" {
  for_each    = toset(var.attendees)
  name        = "lab-${each.key}" # idempotency key per tenant — re-apply safe
  ttl_seconds = 14400             # 4h — ephemeral opt-in; omit for a persistent env
}

# Standalone VM in the persistent tenant network.
resource "fcs_vm" "target" {
  for_each            = toset(var.attendees)
  environment_id      = fcs_environment.attendee[each.key].id
  image               = "coriolis-worker-ubuntu2204-qga" # fcs_images data source / tenant catalog
  cpu_cores           = 2
  memory_gb           = 4
  nic_network         = "tenant" # persistent tenant VLAN NAD; server picks NAD + IP
  cloud_init_userdata = lookup(var.userdata, each.key, null)

  # running = true is the default; set false to park the VM (in-place
  # power stop/start — the only mutable attribute).

  timeouts = {
    create = "15m"
  }
}

output "vm_ips" {
  value = { for k, vm in fcs_vm.target : k => vm.vm_ip }
}

output "available_images" {
  value = [for img in data.fcs_images.catalog.images : img.name]
}

output "quota_vms" {
  value = "${data.fcs_quota.tenant.used_vms}/${data.fcs_quota.tenant.max_vms} VMs"
}
