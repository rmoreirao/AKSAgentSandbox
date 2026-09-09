# MVP-15 validation and deployment

## Local gates

Use Go 1.27.0, Node.js 22, PowerShell 7, Azure CLI with Bicep, and kubectl with
Kustomize:

```powershell
npm ci
pwsh ./scripts/validate.ps1 -Mode Static
pwsh ./scripts/validate.ps1 -Mode Build
az bicep build --file ./infra/foundation.bicep --stdout
az bicep build --file ./infra/main.bicep --stdout
kubectl kustomize ./deploy/kustomize/overlays/poc
pwsh ./scripts/build-images.ps1 -Action Validate
npx --no-install playwright test --list
pwsh ./scripts/security-validate.ps1 -Mode Static
```

Only the `Static` and `Build` jobs are pull-request gates. Actions are pinned to
full commit SHAs, Go comes from `go.mod`, and dependencies come from `npm ci`.
`images.yml` remains a manual image workflow. npm uses the Microsoft feed proxy
at `https://packagefeedproxy.microsoft.io/npm/`.

## Preflight and Blocked

`preflight.ps1` prints only missing **names**, never values. A missing input,
tool, Azure identity capability, selected VM-family quota, test identity, fixture,
or deployed dependency prints `Blocked:` and exits 2. A failed validation is
nonzero and is not converted into success. Static validation does not require
Azure credentials.

The default Front Door deployment and E2E inputs are:

```text
AZURE_SUBSCRIPTION_ID
AZURE_LOCATION
DEVSANDBOX_RESOURCE_PREFIX
AZURE_DEPLOYMENT_PRINCIPAL_ID
DEVSANDBOX_TEST_ORG
DEVSANDBOX_TEST_PRIVATE_REPO
DEVSANDBOX_TEST_PUBLIC_REPO
DEVSANDBOX_TEST_USER
DEVSANDBOX_TEST_USER_ID
DEVSANDBOX_TEST_USER_SESSION
DEVSANDBOX_TEST_SECOND_USER_SESSION
```

Kubernetes runtime configuration additionally requires these names. Store all
except the organization in GitHub environment **secrets**:

```text
DEVSANDBOX_GITHUB_APP_CLIENT_ID
DEVSANDBOX_GITHUB_APP_CLIENT_SECRET
DEVSANDBOX_PRIMARY_GITHUB_ORG
DEVSANDBOX_BROKER_TLS_CERTIFICATE
DEVSANDBOX_BROKER_TLS_PRIVATE_KEY
DEVSANDBOX_ROUTE_PUBLIC_KEY
```

The optional GitHub environment used by `deploy-poc.yml` stores
`DEVSANDBOX_PRIMARY_GITHUB_ORG`, `DEVSANDBOX_TEST_ORG`,
`DEVSANDBOX_TEST_PRIVATE_REPO`, `DEVSANDBOX_TEST_PUBLIC_REPO`, and
`DEVSANDBOX_TEST_USER` as variables. It stores the runtime names above (except
the organization), both test sessions, and `AZURE_CLIENT_ID`,
`AZURE_TENANT_ID`, `AZURE_SUBSCRIPTION_ID`, and
`AZURE_DEPLOYMENT_PRINCIPAL_ID` as secrets. OIDC is the only Azure login
mechanism; no Azure client secret is used. OIDC is not required for local
deployment through an authenticated `az login` session.

The deployment workflow derives `DEVSANDBOX_TEST_USER_ID` from the authenticated
primary test session before verification; local verification can derive it from
`gh api user`.

The deployment identity needs subscription/resource deployment and role
assignment permissions. After creation, Bicep grants scoped AcrPush and only
AKS Run Command invoke/result actions. Preflight checks visible role
assignments, regional and selected VM-family quota, exact deployed scopes when
available, test session separation, and repository resolution. E2E validates
the private fixture's LFS, submodule, push, and PR permissions.

## Manual PoC flow

