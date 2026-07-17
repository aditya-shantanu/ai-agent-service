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

.PHONY: build test lint validate-hermes-image image kind-up kind-load deploy-kind dev e2e simulate-users undeploy sandbox-install images-push deploy-gke gke-credentials help

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

undeploy: ## Remove the helm release (users/claims are NOT deleted)
	helm uninstall hermes-service -n $(NAMESPACE)

sandbox-install: ## Install agent-sandbox release manifest into current cluster
	kubectl apply -f https://github.com/kubernetes-sigs/agent-sandbox/releases/download/$(AGENT_SANDBOX_VERSION)/sandbox-with-extensions.yaml

gke-credentials: ## Point kubectl at the GKE cluster
	gcloud container clusters get-credentials hermes-svc --zone=us-central1-a --project=$(GCP_PROJECT)

deploy-gke: images-push sandbox-install ## PRODUCTION MODE: push images + deploy to GKE (15m idle)
	helm upgrade --install hermes-service charts/hermes-service \
	  -n $(NAMESPACE) --create-namespace -f charts/hermes-service/values-gke.yaml
	kubectl -n $(NAMESPACE) rollout status deploy/hermes-gateway --timeout=300s

images-push: ## Build+push gateway and mirror hermes image to Artifact Registry
	docker build --platform linux/amd64 -t $(AR_REPO)/hermes-gateway:latest .
	docker push $(AR_REPO)/hermes-gateway:latest
	docker pull --platform linux/amd64 $(HERMES_IMAGE)
	docker tag $(HERMES_IMAGE) $(AR_REPO)/hermes-agent:$(lastword $(subst :, ,$(HERMES_IMAGE)))
	docker push $(AR_REPO)/hermes-agent:$(lastword $(subst :, ,$(HERMES_IMAGE)))

help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-24s %s\n", $$1, $$2}'
