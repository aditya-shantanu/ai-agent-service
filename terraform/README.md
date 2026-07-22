# Terraform

Provisions everything the platform needs in GCP: the GKE cluster (Dataplane
V2, Workload Identity), the system and Spot sandbox node pools, an Artifact
Registry repo, the Secret Manager container for LLM provider keys, and the
IAM binding that lets External Secrets Operator sync them.

## Prerequisites

- An existing GCP project with these APIs enabled: `container`, `compute`,
  `artifactregistry`, `secretmanager`, `iam`.
- `project_id` has no default — pass `-var project_id=...` or a tfvars file.
- State is local by default; `versions.tf` documents switching to a GCS
  backend for anything beyond a single operator.

## Node pools: why some are managed outside Terraform

The swap-enabled sandbox pools (`hermes-swap-pool`, `hermes-gvisor-pool`) are
created by `hack/gke-swap-pool.sh` / `hack/gke-gvisor-pool.sh` via gcloud,
because the Terraform google provider does not yet expose kubelet
`swapConfig`. Terraform additionally keeps a plain (non-swap) Spot
`sandbox-pool` as a rollback target. Once the provider supports swap
configuration, fold the gcloud pools into `main.tf` and drop the rollback
pool.
