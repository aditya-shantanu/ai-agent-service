# --- Pinned versions (bump deliberately; keep in sync with docs/README) ---
HERMES_IMAGE        ?= nousresearch/hermes-agent:v2026.7.7.2
AGENT_SANDBOX_VERSION ?= v0.5.1

.PHONY: validate-hermes-image
validate-hermes-image: ## Validate the pinned Hermes image against our env contract
	HERMES_IMAGE=$(HERMES_IMAGE) hack/validate-hermes-image.sh

.PHONY: help
help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-24s %s\n", $$1, $$2}'
