# Security Policy

## Reporting a vulnerability

Please report suspected vulnerabilities privately via
[GitHub Security Advisories](https://github.com/aditya-shantanu/ai-agent-service/security/advisories/new)
rather than public issues. You should receive an acknowledgement within a few
days. Please include reproduction steps and the deployment mode (kind or GKE).

## Supported versions

The `main` branch is the only supported line; there are no maintained release
branches yet.

## Isolation model (what this platform assumes)

- Every user's agent runs in its own pod, sandboxed with **gVisor** (GKE
  Sandbox) on GKE, with a **NetworkPolicy** restricting ingress to the gateway.
- The gateway verifies per-user bearer tokens (SHA-256 at rest, constant-time
  compare) before proxying; platform LLM keys are injected server-side and
  never exposed to clients.
- Agents execute untrusted LLM-driven code *by design* — the pod/sandbox
  boundary, not the agent process, is the security boundary. Reports that
  cross that boundary (sandbox escape, cross-user access, token forgery) are
  the highest-priority class.
