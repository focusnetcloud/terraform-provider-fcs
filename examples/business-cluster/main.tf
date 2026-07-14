# Example: one environment, one business cluster, and the standard Kubernetes
# provider configured from short-lived cluster credentials.

terraform {
  # Ephemeral resources (ephemeral "fcs_cluster_kubeconfig") need TF >= 1.10.
  required_version = ">= 1.10"

  required_providers {
    fcs = {
      source  = "focusnetcloud/fcs"
      version = "~> 0.12"
    }
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = "~> 2.38"
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

# TTL envelope; the server-side reaper is authoritative for teardown.
resource "fcs_environment" "lab" {
  name        = "lab-alice"
  ttl_seconds = 14400 # 4h — ephemeral opt-in; omit for a persistent env

  labels = {
    workshop = "k8s-intro"
  }
}

# Business vCluster in the environment. CIDRs/IPs/VLAN are allocated
# SERVER-side; create polls until status=active (timeouts configurable).
resource "fcs_business_cluster" "lab" {
  environment_id = fcs_environment.lab.id
  size           = "S" # or custom: vcpu/ram_gb/storage_gb (mutually exclusive)
  k8s_version    = "v1.35.3-k3s1"

  timeouts = {
    create = "20m"
    delete = "15m"
  }
}

# Recovery for an existing cluster that is no longer in state:
# terraform import fcs_business_cluster.lab '<environment_id>/<cluster_id>'
# Verify the declared sizing matches the existing cluster before applying.

# Cluster-scoped credentials as an EPHEMERAL resource (Terraform >= 1.10):
# minted fresh on every run and held in memory only — never written to the
# Terraform plan or state.
ephemeral "fcs_cluster_kubeconfig" "sbx" {
  environment_id = fcs_environment.lab.id
  cluster_id     = fcs_business_cluster.lab.id
}

# Workloads are deployed with the standard Kubernetes provider against the
# returned cluster API. Provider configurations accept ephemeral values
# (TF >= 1.10), so dependent provider blocks like this one never pull the
# credentials into the state; they only exist for the run.
provider "kubernetes" {
  alias = "sbx"
  host  = ephemeral.fcs_cluster_kubeconfig.sbx.api_server_url
  token = ephemeral.fcs_cluster_kubeconfig.sbx.sa_token # ephemeral, sensitive
}

resource "kubernetes_namespace" "lab_app" {
  provider = kubernetes.sbx
  metadata {
    name = "lab-app"
  }
}

output "api_server_url" {
  value = fcs_business_cluster.lab.api_server_url
}

output "cluster_cidr" {
  value = fcs_business_cluster.lab.cluster_cidr
}

# NOTE: ephemeral values cannot be referenced from (non-ephemeral) root
# outputs because that would persist them.
