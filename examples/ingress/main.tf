# Example: a business cluster inside an environment, exposed externally through
# the FCS NAT gateway, plus an optional egress allow rule.

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

provider "fcs" {
  endpoint = "https://api.focusnet.de"
  token    = var.fcs_token
}

# TTL envelope.
resource "fcs_environment" "lab" {
  name        = "lab-ingress-demo"
  ttl_seconds = 14400 # 4h — ephemeral opt-in; omit for a persistent env
}

# Business cluster: allocates its own public IP (EIP). Create is asynchronous;
# the provider polls until status=active before the ingress is created.
resource "fcs_business_cluster" "app" {
  environment_id = fcs_environment.lab.id
  size           = "S"
}

# Ingress: DNAT public_ip:port -> the cluster ingress. Binds NO extra public IP
# (reuses the cluster EIP). Idempotent over (environment, cluster, prefix).
resource "fcs_environment_ingress" "app" {
  environment_id  = fcs_environment.lab.id
  cluster_id      = fcs_business_cluster.app.id
  service         = "frontend"
  port            = 443
  tls             = "auto" # the cluster ingress terminates TLS
  hostname_prefix = "lab-alice"
}

# Optional egress-allow rule: let the cluster reach an external CIDR/host.
resource "fcs_environment_egress" "dns" {
  environment_id   = fcs_environment.lab.id
  cluster_id       = fcs_business_cluster.app.id
  destination_cidr = "1.1.1.1/32"
  protocol         = "udp"
  port_range       = "53"
}

output "public_url" {
  description = "The externally reachable URL of the exposed service."
  value       = fcs_environment_ingress.app.public_url
}
