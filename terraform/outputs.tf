output "cluster_name" {
  value = google_container_cluster.hermes.name
}

output "get_credentials" {
  value = "gcloud container clusters get-credentials ${google_container_cluster.hermes.name} --zone=${var.zone} --project=${var.project_id}"
}

output "artifact_registry" {
  value = "${var.region}-docker.pkg.dev/${var.project_id}/${google_artifact_registry_repository.hermes.repository_id}"
}

output "gsm_secret" {
  value = google_secret_manager_secret.provider_keys.secret_id
}
