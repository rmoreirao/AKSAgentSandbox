# Development guide

This guide maps the repository to the running DevSandbox platform and lists the
shortest supported development workflow. For product behavior and detailed
requirements, use [DEVSANDBOX_SPEC.md](../DEVSANDBOX_SPEC.md).

## Technology stack

| Area | Technology |
| --- | --- |
| Services and CLI | Go 1.27, Cobra, Gorilla WebSocket |
| Kubernetes controllers | controller-runtime and Kubernetes Go APIs |
| Infrastructure | Azure Bicep |
| Cluster deployment | Kubernetes manifests and Kustomize |
| Workload isolation | AKS Kata VM Isolation and Agent Sandbox |
| Azure services | Front Door, AKS, ACR, Key Vault, Managed Identity, Log Analytics |
| Automation | PowerShell 7 and GitHub Actions |
| API contract | OpenAPI 3.0 |
| UI validation | Playwright with Node.js 22 |
| Images | Dockerfiles and digest-pinned release metadata |

## Repository map

| Path | Contents |
| --- | --- |
| `cmd/` | Executable entry points for the CLI and platform processes. |
| `internal/` | CLI, API, authentication, gateway, broker, operator, repository, lifecycle, supervisor, and observability packages. |
| `api/v1alpha1/` | Kubernetes custom resource types and schemas. |
| `api/openapi.yaml` | Public management and connectivity API contract. |
| `infra/` | Subscription and resource-group Bicep deployments. |
| `deploy/kustomize/` | AKS namespaces, workloads, RBAC, policies, templates, and PoC overlay. |
| `deploy/runtime/` | Operator-rendered sandbox runtime fragment. |
| `images/` | Curated standard, VS Code, VS Code AI, Copilot, and management images. |
| `scripts/` | Validation, image, deployment, smoke, E2E, audit, and cleanup automation. |
| `tests/ui/` | Browser-based VS Code smoke coverage. |
| `docs/` | Architecture, security, operations, validation, and contributor guides. |

## Executables

| Command | Purpose |
| --- | --- |
| `devsandbox` | User-facing CLI. |
| `devsandbox-api` | Public authentication, lifecycle, repository, and connectivity API. |
| `devsandbox-web` | Browser gateway for authenticated VS Code traffic. |
| `devsandbox-operator` | Reconciles DevSandbox custom resources and upstream sandboxes. |
| `devsandbox-broker` | Exchanges bound workload identity for short-lived GitHub credentials. |
| `devsandbox-init` | Initializes or resumes the persistent workspace. |
| `devsandbox-agent` | Runs the authenticated process supervisor inside a sandbox. |
| `devsandbox-copilot` | Starts Copilot CLI with the supported runtime credential handling. |
| `devsandbox-opencode` | Starts OpenCode with process-only GitHub Copilot authentication. |

Sandbox Router is built from the Agent Sandbox version and source commit pinned
under `deploy/kustomize/base/upstream/agent-sandbox/`.

## Local workflow

Install the prerequisites listed in the [README](../README.md#prerequisites),
then restore the Node.js tools used by Playwright:

```powershell
npm ci
```

Run the two pull-request gates:

```powershell
pwsh ./scripts/validate.ps1 -Mode Static
pwsh ./scripts/validate.ps1 -Mode Build
```

`Static` checks Go formatting, vet and tests, OpenAPI coverage, Bicep builds,
Kustomize rendering, upstream checksums, PowerShell parsing, image metadata,
security assertions, and Playwright test discovery. `Build` builds all Go
commands and builds and smokes container images when Docker is available.

Useful focused commands are:

```powershell
go test ./internal/cli
go test ./internal/operator
go test ./internal/broker ./internal/auth
go test ./api/v1alpha1 -run '^TestOpenAPICoversSectionTenOperations$' -count=1
kubectl kustomize ./deploy/kustomize/overlays/poc
az bicep build --file ./infra/foundation.bicep --stdout
```

Use [Validation and deployment](validation.md) for cloud preflight, staged
deployment, smoke tests, and E2E requirements.

## Change guide

| Change | Start here | Also review |
| --- | --- | --- |
| Add or change a CLI command | `internal/cli/commands.go`, `internal/cli/connectivity.go` | README CLI reference, client models, API contract |
| Add an API operation | `api/openapi.yaml`, `internal/api/` | CLI client, authorization, OpenAPI consistency test |
| Change authentication | `internal/auth/`, `internal/api/auth_handlers.go` | Key Vault permissions, broker, security documentation |
| Change sandbox lifecycle | `internal/lifecycle/`, `internal/operator/` | CRD schema, E2E coverage, operations guide |
| Add a template | `deploy/kustomize/base/templates/`, `images/` | `images/versions.json`, image workflow, template tests |
| Change sandbox runtime | `deploy/runtime/`, `internal/supervisor/`, `internal/repository/` | Network policies, broker identity, security validation |
| Add an Azure resource | `infra/main.bicep`, `infra/modules/` | outputs, deployment script, preflight, validation guide |
| Change AKS workloads | `deploy/kustomize/base/` | RBAC, NetworkPolicy, probes, resource limits, Kustomize render |

## Infrastructure and image delivery

The supported deployment sequence is:

1. `infra/foundation.bicep` creates the resource group and invokes
   `infra/main.bicep`.
2. `scripts/deploy.ps1` bootstraps cluster dependencies and route-signing
   material by AKS Run Command.
3. Management and curated images are built and pushed to ACR.
4. The PoC Kustomize overlay is rendered and applied.
5. `infra/front-door-binding.bicep` binds Front Door to the deployed origins.
6. Verification checks infrastructure, workloads, routing, identities, and
   isolation.

Published image versions are declared in `images/versions.json`. Release tags
are immutable, Kubernetes workloads use digests, and a changed image requires a
new version rather than overwriting an existing release.

## Sources and generated artifacts

- Change source Bicep and manifests, not rendered files under `artifacts/`.
- Keep the upstream Agent Sandbox manifest and `pin.json` checksum aligned.
- Treat `api/openapi.yaml` as the public API source of truth.
- Keep the README CLI reference synchronized with the implemented Cobra command
  surface.
- Do not commit `.env`, runtime credentials, deployment secrets, or local test
  output.

## Related documentation

- [Architecture](architecture.md)
- [Security](security.md)
- [Operations](operations.md)
- [Validation and deployment](validation.md)
