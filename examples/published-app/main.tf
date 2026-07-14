# Publish a Service from a Free namespace through the FCS public app gateway.
# The Kubernetes Deployment and Service are managed separately inside the
# namespace, for example via the Kubernetes provider.

terraform {
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

provider "fcs" {
  endpoint = "https://api.focusnet.de"
  token    = var.fcs_token
}

resource "fcs_environment" "app" {
  name = "published-app-demo"
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
