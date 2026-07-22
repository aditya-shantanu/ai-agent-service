variable "project_id" {
  description = "GCP project to deploy into (no default — pass -var or set TF_VAR_project_id)"
  type        = string
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

variable "system_machine_type" {
  description = "Machine type for the system pool (gateway, controllers, ESO)"
  type        = string
  default     = "e2-standard-2"
}

variable "sandbox_machine_type" {
  description = "Machine type for the Spot sandbox pool. e2-custom with ~1.25GB/vCPU covers GKE node reservations without idle RAM (cost model: costcalc/)"
  type        = string
  default     = "e2-custom-16-20480"
}

variable "sandbox_node_count" {
  description = "Number of Spot sandbox nodes"
  type        = number
  default     = 1
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
