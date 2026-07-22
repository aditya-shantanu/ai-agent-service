data "google_project" "this" {
  project_id = var.project_id
}

# ---------------------------------------------------------------------------
# GKE: zonal Standard cluster, Dataplane V2 (NetworkPolicy enforcement is the
# platform's per-user isolation boundary — non-negotiable), Workload Identity
# (keyless Secret Manager access for External Secrets Operator).
# ---------------------------------------------------------------------------
resource "google_container_cluster" "hermes" {
  name     = var.cluster_name
  location = var.zone

  # Dataplane V2 (Cilium/eBPF): NetworkPolicy always enforced.
  datapath_provider = "ADVANCED_DATAPATH"

  workload_identity_config {
    workload_pool = "${var.project_id}.svc.id.goog"
  }

  # We manage the node pool as its own resource.
  remove_default_node_pool = true
  initial_node_count       = 1

  # Test/dev posture: allow `terraform destroy` to remove the cluster.
  deletion_protection = false
}

# System pool: gateway, agent-sandbox controller, ESO, kube-system. Small,
# on-demand (the gateway's in-memory idle state shouldn't ride Spot).
resource "google_container_node_pool" "system" {
  name     = "system-pool"
  cluster  = google_container_cluster.hermes.name
  location = var.zone

  node_count = 1

  node_config {
    machine_type = var.system_machine_type
    image_type   = "COS_CONTAINERD"
    workload_metadata_config {
      mode = "GKE_METADATA"
    }
    oauth_scopes = ["https://www.googleapis.com/auth/cloud-platform"]
  }
}

# Rollback sandbox pool (idle): sandboxes normally run on the gcloud-managed
# LSSD-swap pool (hack/gke-swap-pool.sh; see terraform/README.md for why it
# is not Terraform-managed) — this plain pool is kept as an instant rollback
# target (flip values-gke selectors back). Spot VMs — the platform is
# restart-tolerant by design
# (suspend/resume IS a kill; PVCs survive; Hermes resumes sessions), so a
# Spot preemption is just an unscheduled suspend. Tainted so only sandbox
# pods (which carry the toleration via the SandboxTemplate) land here.
# Shape: e2-custom with ~1.25GB RAM per vCPU — enough to cover GKE node
# reservations without paying for idle RAM (nodes bin-pack CPU-and-RAM
# balanced for ~1vCPU/1GB agent requests).
resource "google_container_node_pool" "sandbox" {
  name     = "sandbox-pool"
  cluster  = google_container_cluster.hermes.name
  location = var.zone

  node_count = var.sandbox_node_count

  node_config {
    machine_type = var.sandbox_machine_type
    image_type   = "COS_CONTAINERD"
    spot         = true

    labels = { "hermes-sandbox" = "true" }
    taint {
      key    = "hermes-sandbox"
      value  = "true"
      effect = "NO_SCHEDULE"
    }

    workload_metadata_config {
      mode = "GKE_METADATA"
    }
    oauth_scopes = ["https://www.googleapis.com/auth/cloud-platform"]
  }
}

# ---------------------------------------------------------------------------
# Artifact Registry: gateway image + mirrored Hermes image
# (pushed by `make images-push`).
# ---------------------------------------------------------------------------
resource "google_artifact_registry_repository" "hermes" {
  repository_id = "hermes-service"
  location      = var.region
  format        = "DOCKER"
  description   = "hermes-gateway + mirrored hermes-agent images"
}

# ---------------------------------------------------------------------------
# Secret Manager: the provider-keys secret CONTAINER only. The value is
# pushed out-of-band (`make gsm-push-key` from a local .env) so API keys
# never enter Terraform state.
# ---------------------------------------------------------------------------
resource "google_secret_manager_secret" "provider_keys" {
  secret_id = var.gsm_secret_name

  replication {
    auto {}
  }
}

# External Secrets Operator reads the secret via its Workload Identity
# principal — no GCP service account, no key files.
resource "google_secret_manager_secret_iam_member" "eso_accessor" {
  secret_id = google_secret_manager_secret.provider_keys.secret_id
  role      = "roles/secretmanager.secretAccessor"
  member    = "principal://iam.googleapis.com/projects/${data.google_project.this.number}/locations/global/workloadIdentityPools/${var.project_id}.svc.id.goog/subject/ns/${var.eso_namespace}/sa/${var.eso_service_account}"

  depends_on = [google_container_cluster.hermes] # WI pool must exist
}
