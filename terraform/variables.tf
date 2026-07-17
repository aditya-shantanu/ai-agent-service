variable "project_id" {
  description = "GCP project to deploy into"
  type        = string
  default     = "gke-ai-eco-dev"
}

variable "zone" {
  description = "Zone for the (zonal) GKE cluster"
  type        = string
  default     = "us-central1-a"
}

variable "region" {
  description = "Region for Artifact Registry"
  type        = string
  default     = "us-central1"
}

variable "cluster_name" {
  description = "GKE cluster name"
  type        = string
  default     = "hermes-svc"
}

variable "node_machine_type" {
  description = "Node machine type"
  type        = string
  default     = "e2-standard-4"
}

variable "node_count" {
  description = "Number of nodes"
  type        = number
  default     = 2
}

variable "gsm_secret_name" {
  description = "Secret Manager secret holding provider keys (JSON payload; value pushed out-of-band via `make gsm-push-key` — never via Terraform, so keys stay out of TF state)"
  type        = string
  default     = "hermes-provider-keys"
}

variable "eso_namespace" {
  description = "Namespace External Secrets Operator runs in (its Workload Identity principal is granted access to the secret)"
  type        = string
  default     = "external-secrets"
}

variable "eso_service_account" {
  description = "ESO controller's Kubernetes ServiceAccount name"
  type        = string
  default     = "external-secrets"
}
