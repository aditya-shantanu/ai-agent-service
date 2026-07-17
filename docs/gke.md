# Deploying to GKE (`gke-ai-eco-dev`)

## One-time setup

All GCP infrastructure is Terraform-managed (`terraform/`): the GKE cluster
(Dataplane V2 — REQUIRED, the sandbox NetworkPolicy is the per-user isolation
boundary — plus Workload Identity), node pool, Artifact Registry repo, the
Secret Manager secret *container*, and the IAM binding that lets External
Secrets Operator read it. The secret *value* is deliberately NOT in Terraform
(API keys must never enter TF state) — it is pushed from your local `.env`.

```sh
gcloud auth application-default login   # credentials for Terraform
gcloud auth configure-docker us-central1-docker.pkg.dev

cd terraform && terraform init && terraform apply   # or: make infra-apply
```

## Deploy

One command does everything (idempotent — safe to re-run):

```sh
cp .env.example .env    # fill in GEMINI_API_KEY etc. (gitignored)
make deploy-gke
```

That target sets up, in order:

1. **Images** → Artifact Registry (amd64 gateway build + mirrored Hermes image).
2. **agent-sandbox** CRDs + controller (pinned release manifest).
3. **Workload Identity** on the cluster + node pool (first run only; the node
   pool update is a rolling node recreation — expect a few minutes of churn).
4. **External Secrets Operator** (helm, its own namespace).
5. **Provider keys**: your local `.env` is converted to JSON and pushed to the
   Google Secret Manager secret `hermes-provider-keys`; ESO's pod identity is
   granted `secretAccessor` on exactly that secret (keyless — Workload
   Identity principal, no service-account files).
6. **The chart** with `values-gke.yaml`: an `ExternalSecret` keeps the
   in-cluster `hermes-provider-keys` Secret in sync with Secret Manager
   (refresh: 1h); the deploy waits for the first sync and cycles warm-pool
   spares so new users chat immediately.

**Key rotation**: edit `.env` → `make gsm-push-key` (or add a version in the
Secret Manager console) → ESO syncs within the refresh interval → sandboxes
pick it up on their next suspend/resume cycle. Nothing else to do.

`values-gke.yaml` exposes the gateway as a `LoadBalancer`. Wait for the IP:

```sh
kubectl -n hermes-users get svc hermes-gateway -w
```

## Verify

```sh
NS=hermes-users hack/e2e.sh    # same 10-check suite as kind
```

Note the e2e uses a port-forward, so it works before the LB is provisioned.

## Production notes

- **TLS / domain**: put GKE Ingress or a Gateway API `Gateway` with a managed
  certificate in front of the `hermes-gateway` Service; the plain LoadBalancer
  is HTTP-only. The dashboard's cookie login should not run over plain HTTP
  outside dev.
- The default idle timeout is 1m (`idle.timeout`) — chosen for fast test iteration; raise it for real users if wake-on-connect churn becomes annoying.
- gVisor hardening: schedule sandboxes onto a `--sandbox type=gvisor` node
  pool by adding `runtimeClassName: gvisor` to the SandboxTemplate podTemplate
  — documented follow-up, not enabled by default.
- Do NOT enable autoscaling of the gateway (1-replica constraint, README
  decision 11).
