terraform {
  required_version = ">= 1.10"

  required_providers {
    fcs = {
      source = "focusnetcloud/fcs"
    }
  }
}

variable "fcs_endpoint" {
  type    = string
  default = "https://api.focusnet.de"
}

provider "fcs" {
  endpoint = var.fcs_endpoint
  # Export FCS_TOKEN instead of writing the tenant token into configuration.
}

resource "fcs_harbor_robot_account" "ci" {
  name               = "ci"
  description        = "CI image publishing"
  permission_profile = "push_pull"
  expires_in_days    = 90

  # Increment or otherwise change this marker to rotate the credential in place.
  rotation_trigger = "2026-q3"
}

# Push the image with Docker, Podman, Skopeo, ORAS or Crane before registering
# it. The FCS API resolves this tag once and stores the immutable digest.
resource "fcs_harbor_artifact" "application" {
  repository = "apps/example"
  reference  = "2026-07"
  kind       = "container"
}

output "harbor_robot_username" {
  value = fcs_harbor_robot_account.ci.username
}

# This output is intentionally sensitive. Terraform state still contains the
# secret, so use an encrypted, access-controlled remote state backend.
output "harbor_robot_secret" {
  value     = fcs_harbor_robot_account.ci.secret
  sensitive = true
}

output "application_image" {
  value = fcs_harbor_artifact.application.canonical_reference
}
