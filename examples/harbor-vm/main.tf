terraform {
  required_version = ">= 1.10"

  required_providers {
    fcs = {
      source = "focusnetcloud/fcs"
    }
  }
}

provider "fcs" {
  endpoint = "https://api.focusnet.de"
  # Export FCS_TOKEN. Do not write the tenant token into configuration.
}

resource "fcs_environment" "vm" {
  name = "harbor-vm"
}

# Upload a CDI-compatible KubeVirt disk to
# harbor.cloud.focusnet.de/t-<tenant>/vm-images/windows-2025:<tag> with an OCI
# tool before apply. Terraform registers metadata; it does not upload layers.
resource "fcs_harbor_artifact" "windows_2025" {
  repository = "vm-images/windows-2025"
  reference  = "2026-07"
  kind       = "vm_disk"
}

resource "fcs_vm" "windows_2025" {
  environment_id     = fcs_environment.vm.id
  name               = "windows-2025"
  harbor_artifact_id = fcs_harbor_artifact.windows_2025.id
  cpu_cores          = 2
  memory_gb          = 8
  disk_gb            = 80

  timeouts = {
    create = "30m"
    delete = "30m"
  }
}

output "vm" {
  value = {
    id     = fcs_vm.windows_2025.id
    status = fcs_vm.windows_2025.status
    ip     = fcs_vm.windows_2025.vm_ip
  }
}
