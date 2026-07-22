# Contributing

Thanks for your interest! This project is young and moving fast; issues and
PRs are welcome.

## Development setup

Requirements: Go (version in `go.mod`), Docker (or colima), kind, kubectl,
helm.

```sh
make dev        # kind cluster + agent-sandbox + deploy the platform
make test       # unit tests (fast, hermetic — fake clientsets + httptest)
make lint       # gofmt + go vet
make e2e        # full-loop functional test against the kind deployment (11 checks)
make bench      # UX latency benchmark (see benchmarks/README.md)
```

GKE deployment and operations: `docs/gke.md`.

## Pull requests

- `make test lint` must pass; CI runs both plus `helm lint`.
- If you touch the agent lifecycle (provision/suspend/resume/proxy), run
  `make e2e` against kind and say so in the PR.
- If you touch anything latency-sensitive, run `make bench-check` — budgets
  live in `benchmarks/budgets-*.yaml` and regressions fail the gate.
- Keep docs in sync: each fact has one canonical home (latency →
  `benchmarks/`, cost → `costcalc/`, rationale → `docs/design-decisions.md`).

## Design context

Read `docs/design-decisions.md` first — most "why is it like this?" questions
are answered there, numbered and dated.
