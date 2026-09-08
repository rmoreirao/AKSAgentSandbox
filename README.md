# DevSandbox

DevSandbox is an internal developer platform for isolated, persistent development
environments on AKS. The repository contains the complete PoC/MVP implementation;
see
[DEVSANDBOX_SPEC.md](DEVSANDBOX_SPEC.md) for the full design.

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

## CLI

After a successful local deployment, the repository root contains an ignored,
secret-free `.env` generated from the Front Door outputs and a native
`devsandbox.exe`. With `az` authenticated for deployment and `gh` authenticated
for the PoC GitHub credential, create a sandbox for the current pushed
repository:

```powershell
.\devsandbox.exe up
```

The generated configuration selects the Azure Front Door API endpoint, the
`standard` template, the explicit `github-cli-static` authentication mode, and
the deployed GitHub login. Account-specific `gh auth token --user <login>`
lookup avoids ambient `GH_TOKEN` differences between terminals.
The CLI uses the current Git repository and its pushed HEAD, applies the
template's default resource profile, creates or resumes the sandbox, and
attaches to its shell. Run `.\devsandbox.exe doctor` for configuration,
authentication, API, and repository readiness checks.

Command flags override process environment, which overrides the generated
configuration. `--api-url` overrides `DEVSANDBOX_API_URL`; `--template`,
`--repo`, and the other `up` flags retain their existing behavior. Read
commands (`templates`, `templates show`, `list`, and `status`) accept `--json`.
Use `--no-attach` with `up` in automation; commands that require a choice never
prompt when stdin is not a terminal.

The CLI stores only the short-lived DevSandbox platform session in the native
OS credential store (Windows Credential Manager, macOS Keychain, or Linux
Secret Service). In the static PoC profile it obtains the GitHub bootstrap
credential from `gh auth token` only in process memory. It does not use a file
or plaintext credential fallback and fails clearly when the native credential
store or GitHub CLI authentication is unavailable.

## Operations and observability

See [the PoC operator runbook](docs/operations.md) for safe top-level
`DevSandbox` deletion, dependency-aware probes, metrics, and the explicit lack
of an administrator API. [Log Analytics queries](docs/log-analytics.kql) and
the separate [`scripts/audit-e2e.ps1`](scripts/audit-e2e.ps1) audit validator
support Azure Monitor operations.