```powershell
pwsh ./scripts/preflight.ps1 -Mode Deployment
pwsh ./scripts/deploy.ps1 -Stage Validate
pwsh ./scripts/deploy.ps1 -Stage WhatIf
pwsh ./scripts/deploy.ps1 -Stage Foundation
pwsh ./scripts/deploy.ps1 -Stage Bootstrap
pwsh ./scripts/deploy.ps1 -Stage Images
pwsh ./scripts/deploy.ps1 -Stage Kubernetes
pwsh ./scripts/deploy.ps1 -Stage FrontDoor
pwsh ./scripts/deploy.ps1 -Stage Verify
pwsh ./scripts/preflight.ps1 -Mode E2E
pwsh ./scripts/e2e.ps1 -Scenario All
pwsh ./scripts/cleanup-e2e.ps1 -Method Auto
```

All Kubernetes apply, rollout, inspection, security, and cleanup operations use
`az aks command invoke`; the AKS API remains private. Verify covers private AKS,
system/Kata pools, the AKS RuntimeClass, a scheduled large profile, Front Door,
the restricted Gateway origin, trusted endpoint TLS, workload identities,
system placement, health, RBAC, and network segmentation. `e2e.ps1 -Scenario All` covers baseline API/template
discovery, empty standard create/exec/list/status, stop/resume persistence,
repository behavior, shell/exec/job/tunnel, idle, concurrent quota rejection,
retention, VS Code/Playwright, Copilot, owner isolation, and cleanup.

The workflow always invokes idempotent cleanup. `cleanup-e2e.ps1` prefers the
owner API and falls back to AKS Run Command without printing authentication.
One-time VS Code URLs and credentials are redacted and are never workflow
outputs.

## Explicit PoC security blockers

The security checks do **not** claim production least privilege: sandbox egress
is unrestricted, a raw GitHub token is injected into sandbox memory, and Front
Door Standard forwards to the source-restricted AKS origin over HTTP rather
than end-to-end TLS. These are explicit PoC blockers. Static and deployed checks still enforce Kata,
resource profiles, non-root/no-host security, per-sandbox zero-RBAC identities,
projected broker audience, capability/seccomp controls, scoped API/broker/web
RBAC, separate origins/cookies, network segmentation, Gateway ports,
fragment-only browser credentials, and token-like-content scans.

## Deployed West Europe Front Door profile

The default profile uses:

- West Europe with `Standard_D4s_v6` system nodes and a
  `Standard_D16s_v6` Kata pool.
- Azure Front Door Standard with separate generated `azurefd.net` API and
  web endpoints, preserving origin isolation without requiring a domain.
- Browser-trusted TLS terminated by Front Door.
- An HTTP AKS Gateway origin restricted at the subnet to
  `AzureFrontDoor.Backend` and at the route to the exact `X-Azure-FDID` profile
  identifier.
- A personal owner boundary (`rmoreirao`) rather than mandatory organization
  membership.
- Public fixture `rmoreirao/AKSAgentSandbox` and private fixture
  `rmoreirao/AKSAgentSandbox-e2e-private`.

After local deployment, `scripts/configure-local.ps1` writes a secret-free
`.env` beside the generated `devsandbox.exe`. The CLI loads only that adjacent
file unless `DEVSANDBOX_ENV_FILE` explicitly selects another file. From a clean
PowerShell process, `.\devsandbox.exe up` requires no hosts-file entry,
certificate installation, Host/SNI override, or manually exported DevSandbox
variables.

GitHub App registration requires an interactive GitHub browser confirmation.
When that confirmation is unavailable, `deploy.ps1 -Stage Kubernetes` uses the
authenticated `gh` token through the owner-bound static validation provider,
and the CLI exchanges the current in-memory `gh` credential for a short-lived
platform session automatically. Only the platform session is cached in the OS
keyring.
The Copilot wrapper validates the supported GitHub `/user` endpoint, injects the
credential only into the Copilot process, and delegates entitlement handling to
Copilot CLI. It does not call GitHub's private Copilot token endpoint.
