#!/usr/bin/env bash
set -euo pipefail
gcloud container node-pools delete swap-test-pool \
  --cluster hermes-svc --zone us-central1-a --quiet
