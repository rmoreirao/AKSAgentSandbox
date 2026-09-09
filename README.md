# DevSandbox

DevSandbox is an internal developer platform for isolated, persistent development
environments on AKS. The repository contains the complete PoC/MVP implementation;
see
[DEVSANDBOX_SPEC.md](DEVSANDBOX_SPEC.md) for the full design.

## Documentation

| Guide | Use it for |
| --- | --- |
| [Architecture](docs/architecture.md) | Azure topology, AKS components, sandbox lifecycle, and CLI interaction diagrams. |
| [Development](docs/development.md) | Technology stack, repository map, executable responsibilities, and local workflow. |
| [Security](docs/security.md) | Trust boundaries, implemented controls, credential handling, known PoC risks, and hardening priorities. |
| [Operations](docs/operations.md) | Supported lifecycle operations, health, troubleshooting, metrics, and audit investigation. |
| [Validation and deployment](docs/validation.md) | Local gates, cloud preflight, staged deployment, security checks, and E2E coverage. |
| [OpenAPI contract](api/openapi.yaml) | Authoritative public API operations and schemas. |
| [Product specification](DEVSANDBOX_SPEC.md) | Detailed requirements, product decisions, resource models, and acceptance criteria. |

> [!IMPORTANT]
> This repository implements a PoC, not a production security baseline. Review
> the [known security limitations](docs/security.md#known-poc-limitations)
> before deploying it beyond an isolated evaluation environment.

## Prerequisites

- Go 1.27.0 (the current `go 1.27` toolchain from `go.mod`)
- Node.js 22 and npm
- PowerShell 7
- Azure CLI with Bicep
- kubectl with Kustomize support


## Validate

```powershell
npm ci
pwsh ./scripts/validate.ps1 -Mode Static
pwsh ./scripts/validate.ps1 -Mode Build
pwsh ./scripts/build-images.ps1 -Action Validate
npx --no-install playwright test --list
pwsh ./scripts/security-validate.ps1 -Mode Static
pwsh ./scripts/deploy.ps1 -Stage Validate
```

Repository and image builds use `https://packagefeedproxy.microsoft.io/npm/`
for npm package acquisition. Published image release tags are immutable; advance
the version in `images/versions.json` before publishing changed images.

`Static` runs formatting, vet/tests (including OpenAPI consistency), both Bicep
builds, Kustomize/pin checks, PowerShell parsing, image metadata validation,
Section 14.5 static security assertions, and Playwright discovery. `Build`
builds every Go command and builds/smokes all container images when a Docker
daemon is available. An unavailable Docker daemon or cloud dependency is
reported as **Blocked**, never Passed; preflight exits 2 for Blocked.

## Deploy

`infra/foundation.bicep` is the subscription-scope entry point. It creates the
resource group and composes `infra/main.bicep`; the latter can also be deployed
at resource-group scope. The default PoC ingress uses Azure Front Door Standard
with separate Azure-owned API and web endpoints. It does not require a domain,
DNS delegation, hosts-file changes, or local certificate trust.

Review and apply a cloud deployment:

```powershell
$env:AZURE_SUBSCRIPTION_ID = '<subscription-id>'
$env:AZURE_LOCATION = 'westeurope'
$env:DEVSANDBOX_RESOURCE_PREFIX = 'devsbx'
$env:AZURE_DEPLOYMENT_PRINCIPAL_ID = '<deployment-principal-object-id>'
pwsh ./scripts/deploy.ps1 -Stage WhatIf
pwsh ./scripts/deploy.ps1 -Stage All
```

The PoC overlay vendors Agent Sandbox v1.0.0 with a verified release checksum,
pins its controller image by digest, and builds Sandbox Router from the matching
source commit. Render it locally with:

```powershell
kubectl kustomize ./deploy/kustomize/overlays/poc
```

Deployment and verification use AKS Run Command because the API server is
private. For troubleshooting or staged deployment, run:

```powershell
pwsh ./scripts/deploy.ps1 -Stage Foundation
pwsh ./scripts/deploy.ps1 -Stage Bootstrap
pwsh ./scripts/deploy.ps1 -Stage Images
pwsh ./scripts/deploy.ps1 -Stage Kubernetes
pwsh ./scripts/deploy.ps1 -Stage FrontDoor
pwsh ./scripts/deploy.ps1 -Stage Verify
pwsh ./scripts/smoke-kata.ps1 -ResourceGroupName <rg> -AksClusterName <aks>
```

The Kubernetes stage normally uses GitHub App credentials. For local PoC
deployment, it can use the current authenticated `gh` credential as an
owner-bound static credential when App registration cannot be completed
unattended. The generated local configuration marks this explicit PoC-only
mode; the GitHub credential is never written to `.env`.

`DEVSANDBOX_RESOURCE_GROUP`, `DEVSANDBOX_AKS_CLUSTER`,
`DEVSANDBOX_CONTAINER_REGISTRY`, and
`DEVSANDBOX_APP_ROUTING_CLIENT_ID` can override foundation outputs.

Run `pwsh ./scripts/preflight.ps1 -Mode Deployment` before cloud work and
`pwsh ./scripts/preflight.ps1 -Mode E2E` before deployed tests. Full input,
identity, quota, fixture, workflow secret, verification, cleanup, and Blocked
semantics are documented in [docs/validation.md](docs/validation.md).

## Use the DevSandbox CLI

After deployment, the repository root contains `devsandbox.exe` and an ignored,
secret-free `.env` configured for the deployed Azure Front Door endpoint.

```powershell
.\devsandbox.exe login
.\devsandbox.exe doctor
.\devsandbox.exe templates
.\devsandbox.exe up
```

The examples below use `devsandbox` for installations on `PATH`. Use
`.\devsandbox.exe` for the repository-local Windows executable.

### Command surface

| Command | Purpose |
| --- | --- |
| `devsandbox login` | Authenticate and store a short-lived platform session in the OS credential store. |
| `devsandbox doctor` | Check configuration, authentication, API access, templates, and repository readiness. |
| `devsandbox templates [show NAME]` | List templates or inspect one template. |
| `devsandbox up` | Create or resume a sandbox and perform its entry action. |
| `devsandbox list [--all]` | List active sandboxes or include stopped and failed sandboxes. |
| `devsandbox status [NAME]` | Show sandbox state, source, activity, and recent conditions. |
| `devsandbox shell [NAME]` | Open an interactive shell. |
| `devsandbox exec [NAME] -- COMMAND` | Run a command; add `--detach` to create a managed job. |
| `devsandbox jobs [NAME]` | List managed jobs; use `jobs stop` to stop one by ID. |
| `devsandbox port [NAME] REMOTE_PORT` | Forward a sandbox port to the local machine. |
| `devsandbox stop [NAME]` | Stop compute while retaining the workspace. |
| `devsandbox resume [NAME]` | Resume compute with the retained workspace. |
| `devsandbox delete [NAME]` | Permanently delete the sandbox and workspace. |

Run `devsandbox COMMAND --help` for all supported flags and variations.

### Common examples

Create a sandbox from the current repository, an explicit repository, or an
empty workspace:

```text
devsandbox up
devsandbox up --repo OWNER/REPOSITORY --template standard
devsandbox up --empty --template copilot --name investigation
```

Connect and run work:

```text
devsandbox shell my-sandbox
devsandbox exec my-sandbox -- go test ./...
devsandbox exec --detach my-sandbox -- pwsh -File ./build.ps1
devsandbox port my-sandbox 3000
```

Manage its lifecycle:

```text
devsandbox list --all
devsandbox status my-sandbox
devsandbox stop my-sandbox
devsandbox resume my-sandbox
devsandbox delete my-sandbox --yes
```

For automation, use explicit sandbox names, `--json`, and `up --no-attach`.
Commands do not prompt when stdin is noninteractive.

### Important behavior

- With no source option, `up` uses the current clean Git repository and verifies
  that its `HEAD` is available from GitHub.
- Commands with optional `[NAME]` can select the only sandbox associated with
  the current repository; pass a name when the selection may be ambiguous.
- Configuration precedence is command flags, process environment, the selected
  environment file, and built-in defaults.
- The CLI stores only the short-lived platform session in the native OS
  credential store. The GitHub bootstrap credential is not written to `.env`.

See [CLI interaction architecture](docs/architecture.md#how-the-cli-interacts-with-the-platform)
for the request flow and
[specification section 6.2](DEVSANDBOX_SPEC.md#62-cli-command-surface) for the
complete behavioral contract.

## Operations and observability

See [the PoC operator runbook](docs/operations.md) for safe top-level
`DevSandbox` deletion, dependency-aware probes, metrics, and the explicit lack
of an administrator API. [Log Analytics queries](docs/log-analytics.kql) and
the separate [`scripts/audit-e2e.ps1`](scripts/audit-e2e.ps1) audit validator
support Azure Monitor operations.
