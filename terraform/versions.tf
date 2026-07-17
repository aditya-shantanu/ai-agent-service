terraform {
  required_version = ">= 1.5"
  required_providers {
    google = {
      source  = "hashicorp/google"
      version = ">= 6.0"
    }
  }
  # State: local by default (fine for a single-operator dev project).
  # For teams, switch to a GCS backend:
  #   backend "gcs" { bucket = "<your-tf-state-bucket>", prefix = "hermes-service" }
}

provider "google" {
  project = var.project_id
}
