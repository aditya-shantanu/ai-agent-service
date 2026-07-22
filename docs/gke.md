# Deploying to GKE

## One-time setup

All GCP infrastructure is Terraform-managed (`terraform/`): the GKE cluster
(Dataplane V2 — REQUIRED, the sandbox NetworkPolicy is the per-user isolation
boundary — plus Workload Identity), node pool, Artifact Registry repo, the
Secret Manager secret *container*, and the IAM binding that lets External
Secrets Operator read it. The secret *value* is deliberately NOT in Terraform
(API keys must never enter TF state) — it is pushed from your local `.env`.

```sh
export GCP_PROJECT=<your-gcp-project>   # required by all GKE-touching targets
gcloud auth application-default login   # credentials for Terraform
gcloud auth configure-docker us-central1-docker.pkg.dev

make infra-apply   # or: cd terraform && terraform apply -var project_id=$GCP_PROJECT
```

## Deploy

One command does everything (idempotent — safe to re-run):

```sh
cp .env.example .env    # fill in GEMINI_API_KEY etc. (gitignored)
make deploy-gke GCP_PROJECT=<your-gcp-project>
```

(The chart's `values-gke.yaml` carries `PROJECT_ID` placeholders; the
Makefile injects your real image paths and project via `--set-string`.)

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
NS=hermes-users hack/e2e.sh    # same 11-check suite as kind
```

Note the e2e uses a port-forward, so it works before the LB is provisioned.

## Production notes

- **TLS / domain**: put GKE Ingress or a Gateway API `Gateway` with a managed
  certificate in front of the `hermes-gateway` Service; the plain LoadBalancer
  is HTTP-only. The dashboard's cookie login should not run over plain HTTP
  outside dev.
- Idle suspension is adaptive: 15s base tail, 10m active window on GKE
  (`idle.timeout` / `idle.activeTimeout`) — most same-day returns wake
  sub-second from swap residency; cold resumes measure ~20–24s under
  gVisor (~11–14s on the runc rollback pool).
- **gVisor hardening is enabled by default** (2026-07-17): sandboxes run
  under GKE Sandbox on `hermes-gvisor-pool` (`hack/gke-gvisor-pool.sh` —
  same Spot n2d + LSSD-swap shape as the runc pool, plus
  `--sandbox type=gvisor`), selected via `sandbox.runtimeClassName: gvisor`
  in `values-gke.yaml`. GKE's `gvisor` RuntimeClass auto-adds the
  `sandbox.gke.io/runtime` nodeSelector + toleration. Existing user
  Sandboxes keep their stamped runc podTemplate until recreated; warm
  spares recycle automatically. Rollback: comment out
  `runtimeClassName` and cycle warm spares (pods fall back to
  `hermes-swap-pool` via the shared `hermes-swap` label). Caveats:
  `kubectl port-forward` to sandboxed pods is not supported (the gateway
  data path is unaffected), and container-level memory metrics are
  pod-level only. Cost/perf analysis: `costcalc/COST-REDUCTION.md`.
- Do NOT enable autoscaling of the gateway (1-replica constraint, README
  decision 11).
