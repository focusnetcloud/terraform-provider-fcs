terraform {
  required_version = ">= 1.10"

  required_providers {
    fcs = {
      source = "focusnetcloud/fcs"
    }
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = "~> 2.38"
    }
  }
}

provider "fcs" {
  endpoint = "https://api.focusnet.de"
  # Export FCS_TOKEN. Do not write the tenant token into configuration.
}

resource "fcs_environment" "application" {
  name = "harbor-application"
}

resource "fcs_business_cluster" "application" {
  environment_id = fcs_environment.application.id
  size           = "S"
  k8s_version    = "v1.35.3-k3s1"
}

ephemeral "fcs_cluster_kubeconfig" "application" {
  environment_id = fcs_environment.application.id
  cluster_id     = fcs_business_cluster.application.id
}

provider "kubernetes" {
  alias = "application"
  host  = ephemeral.fcs_cluster_kubeconfig.application.api_server_url
  token = ephemeral.fcs_cluster_kubeconfig.application.sa_token
}

resource "kubernetes_namespace_v1" "application" {
  provider = kubernetes.application

  metadata {
    name = "application"
  }
}

# Push harbor.cloud.focusnet.de/t-<tenant>/apps/example:2026-07 with a standard
# OCI tool before apply. Terraform registers metadata; it does not upload layers.
resource "fcs_harbor_artifact" "application" {
  repository = "apps/example"
  reference  = "2026-07"
  kind       = "container"
}

resource "fcs_harbor_registry_binding" "application" {
  environment_id   = fcs_environment.application.id
  cluster_id       = fcs_business_cluster.application.id
  namespace        = kubernetes_namespace_v1.application.metadata[0].name
  secret_name      = "fcs-harbor-pull"
  service_accounts = ["default"]

  # Change this provider-local marker to rotate entirely server-side.
  rotation_trigger = "2026-q3"
}

resource "kubernetes_deployment_v1" "application" {
  provider   = kubernetes.application
  depends_on = [fcs_harbor_registry_binding.application]

  metadata {
    name      = "example"
    namespace = kubernetes_namespace_v1.application.metadata[0].name
  }

  spec {
    replicas = 1

    selector {
      match_labels = {
        app = "example"
      }
    }

    template {
      metadata {
        labels = {
          app = "example"
        }
      }

      spec {
        service_account_name = "default"

        container {
          name  = "example"
          image = fcs_harbor_artifact.application.canonical_reference
        }
      }
    }
  }
}
