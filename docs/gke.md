# Deploying to GKE (`gke-ai-eco-dev`)

## One-time setup

```sh
gcloud config set project gke-ai-eco-dev
REGION=us-central1

# Artifact Registry repo for our images
gcloud artifacts repositories create hermes-service \
  --repository-format=docker --location=$REGION
gcloud auth configure-docker $REGION-docker.pkg.dev

# A cluster with NetworkPolicy enforcement (Dataplane V2) — REQUIRED:
# the sandbox NetworkPolicy is the per-user isolation boundary.
gcloud container clusters create hermes-svc \
  --region=$REGION --enable-dataplane-v2 --num-nodes=1 \
  --machine-type=e2-standard-4
gcloud container clusters get-credentials hermes-svc --region=$REGION
```

## Deploy

```sh
make images-push        # builds amd64 gateway + mirrors pinned hermes image to AR
make sandbox-install    # agent-sandbox v0.5.2 CRDs + controller

helm upgrade --install hermes-service charts/hermes-service \
  -n hermes-users --create-namespace \
  -f charts/hermes-service/values-gke.yaml

# real LLM key (chat won't work without one)
kubectl -n hermes-users create secret generic hermes-provider-keys \
  --from-literal=GEMINI_API_KEY=YOUR_KEY --dry-run=client -o yaml | kubectl apply -f -
```

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
- The default idle timeout is 15m (`idle.timeout`).
- gVisor hardening: schedule sandboxes onto a `--sandbox type=gvisor` node
  pool by adding `runtimeClassName: gvisor` to the SandboxTemplate podTemplate
  — documented follow-up, not enabled by default.
- Do NOT enable autoscaling of the gateway (1-replica constraint, README
  decision 11).
