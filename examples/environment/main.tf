# Minimal environment example.

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

provider "fcs" {
  endpoint = "https://api.focusnet.de"
  token    = var.fcs_token
}

# Environments are PERSISTENT by default (no ttl_seconds -> no expiry, never reaped)
# — that is what a normal customer with long-lived resources wants.
resource "fcs_environment" "prod" {
  name = "prod"
  labels = {
    team = "platform"
  }
}

# Ephemeral env (opt-in): set ttl_seconds and the server-side TTL reaper tears it
# down after expiry (terraform destroy is advisory).
resource "fcs_environment" "attendee" {
  for_each    = toset(var.attendees)
  name        = "lab-${each.key}" # idempotency key per tenant — re-apply safe
  ttl_seconds = 14400             # 4h — omit this line for a persistent environment

  labels = {
    workshop = "k8s-intro"
  }
}

output "environment_ids" {
  value = { for k, env in fcs_environment.attendee : k => env.id }
}

output "environment_expiry" {
  value = { for k, env in fcs_environment.attendee : k => env.ttl_expires_at }
}
