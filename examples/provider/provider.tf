terraform {
  required_providers {
    fcs = {
      source  = "focusnetcloud/fcs"
      version = "~> 0.10"
    }
  }
}

provider "fcs" {
  endpoint = "https://api.focusnet.de"
  token    = var.fcs_token # or export FCS_TOKEN
}
