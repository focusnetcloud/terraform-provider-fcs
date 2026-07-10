# Standalone IaaS-vDC intent example.
#
# This creates one service-scoped IaaS-vDC, one VM network, and one VM bound to
# that network. The backend owns the KubeOVN/Harvester realization; Terraform
# waits for the network and VM to become active.

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

variable "endpoint" {
  type        = string
  description = "FCS API endpoint, for example https://api.focusnet.de."
  default     = "https://api.focusnet.de"

  validation {
    condition     = can(regex("^https://", var.endpoint))
    error_message = "endpoint must be an HTTPS URL, for example https://api.focusnet.de."
  }
}

variable "name" {
  type        = string
  description = "DNS-safe name used for the environment and IaaS-vDC."
  default     = "prod"

  validation {
    condition     = can(regex("^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$", var.name))
    error_message = "name must be lowercase DNS-safe, max 63 chars, and must not start or end with a hyphen."
  }
}

variable "network_cidr" {
  type        = string
  description = "CIDR for the first IaaS-vDC VM network."
  default     = "10.80.1.0/24"
}

variable "vm_image" {
  type        = string
  description = "Image catalog name for the VM. Check data.fcs_images.catalog for tenant-specific availability."
  default     = "coriolis-worker-ubuntu2204-qga"
}

data "fcs_images" "catalog" {}

provider "fcs" {
  endpoint = var.endpoint
  token    = var.fcs_token
}

resource "fcs_environment" "iaas" {
  name = var.name

  labels = {
    product = "iaas-vdc"
  }
}

resource "fcs_iaas_vdc" "iaas" {
  environment_id = fcs_environment.iaas.id
  name           = var.name

  labels = {
    product = "iaas-vdc"
  }
}

resource "fcs_iaas_network" "web" {
  environment_id = fcs_environment.iaas.id
  vdc_id         = fcs_iaas_vdc.iaas.id
  name           = "web"
  cidr           = var.network_cidr
  policy_default = "private"
}

resource "fcs_vm" "web01" {
  environment_id = fcs_environment.iaas.id
  image          = var.vm_image
  name           = "web01"
  nic_network    = "iaas"
  vdc_id         = fcs_iaas_vdc.iaas.id
  network_id     = fcs_iaas_network.web.id
}

output "environment_id" {
  value = fcs_environment.iaas.id
}

output "iaas_vdc" {
  value = {
    id            = fcs_iaas_vdc.iaas.id
    status        = fcs_iaas_vdc.iaas.status
    scope_key     = fcs_iaas_vdc.iaas.scope_key
    gateway_scope = fcs_iaas_vdc.iaas.gateway_scope
    gateway_vpc   = fcs_iaas_vdc.iaas.gateway_vpc_name
    gateway_name  = fcs_iaas_vdc.iaas.gateway_name
  }
}

output "iaas_network" {
  value = {
    id                  = fcs_iaas_network.web.id
    status              = fcs_iaas_network.web.status
    cidr                = fcs_iaas_network.web.cidr
    vlan_id             = fcs_iaas_network.web.vlan_id
    harvester_namespace = fcs_iaas_network.web.harvester_namespace
    harvester_nad_name  = fcs_iaas_network.web.harvester_nad_name
    kubeovn_subnet_name = fcs_iaas_network.web.kubeovn_subnet_name
    default_gateway_ip  = fcs_iaas_network.web.default_gateway_ip
  }
}

output "vm" {
  value = {
    id     = fcs_vm.web01.id
    name   = fcs_vm.web01.name
    status = fcs_vm.web01.status
    ip     = fcs_vm.web01.vm_ip
  }
}

output "available_images" {
  value = [for img in data.fcs_images.catalog.images : img.name]
}
