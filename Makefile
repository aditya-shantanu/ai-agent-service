# --- Pinned versions (bump deliberately; keep in sync with README/docs) ---
HERMES_IMAGE          ?= nousresearch/hermes-agent:v2026.7.7.2
AGENT_SANDBOX_VERSION ?= v0.5.2
GATEWAY_IMAGE         ?= hermes-gateway:dev
KIND_CLUSTER          ?= hermes-svc
NAMESPACE             ?= hermes-users

# GKE / Artifact Registry (M7)
GCP_PROJECT ?= gke-ai-eco-dev
AR_REGION   ?= us-central1
AR_REPO     ?= $(AR_REGION)-docker.pkg.dev/$(GCP_PROJECT)/hermes-service

.PHONY: build test lint validate-hermes-image image kind-up kind-load deploy-kind dev e2e simulate-users set-provider-key undeploy sandbox-install images-push deploy-gke gke-credentials infra-apply infra-destroy eso-install gsm-push-key gke-swap-pool help

build: ## Build the gateway binary
	go build -o bin/gateway ./cmd/gateway

test: ## Run unit tests
	go test ./...

lint: ## gofmt + go vet
	@test -z "$$(gofmt -l . | tee /dev/stderr)" || (echo 'gofmt: files need formatting' && exit 1)
	go vet ./...

validate-hermes-image: ## M1: validate the pinned Hermes image contract (needs Docker)
	HERMES_IMAGE=$(HERMES_IMAGE) hack/validate-hermes-image.sh

image: ## Build the gateway container image
	docker build -t $(GATEWAY_IMAGE) .

kind-up: ## Create kind cluster + install agent-sandbox (pinned)
	AGENT_SANDBOX_VERSION=$(AGENT_SANDBOX_VERSION) CLUSTER_NAME=$(KIND_CLUSTER) hack/kind-up.sh

kind-load: image ## Load the gateway image into the kind cluster
	docker save $(GATEWAY_IMAGE) -o /tmp/hermes-gateway-img.tar
	kind load image-archive /tmp/hermes-gateway-img.tar --name $(KIND_CLUSTER)
	rm /tmp/hermes-gateway-img.tar

deploy-kind: kind-load ## Helm-install/upgrade onto kind with dev values
	helm upgrade --install hermes-service charts/hermes-service \
	  -n $(NAMESPACE) --create-namespace -f charts/hermes-service/values-kind.yaml
	kubectl -n $(NAMESPACE) rollout status deploy/hermes-gateway --timeout=180s

dev: kind-up deploy-kind ## LOCAL MODE: kind cluster + deploy (fast 60s idle-suspend)
	@echo "Local dev deployment ready. Next: make e2e  or  make simulate-users"

e2e: ## Run the full-loop e2e test (expects deploy-kind done)
	NS=$(NAMESPACE) hack/e2e.sh

simulate-users: ## Emulate N concurrent users (USERS=3): provision, traffic, idle, wake
	NS=$(NAMESPACE) USERS=$(or $(USERS),3) hack/simulate-users.sh

set-provider-key: ## Load LLM provider keys from .env into the cluster (kind or GKE)
	@test -f .env || (echo "No .env file — copy .env.example to .env and fill in your key" && exit 1)
	kubectl -n $(NAMESPACE) create secret generic hermes-provider-keys \
	  --from-env-file=.env --dry-run=client -o yaml | kubectl apply -f -
	@echo "Cycling warm-pool spares so new sandboxes get the key..."
	kubectl -n $(NAMESPACE) delete sandboxes -l agents.x-k8s.io/warm-pool-sandbox --ignore-not-found
	@echo "Done. Existing users pick the key up on their next suspend/resume cycle"
	@echo "(or immediately: POST /api/v1/users/{id}/suspend then /resume)."

undeploy: ## Remove the helm release (users/claims are NOT deleted)
	helm uninstall hermes-service -n $(NAMESPACE)

sandbox-install: ## Install agent-sandbox release manifest into current cluster
	kubectl apply -f https://github.com/kubernetes-sigs/agent-sandbox/releases/download/$(AGENT_SANDBOX_VERSION)/sandbox-with-extensions.yaml

gke-credentials: ## Point kubectl at the GKE cluster
	gcloud container clusters get-credentials hermes-svc --zone=us-central1-a --project=$(GCP_PROJECT)

GKE_CLUSTER ?= hermes-svc
GKE_ZONE    ?= us-central1-a
GSM_SECRET  ?= hermes-provider-keys
ESO_NS      ?= external-secrets

infra-apply: ## Provision/update ALL GCP infra via Terraform (cluster, WI, AR, GSM, IAM)
	cd terraform && terraform init -input=false >/dev/null && terraform apply

infra-destroy: ## Tear down all Terraform-managed GCP infra
	cd terraform && terraform destroy

eso-install: ## Install External Secrets Operator (idempotent)
	helm repo add external-secrets https://charts.external-secrets.io --force-update >/dev/null
	helm upgrade --install external-secrets external-secrets/external-secrets \
	  -n $(ESO_NS) --create-namespace --set installCRDs=true --wait --timeout 5m

gsm-push-key: ## Push local .env values to Google Secret Manager (container+IAM owned by Terraform)
	@test -f .env || (echo "No .env file — copy .env.example to .env and fill in your key" && exit 1)
	@echo "Converting .env to JSON and pushing a new version to '$(GSM_SECRET)'..."
	@python3 -c "import json; print(json.dumps(dict(l.strip().split('=',1) for l in open('.env') if l.strip() and not l.startswith('#'))))" \
	  | gcloud secrets versions add $(GSM_SECRET) --project=$(GCP_PROJECT) --data-file=-

gke-swap-pool: ## Ensure the swap-enabled Spot sandbox pool exists (gcloud until TF supports swapConfig)
	@gcloud container node-pools describe hermes-swap-pool --cluster hermes-svc --zone us-central1-a >/dev/null 2>&1 \
	  && echo "hermes-swap-pool already exists" || hack/gke-swap-pool.sh

deploy-gke: infra-apply images-push sandbox-install eso-install gsm-push-key gke-swap-pool ## PRODUCTION MODE: full GKE setup + deploy
	$(MAKE) gke-credentials
	helm upgrade --install hermes-service charts/hermes-service \
	  -n $(NAMESPACE) --create-namespace -f charts/hermes-service/values-gke.yaml
	kubectl -n $(NAMESPACE) rollout status deploy/hermes-gateway --timeout=300s
	@echo "Waiting for provider keys to sync from Secret Manager..."
	kubectl -n $(NAMESPACE) wait --for=condition=Ready externalsecret/hermes-provider-keys --timeout=120s
	@echo "Cycling warm-pool spares so new sandboxes get the synced key..."
	kubectl -n $(NAMESPACE) delete sandboxes -l agents.x-k8s.io/warm-pool-sandbox --ignore-not-found

images-push: ## Build+push gateway and mirror hermes image to Artifact Registry
	docker build --platform linux/amd64 -t $(AR_REPO)/hermes-gateway:latest .
	docker push $(AR_REPO)/hermes-gateway:latest
	docker pull --platform linux/amd64 $(HERMES_IMAGE)
	docker tag $(HERMES_IMAGE) $(AR_REPO)/hermes-agent:$(lastword $(subst :, ,$(HERMES_IMAGE)))
	docker push $(AR_REPO)/hermes-agent:$(lastword $(subst :, ,$(HERMES_IMAGE)))

help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-24s %s\n", $$1, $$2}'
