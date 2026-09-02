# DevSandbox PoC/MVP Specification

Status: Draft implementation specification  
Target: Internal proof of concept and MVP  
Primary interface: `devsandbox` CLI  
Workload platform: Azure Kubernetes Service (AKS)  
Sandbox runtime: Kubernetes Agent Sandbox with AKS Pod Sandboxing/Kata Containers  
Infrastructure as code: Bicep

## 1. Purpose

DevSandbox is an internal developer platform that creates isolated, cloud-hosted
development environments on AKS. A developer can start a sandbox from a local
Git repository, an explicit GitHub repository, or an empty workspace; connect
through a shell, browser-based VS Code, or GitHub Copilot CLI; run development
commands; and stop or resume the environment without losing the workspace.

Kubernetes Agent Sandbox owns the low-level sandbox Pod and persistent volume
lifecycle. AKS Pod Sandboxing runs every developer workload with the
`kata-vm-isolation` RuntimeClass so that each sandbox has a dedicated guest
kernel. DevSandbox adds the product-facing API, CLI, authentication, templates,
repository initialization, quotas, idle handling, connectivity, and audit
events.

This specification is intentionally optimized for implementation by GitHub
Copilot in autopilot mode. Each work package defines concrete deliverables and
automated validation. The implementation must not be considered complete until
the applicable checks have run successfully.

## 2. Product Goals

The MVP must:

1. Provide a `devsandbox` CLI for creating and managing developer sandboxes.
2. Run sandbox workloads in an isolated AKS cluster using Kata.
3. Support standard, VS Code, and Copilot templates.
4. Automatically clone a supported GitHub repository at an exact commit.
5. Make Git and GitHub credentials available without copying local SSH keys,
   GitHub CLI configuration, or local PATs.
6. List active and retained sandboxes owned by the current user.
7. Automatically stop compute after at most two hours without qualifying
   activity.
8. Preserve `/workspace` across stop and resume.
9. Provide shell, command execution, managed detached jobs, port tunneling, and
   browser-based VS Code connectivity.
10. Provision all Azure infrastructure through Bicep.
11. Include repeatable automated validation for source code, Bicep,
    containers, Kubernetes resources, cloud behavior, and the VS Code UI.

## 3. Non-Goals for the MVP

The following are explicitly outside the MVP:

- A custom web dashboard.
- Public or organization-shareable sandbox URLs.
- Multi-user access to one sandbox.
- Product-level administrator workflows.
- Continuous GitHub organization membership or repository-access revocation
  monitoring.
- Generic user secret injection.
- Private package registry credentials.
- Azure workload identities for user sandbox code.
- Repository devcontainer processing.
- User-defined templates or arbitrary images.
- Container-image builds inside a sandbox.
- Warm pools or a sub-30-second startup objective.
- Uploading dirty working trees, untracked files, or unpushed commits.
- Full shell or command transcripts.
- Multi-region or multi-cluster scheduling.
- A transactional product database.
- Extensive test coverage beyond focused unit tests and critical smoke tests.

## 4. Users and Access Model

### 4.1 Developer

A developer is an active member of the configured primary GitHub organization.
All organization members may use the MVP. A developer may only access their own
sandboxes through the product API.

### 4.2 Kubernetes operator

AKS administrators retain direct cluster access for emergency operations. The
MVP does not expose an administrator API. Operators must delete the top-level
`DevSandbox` custom resource when permanently removing a sandbox. Deleting only
the Pod or the upstream Agent Sandbox resource may cause reconciliation to
recreate it.

### 4.3 Service identities

All trusted services run in AKS and use dedicated Kubernetes ServiceAccounts.
Only services that call Azure APIs are federated to dedicated user-assigned
managed identities through AKS Workload Identity. Sandbox service accounts
have no Kubernetes RBAC permissions and do not automatically mount a Kubernetes
API token.

## 5. Confirmed Product Decisions

| Area | MVP decision |
| --- | --- |
| Foundation | Custom DevSandbox API and CLI over Kubernetes Agent Sandbox |
| Tenancy | Internal users from one primary GitHub organization |
| Deployment | One AKS cluster in one Azure region |
| Workload namespace | Shared sandbox namespace |
| Runtime | `kata-vm-isolation` on an Azure Linux Kata node pool |
| State store | Kubernetes custom resources and Leases |
| Historical audit | Azure Monitor/Log Analytics only |
| Management plane | API, web gateway, broker, and operator on the AKS system pool |
| Public ingress | AKS Application Routing Gateway API with separate API and sandbox-web origins |
| Workload network | Dedicated isolated VNet with no corporate VNet peering |
| Egress | Unrestricted egress, with the associated risk explicitly accepted |
| Sandbox access | Owner only |
| Persistence | `/workspace` PVC only |
| Workspace storage | Azure Disk CSI, Standard SSD |
| Idle policy | Configurable below, but never above, two hours |
| Idle warning | None |
| Stopped retention | Seven days |
| Failed-create retention | 24 hours |
| Active quota | Three sandboxes per user |
| Retained quota | Ten sandboxes and 200 GiB provisioned storage per user |
| Startup target | Best effort, up to five minutes |
| Kata capacity | At least one Kata node remains available |
| Warm pools | Deferred |
| Git writes | Push branches and create/update pull requests |
| Git local state | Require a clean worktree and a pushed exact commit |
| Generic secrets | Deferred |
| Devcontainers | Deferred to a later phase |

## 6. Functional Specification

### 6.1 CLI conventions

The executable name is `devsandbox`.

All resource-changing commands must support structured errors. Read commands
must support `--json` so automated callers do not parse human-readable tables.
Interactive prompts are allowed only when stdin is a TTY. A command that needs
input in a noninteractive environment must fail with a clear message and
available choices.

Required exit behavior:

| Code | Meaning |
| --- | --- |
| 0 | Success |
| 2 | Invalid arguments or required noninteractive choice |
| 3 | Authentication required or expired |
| 4 | Sandbox/repository/template not found or target is ambiguous |
| 5 | Conflict, invalid state transition, or quota exceeded |
| 6 | Provisioning or infrastructure failure |
| 7 | Remote execution transport failed before an exit code was available |
| 10 | Unexpected internal error |

`devsandbox exec` must return the remote process exit code unchanged when it is
available. A nonzero remote exit code must not be converted to a generic
DevSandbox error code.

### 6.2 CLI command surface

#### `devsandbox login`

- Starts a GitHub App device authorization flow through the management API.
- Requires membership in the primary GitHub organization.
- Stores only the platform session credential in the local operating-system
  credential store.
- The management plane stores the refresh credential in Azure Key Vault.
- A successful login is reused for Git, GitHub CLI, and Copilot access.

#### `devsandbox templates`

- Lists the available template names, versions, descriptions, default profiles,
  and entry experiences.
- Supports `devsandbox templates show <name> [--json]`.

#### `devsandbox up`

Supported forms:

```text
devsandbox up
devsandbox up --repo OWNER/REPOSITORY
devsandbox up --repo https://github.com/OWNER/REPOSITORY.git
devsandbox up --empty
devsandbox up --template standard|vscode|copilot
devsandbox up --profile small|medium|large
devsandbox up --name NAME
devsandbox up --ref BRANCH_OR_SHA
devsandbox up --idle-timeout DURATION
devsandbox up --resume-existing
devsandbox up --new
devsandbox up --no-attach
```

Behavior:

- With no source argument, inspect the current directory for a Git repository.
- If no Git repository is present, require `--repo` or `--empty`.
- If `--template` is omitted, prompt in a TTY. In noninteractive mode, fail and
  list the available templates.
- If more than one valid GitHub remote exists and `origin` cannot be selected,
  prompt in a TTY. In noninteractive mode, require an explicit repository.
- Allow multiple sandboxes for the same repository and branch.
- Generate a DNS-safe name when `--name` is not supplied. The generated name
  should include a repository or template prefix plus a short random suffix.
- If an equivalent stopped sandbox exists, prompt to resume or create a new
  sandbox. Automation must supply either `--resume-existing` or `--new`.
- `--resume-existing` and `--new` are mutually exclusive.
- Wait for the sandbox to become ready or fail.
- By default, perform the template entry action:
  - standard: attach a shell;
  - vscode: open the authenticated browser URL;
  - copilot: launch the `copilot` CLI.
- `--no-attach` returns after the sandbox is ready.

#### `devsandbox list`

- Default output includes `Provisioning`, `Initializing`, `Running`,
  `Resuming`, and `Stopping`.
- `devsandbox list --all` also includes `Stopped` and `Failed`.
- Results are restricted to the authenticated owner.
- The table includes:
  - name;
  - state;
  - template and pinned version;
  - profile;
  - repository and ref, or `empty`;
  - age;
  - last qualifying activity;
  - idle deadline when running;
  - retention deadline when stopped or failed.

#### `devsandbox status [NAME]`

- Returns full user-visible status and recent conditions.
- If `NAME` is omitted, resolve the only sandbox associated with the current
  repository.
- Prompt when ambiguous in a TTY; fail when ambiguous in automation.
- `--json` includes stable field names suitable for smoke-test scripts.

#### `devsandbox shell [NAME]`

- Opens an interactive terminal through an authenticated WebSocket.
- Does not expose Kubernetes credentials or require `kubectl`.
- Renews the activity Lease while the session remains connected.
- Exits when the sandbox stops, is deleted, or the network connection closes.

#### `devsandbox exec [NAME] -- COMMAND [ARGUMENTS...]`

- Runs a command as the non-root development user.
- Streams stdout, stderr, and the exit code.
- Renews activity for the duration of a foreground command.
- `--detach` creates a managed background job and returns its job ID.
- Do not log the full command or its output to Azure Monitor.

#### `devsandbox jobs [NAME]`

- Lists managed detached jobs with ID, state, start time, end time, and exit
  status.
- Does not persist the full command in product audit logs.
- Supports stopping a job by ID.
- Active managed jobs suppress idle stopping.
- Managed jobs may run indefinitely in the MVP.
- An arbitrary process started with `nohup`, `&`, or from an interactive shell
  is not a managed job and does not independently suppress idle stopping after
  the user connection closes.

#### `devsandbox port [NAME] REMOTE_PORT`

- Creates an owner-authenticated local TCP tunnel.
- Supports optional `--local-port`.
- Carries raw TCP bytes inside the authenticated WebSocket to the in-sandbox
  agent, which opens the destination connection locally. Sandbox Router only
  transports HTTP/WebSocket traffic and does not need generic TCP routing.
- Does not create a public Kubernetes Service or Ingress.
- Renews the activity Lease while the tunnel is connected.

#### `devsandbox stop [NAME]`

- Requests a transition to `Stopped`.
- Terminates running processes and managed jobs.
- Suspends the upstream Agent Sandbox while retaining the workspace PVC.
- Starts or resets the seven-day retention period.

#### `devsandbox resume [NAME]`

- Recreates compute from the same pinned template digest.
- Reattaches the existing workspace PVC.
- Does not reclone or overwrite an initialized workspace.
- Obtains a fresh GitHub user token.
- Starts a new idle deadline.

#### `devsandbox delete [NAME]`

- Permanently deletes the DevSandbox, upstream Sandbox, Pod, Service, workspace
  PVC, runtime credentials, job records, and connection leases.
- Requires interactive confirmation unless `--yes` is supplied.
- Must be idempotent.

### 6.3 Repository source behavior

The MVP supports:

1. The current local Git repository.
2. An explicit GitHub repository URL or `OWNER/REPOSITORY`.
3. An empty workspace.

For a current local repository:

- Prefer `origin` when it is a valid GitHub remote.
- Otherwise prompt among valid GitHub remotes.
- Require the working tree to be clean, including staged and unstaged tracked
  changes.
- Require HEAD to be reachable from the selected remote.
- Reject unpushed commits instead of transferring a patch or Git bundle.
- Record the exact commit SHA.
- Copy local `user.name` and `user.email`; fall back to the GitHub profile and a
  GitHub noreply address.

For an explicit repository:

- Use `--ref` when supplied.
- Otherwise resolve the repository default branch.
- Record the resolved exact commit before creation succeeds.

Clone behavior:

- Canonicalize GitHub SSH and HTTPS URLs to HTTPS.
- Use a partial clone with `--filter=blob:none`.
- Checkout the exact recorded commit.
- Attach a branch only when the requested branch still resolves safely to that
  commit; otherwise remain detached and report it.
- Support Git LFS.
- Support public submodules and private submodules in the primary GitHub
  organization when the user and GitHub App both have access.
- Reject local-path, `file://`, or unsupported submodule URLs.
- Clone into `/workspace/repo`.
- Reserve `/workspace/.devsandbox` for platform metadata and non-secret
  persisted settings.
- Execute no repository-provided commands after cloning.

Private repository support is limited to repositories owned by the primary
GitHub organization. Public GitHub repositories outside the organization may be
cloned without credentials and are read-only through the platform.

### 6.4 GitHub authentication and credentials

DevSandbox uses a GitHub App user authorization flow.

Minimum intended App permissions:

- Metadata: read.
- Contents: read/write.
- Pull requests: read/write.
- Organization members: read, if required for membership validation.
- User email/profile: minimum access required for identity fallback.

The App must not request GitHub Actions workflow write permission in the MVP.

Credential rules:

- Never copy local SSH private keys, `~/.config/gh`, local credential-helper
  files, or local PATs.
- Keep GitHub App refresh credentials in Azure Key Vault.
- User access tokens expire and are refreshed centrally.
- Every refresh or credential replacement acquires the same per-user
  Kubernetes Lease, re-reads the latest Key Vault secret version after taking
  the lock, and writes any rotated refresh token before releasing the lock.
  API and broker replicas must use one shared credential-manager package and
  must not refresh outside this path.
- On `invalid_grant`, the credential manager may re-read Key Vault and retry
  once only when another writer stored a newer version; otherwise it surfaces
  reauthentication as required.
- Never place a token in a `DevSandbox` or upstream `Sandbox` resource, Pod
  specification, image layer, command-line argument, log, or workspace file.
- A sandbox receives a current token through a runtime credential agent and a
  memory-backed file.
- Git uses a credential helper that reads the current runtime token.
- The `gh` wrapper reads the current token for each invocation.
- The Copilot template launches `copilot` with
  `COPILOT_GITHUB_TOKEN` populated at process start.
- A Copilot process that outlives its token may require restart in the MVP.

The accepted MVP model injects the raw user token into the sandbox runtime.
Therefore, code running as the developer user can read and exfiltrate the token.
The token can reach every repository available to both the user and the GitHub
App installation, not only the cloned repository. This is a known PoC risk.

### 6.5 Templates

Templates are platform-owned and immutable after publication. Every sandbox
records a template name, semantic version, and exact image digest. Resume uses
the same digest.

#### Standard template

- Default profile: small.
- Non-root development user.
- POSIX-compatible shell.
- Core command-line utilities.
- CA certificates.
- Git and Git LFS.
- Platform credential helper.
- DevSandbox supervisor/agent.
- No language runtime or OS package installation through `sudo`.

#### VS Code template

- Default profile: medium.
- Includes the standard baseline.
- Runs browser-based code-server.
- Code-server authentication is disabled only on the internal sandbox listener;
  the sandbox web gateway enforces owner authentication.
- Persists settings and extensions under `/workspace/.devsandbox/vscode`.
- Exposes code-server only through Sandbox Router and the authenticated
  sandbox web gateway.

#### Copilot template

- Default profile: medium.
- Includes the standard baseline.
- Includes GitHub CLI `gh`.
- Includes standalone GitHub Copilot CLI `copilot`.
- Includes Playwright CLI, Playwright operating-system dependencies, and
  Chromium.
- Mounts an appropriately sized memory-backed `/dev/shm`.
- Uses the centrally refreshed GitHub user token for GitHub and Copilot
  authentication.

Template images must be built in ACR, referenced by digest, and include version
labels. Installation mechanisms and upstream versions must be pinned during
implementation.

### 6.6 Compute profiles and quotas

Initial profiles:

| Profile | CPU | Memory | Workspace PVC |
| --- | ---: | ---: | ---: |
| small | 2 vCPU | 4 GiB | 20 GiB |
| medium | 4 vCPU | 8 GiB | 40 GiB |
| large | 8 vCPU | 16 GiB | 80 GiB |

Profile CPU and memory values are rendered as both Kubernetes requests and
limits on the sandbox workload container. For Kata on AKS, the Pod VM is sized
from limits rather than requests; omitting limits can result in a one-vCPU,
512-MiB sandbox regardless of the selected profile.

AKS/Kata RuntimeClass overhead must be included separately in cluster capacity
planning. `/dev/shm` and every memory-backed volume consume memory from the
fixed guest VM allocation and must fit within the selected profile limit.

Per-user limits:

- Maximum three active sandboxes.
- Maximum ten retained stopped or failed sandboxes.
- Maximum 200 GiB total provisioned retained workspace storage.

Quota checks occur before resource creation and before resume. Because all
sandboxes share a namespace, quota enforcement belongs to DevSandbox admission
and reconciliation, not namespace `ResourceQuota`.

Use one internal `DevSandboxUserQuota` resource per immutable GitHub user ID.
Creation, resume, stop, failure, and deletion reserve or release active count,
retained count, and provisioned GiB using Kubernetes `resourceVersion`
compare-and-swap with conflict retry. A list-then-create quota check is not
sufficient because concurrent requests could bypass it.

### 6.7 Lifecycle and idle behavior

The user-selectable idle timeout must be greater than zero and no greater than
two hours.

Qualifying activity:

- An authenticated CLI/API operation against the sandbox.
- An active shell.
- An active VS Code browser connection.
- An active local port tunnel.
- A foreground `exec`.
- An explicitly managed detached job.

Implementation assumptions:

- Connected clients renew an activity Lease approximately every 30 seconds.
- A connection heartbeat is stale after approximately 90 seconds.
- Every end-to-end WebSocket channel sends ping/pong traffic at least every 60
  seconds so the AKS Gateway API ingress path does not close an otherwise idle
  shell or tunnel.
- The operator coalesces activity updates instead of continuously writing the
  full `DevSandbox` status.
- When a managed job exits, the idle clock starts from the job completion time.
- No warning is emitted before an idle stop.

The normal idle transition sets the upstream Sandbox
`spec.operatingMode: Suspended`. The implementation should also mirror an
appropriate hard deadline to upstream `shutdownTime` with `Retain` as a
fail-safe. Resume sets `operatingMode: Running` and a fresh future deadline.

Retention:

- Manually or automatically stopped sandboxes are deleted seven days after the
  most recent stop.
- Failed creations are deleted after 24 hours.
- Resuming clears the stopped retention deadline.
- Deletion cascades from the top-level `DevSandbox`.

### 6.8 Connectivity

Users never receive AKS credentials.

- AKS Application Routing with the Kubernetes Gateway API terminates public TLS
  and routes REST/CLI traffic to the management API Service.
- Browser-hosted sandbox content uses a second hostname and HTTPRoute to the
  sandbox web gateway Service so untrusted code-server content never shares an
  origin or cookie scope with the management REST API.
- CLI shell, exec, jobs, and port tunnels use authenticated WebSockets through
  the same public Gateway.
- After owner authorization, the API and sandbox web gateway connect directly
  to Sandbox Router over ClusterIP networking without exposing a generic
  Kubernetes proxy.
- The API accesses DevSandbox resources directly with a narrowly scoped
  Kubernetes ServiceAccount. The sandbox web gateway has no Kubernetes write
  permissions and routes only from API-signed, short-lived claims.
- Routing headers such as sandbox UID and destination port are generated by a
  trusted service and are never accepted directly from an unauthenticated user.
- Browser-based VS Code uses a short-lived, single-use URL that exchanges for
  an HttpOnly, host-scoped, `SameSite=Strict` session on the sandbox web
  hostname.
- The one-time credential is carried only in the URL fragment. A no-store
  bootstrap page reads the fragment, POSTs it in the request body for exchange,
  removes it from browser history with `history.replaceState`, and then loads
  code-server from a clean URL. Credentials are prohibited in URL paths and
  query strings because Gateway access logs record them.
- The bootstrap response sets `Cache-Control: no-store`,
  `Referrer-Policy: no-referrer`, and a restrictive Content Security Policy,
  and loads no third-party scripts or assets.
- Arbitrary application ports are local-tunnel only.
- There is no direct public Service, Gateway, HTTPRoute, or LoadBalancer for a
  sandbox.
- The only private in-cluster management endpoint reachable from the workload
  namespace is the authenticated credential-broker Service and port. Public
  origins remain internet endpoints and enforce their normal authentication.

### 6.9 Audit and observability

Send structured events and metrics to Azure Monitor/Log Analytics.

Audit events:

- login success/failure;
- sandbox create, ready, stop, resume, delete, and failure;
- template and profile selected;
- repository ID/URL and commit SHA;
- shell, VS Code, exec, tunnel, and job session start/end;
- quota rejection;
- credential refresh success/failure without token contents.

Do not capture:

- shell input;
- command stdout/stderr;
- full detached-job command lines;
- file contents;
- tokens or refresh credentials.

Required metrics:

- active sandboxes by template/profile/state;
- create and resume duration;
- clone duration and failures;
- idle stops;
- retained PVC count and provisioned GiB;
- active connections and managed jobs;
- controller reconciliation failures;
- token refresh failures.

## 7. State Model

User-visible states:

```text
Requested
  -> Provisioning
  -> Initializing
  -> Running
      -> Stopping
      -> Stopped
          -> Resuming
          -> Running
      -> Failed
  -> Failed

Any nonterminal state -> Deleting -> Deleted
```

`Idle` is not a persisted state. It is a calculated condition based on the last
activity Lease, active connections, and managed jobs.

The state machine must reject invalid transitions, including:

- resuming an already running sandbox;
- connecting to a stopped or failed sandbox;
- changing the template or profile of an existing sandbox;
- resuming after the retention deadline or deletion has begun.

The following operations are required idempotent no-ops:

- stopping an already stopped sandbox;
- deleting an already deleted or missing sandbox when the caller previously
  owned that resource.

## 8. Architecture

### 8.1 System context

```text
Developer workstation
  |
  | devsandbox CLI / browser
  | HTTPS + WebSocket
  v
AKS-managed public load balancer
  |
  v
Application Routing Gateway API (`approuting-istio`)
  +-- api.<base-domain> HTTPS listener/HTTPRoute
  |     -> devsandbox-api Service
  +-- sandbox.<base-domain> HTTPS listener/HTTPRoute
        -> devsandbox-web Service

Private AKS cluster
  |
  +-- System node pool
  |     +-- DevSandbox API
  |     +-- Sandbox web gateway
  |     +-- GitHub credential broker
  |     +-- DevSandbox operator
  |     +-- Agent Sandbox controller
  |     +-- Sandbox Router
  |     +-- Gateway proxy
  |     +-- DevSandbox CRDs and Leases
  |
  +-- Kata user node pool
        +-- Shared workload namespace
              +-- upstream Sandbox
              +-- Kata-isolated Pod
              +-- workspace Azure Disk PVC

External services
  +-- GitHub App and GitHub APIs
  +-- Azure DNS
  +-- Azure Key Vault
  +-- Azure Container Registry
  +-- Azure Monitor / Log Analytics
```

### 8.2 Azure deployment topology

The Bicep deployment creates:

- One dedicated resource group.
- One isolated VNet with:
  - an AKS node subnet, default `/20`;
  - optional private-endpoint subnets if required by implementation.
- Default VNet address space: `/16`.
- No corporate VNet peering in the MVP.
- Log Analytics workspace.
- Azure Container Registry.
- Azure Key Vault using Azure RBAC.
- An Azure DNS zone, or an explicit existing Azure DNS zone resource ID.
- User-assigned managed identities and federated identity credentials for:
  - the management API;
  - the credential broker;
  - the Application Routing DNS/TLS ServiceAccount.
- One private AKS cluster with:
  - a normal system node pool;
  - Azure CNI powered by Cilium;
  - OIDC issuer and workload identity enabled;
  - the Azure Key Vault provider for Secrets Store CSI Driver enabled;
  - the managed Gateway API standard CRDs enabled;
  - the Application Routing add-on enabled;
  - the sidecar-less Application Routing Istio Gateway API implementation
    enabled;
  - a separate Azure Linux user node pool with
    `workloadRuntime: KataVmIsolation`;
  - default Kata SKU `Standard_D16s_v5`, parameterized but not changed without
    repeating profile scheduling validation;
  - Kata `maxPods: 8`;
  - Kata autoscaling with minimum one and default maximum ten nodes;
  - a default `Standard_D4s_v5` system pool.
- One public Kubernetes `Gateway` using `gatewayClassName: approuting-istio`,
  with separate HTTPS listeners for the API and sandbox-web hostnames.
- Separate `HTTPRoute` resources for the API and sandbox web gateway.
- Namespace-scoped Application Routing `ExternalDNS` configuration for the two
  hostnames.
- TLS integration using an unversioned Key Vault certificate URI that covers
  both hostnames.
- Required role assignments for ACR, Key Vault, Azure DNS, monitoring, and
  deployment.

The AKS resource must use a stable API version that supports the managed
Gateway API fields, `2026-03-01` or newer. Its Bicep configuration must include
the equivalent of:

```bicep
ingressProfile: {
  gatewayAPI: {
    installation: 'Standard'
  }
  webAppRouting: {
    enabled: true
    gatewayAPIImplementations: {
      appRoutingIstio: {
        mode: 'Enabled'
      }
    }
  }
}
```

Do not use the managed NGINX Application Routing ingress for new DevSandbox
routes. It is supported only through November 2026; the Gateway API
implementation is the long-term AKS ingress path.

The private AKS API and the public application data plane are independent. The
Gateway controller provisions an external Azure Load Balancer for the Gateway,
while the Kubernetes API server remains private. All DevSandbox management
Deployments, the generated Gateway proxy Deployment, Agent Sandbox controller,
and Sandbox Router must be constrained to the normal system node pool and must
not use the Kata RuntimeClass.

The AKS-provided runtime class name used by sandbox Pods is
`kata-vm-isolation`. The implementation must verify the RuntimeClass after
deployment instead of creating a custom RuntimeClass by default.

The AKS API server remains private. GitHub-hosted CI and developer workstations
must perform Kubernetes deployment and cloud validation through
`az aks command invoke`; they must not depend on direct private API reachability.
The deployment identity requires the scoped
`Microsoft.ContainerService/managedClusters/runcommand/action` and
`Microsoft.ContainerService/managedClusters/commandResults/read` permissions.

All Azure resources must be defined in Bicep. Kubernetes objects may be defined
as YAML/Kustomize and applied by the deployment workflow after Bicep completes.
Terraform must not be introduced.

### 8.3 Management API

The management API is a stateless Go Deployment in the trusted
`devsandbox-system` namespace on the AKS system node pool. It is exposed only
through a public API Service referenced by the API `HTTPRoute`. A second
ClusterIP Service/port exposes only the one-time URL exchange to the web gateway
and is not referenced by any public route. Its ServiceAccount has scoped RBAC
for DevSandbox domain resources and Leases, and its dedicated Workload Identity
can access only the required Key Vault secrets and signing key. It is
responsible for:

- GitHub device-flow login and organization membership validation;
- issuing signed platform session tokens;
- storing and refreshing GitHub App credentials in Key Vault;
- enforcing owner authorization at the public boundary;
- template and sandbox REST APIs;
- CLI WebSocket upgrade and reverse proxying;
- one-time VS Code URLs;
- event and metric emission;
- direct create/list/watch/update/delete operations on the required DevSandbox
  custom resources;
- atomic one-time URL state stored in a short-lived Kubernetes Lease or
  equivalent internal CRD;
- short-lived route authorization signed with a private key held in Key Vault;
- connecting authorized shell, exec, job, and port sessions to Sandbox Router.

The API must not expose generic Kubernetes operations, proxy arbitrary
Kubernetes API paths, return Kubernetes credentials, or receive permission to
read Kubernetes Secrets. It does not use a relational or document database.

### 8.4 Sandbox web gateway

The sandbox web gateway is a separate stateless Go Deployment, Service,
hostname, and HTTPRoute in `devsandbox-system`. It:

- exchanges a short-lived, single-use VS Code URL for a host-only session;
- serves the no-store bootstrap page that POSTs a fragment credential and
  redirects to a clean URL before proxying code-server;
- exposes no general sandbox management API;
- reverse proxies only an authorized owner's selected sandbox web route;
- calls a NetworkPolicy-protected internal API exchange endpoint to atomically
  consume the one-time URL;
- validates API-signed route claims with a public verification key and connects
  directly to Sandbox Router;
- runs with automatic Kubernetes API token mounting disabled and no Kubernetes
  RBAC permissions;
- never sets a cookie for the management API hostname.

### 8.5 GitHub credential broker

The credential broker is a dedicated Go Deployment and ClusterIP Service in
`devsandbox-system`. It has no public Gateway or HTTPRoute. A NetworkPolicy
allows the workload namespace to reach only its dedicated Service port while
denying workload access to all other management Services.

The broker:

- accepts only a short-lived projected Kubernetes ServiceAccount token with
  the exact broker audience;
- validates the token through the Kubernetes TokenReview API;
- binds the authenticated namespace, ServiceAccount UID, Pod UID, and
  DevSandbox UID instead of trusting caller-supplied owner data;
- reads only the ServiceAccount, Pod, and DevSandbox fields required to resolve
  that binding;
- uses its own Workload Identity to read and refresh the owner's GitHub App
  credential in Key Vault;
- uses a per-user Lease shared with the API to serialize refresh-token
  replacement across all replicas;
- returns only a current user access token for the resolved owner;
- never logs, persists, or places the token in a Kubernetes resource.

The broker has a separately scoped ServiceAccount, Kubernetes RBAC policy, and
user-assigned managed identity. It is not a general management API and cannot
create, update, or delete sandboxes.

### 8.6 DevSandbox operator

The operator uses `controller-runtime` and reconciles:

- `DevSandboxTemplate`;
- `DevSandbox`;
- `DevSandboxUserQuota`;
- `DevSandboxJob` status if represented as a CRD;
- activity Leases;
- the rendered upstream Agent Sandbox `Sandbox`;
- workspace PVC ownership;
- quota and retention rules.

It creates upstream `Sandbox` resources directly. Warm-pool-oriented
`SandboxClaim` resources are not used by the MVP.

The operator creates the workspace PVC itself with an owner reference and
cleanup finalizer on `DevSandbox`, then references that existing claim in the
rendered upstream Pod template. It must not use upstream
`volumeClaimTemplates`, because DevSandbox owns retention, quota accounting,
and final disk cleanup.

### 8.7 Sandbox init and supervisor

Every template includes platform-owned components:

#### Init component

- Mounts `/workspace`.
- Uses a dedicated ServiceAccount created for that DevSandbox. The account has
  zero Kubernetes RBAC permissions and is named from the DevSandbox UID.
- Uses an explicitly mounted, short-lived projected token with a dedicated
  broker audience to request a current GitHub credential.
- The broker validates the token through the Kubernetes TokenReview API,
  resolves the Pod UID and ServiceAccount directly to the owning DevSandbox,
  and only returns that DevSandbox owner's credential.
- Clones and initializes the repository only when the workspace marker is
  absent.
- Writes non-secret initialization metadata to
  `/workspace/.devsandbox/initialized.json`.
- On resume, validates the marker and exits without modifying the repository.

#### Supervisor/agent

- Runs as the non-root development user.
- Starts template-specific services.
- Provides authenticated internal RPC for shell, exec, jobs, health, and
  activity.
- Stores no long-lived credential on the PVC.
- Reads current credentials from memory-backed runtime storage.
- Reports managed-job state without exporting full commands or output.
- Shuts down child processes when the sandbox is stopped.

### 8.8 Storage

- One ReadWriteOnce Azure Disk PVC per sandbox.
- StorageClass uses Standard SSD and `WaitForFirstConsumer`.
- The DevSandbox operator creates and accounts for the PVC; the upstream
  Sandbox references it as an existing claim.
- Upstream Agent Sandbox `volumeClaimTemplates` are not used.
- `/workspace/repo` contains the checkout.
- `/workspace/.devsandbox` contains only non-secret platform metadata and
  persisted VS Code state.
- `/home` and the root filesystem are ephemeral.
- Tokens, sockets, and runtime state use a memory-backed volume.
- Deleting the top-level DevSandbox removes both the PVC object and the managed
  disk before finalization completes.

### 8.9 Network model

The cluster and VNet are dedicated to untrusted developer workloads.

Ingress:

- The public Gateway routes only to the API and sandbox web gateway Services.
- The Gateway proxy accepts traffic only on its HTTPS listener and required
  Azure Load Balancer health-probe ports.
- The Gateway proxy, API, web gateway, broker, operator, Agent Sandbox
  controller, and Sandbox Router run in trusted namespaces on system nodes.
- The API and web gateway may reach Sandbox Router, and the web gateway may
  reach only the API's internal exchange Service/port. Sandbox Router is the
  only trusted service allowed to initiate traffic to sandbox Services.
- Sandbox Pods and Services are never externally exposed.
- Workload Pods cannot reach the API, web gateway, operator, or other
  management ClusterIP Services. They may reach only the broker's dedicated
  Service port in the management namespace. The private Kubernetes API remains
  protected by zero sandbox RBAC rather than an MVP private-destination egress
  deny. Public API/web origins remain reachable as internet endpoints but grant
  no access without their normal user authentication.

Egress:

- The MVP intentionally permits unrestricted outbound traffic.
- This overrides Agent Sandbox's secure-default public-only egress behavior.
- The implementation must preserve router-only sandbox ingress and management
  namespace segmentation even when internet egress is unrestricted.

This choice allows source code and injected user tokens to be exfiltrated to the
internet. It is accepted only for the PoC/MVP and must be recorded as a
production blocker.

## 9. Custom Resource Model

The exact OpenAPI schema may evolve during implementation, but the following
fields are required.

### 9.1 `DevSandboxTemplate`

Recommended scope: cluster scoped.

```yaml
spec:
  displayName: string
  description: string
  version: string
  image:
    repository: string
    digest: string
  defaultProfile: small|medium|large
  entryAction: shell|vscode|copilot
  servicePorts: []
  sandboxBlueprint: {}
  capabilities:
    vscode: bool
    copilot: bool
    playwright: bool
```

Published versions are immutable. Updating a template requires a new version.
`sandboxBlueprint` must not define `volumeClaimTemplates`; workspace storage is
owned by the DevSandbox operator.

### 9.2 `DevSandbox`

Recommended scope: namespaced in the shared workload namespace.

```yaml
spec:
  owner:
    githubUserId: string
    githubLogin: string
  source:
    type: git|empty
    repositoryId: string
    repositoryUrl: string
    refName: string
    commitSha: string
    lfs: bool
    submodules: bool
  template:
    name: string
    version: string
    imageDigest: string
  profile:
    name: small|medium|large
    cpu: string
    memory: string
    storage: string
  lifecycle:
    idleTimeoutSeconds: integer
    stoppedRetentionSeconds: 604800
    failedRetentionSeconds: 86400
  desiredState: Running|Stopped
status:
  phase: string
  upstreamSandboxName: string
  initializedCommitSha: string
  lastActivityTime: date-time
  idleDeadline: date-time
  stoppedAt: date-time
  retentionDeadline: date-time
  conditions: []
```

No credential or secret value is permitted in this resource.

### 9.3 `DevSandboxUserQuota`

Recommended scope: namespaced in the shared workload namespace, with one object
per immutable GitHub user ID.

```yaml
spec:
  githubUserId: string
  limits:
    active: 3
    retained: 10
    storageGiB: 200
status:
  reservedActive: integer
  retained: integer
  provisionedStorageGiB: integer
  reservations: []
```

The resource is internal-only. Mutations use optimistic concurrency and are
reconciled against actual DevSandbox/PVC state to repair drift.

### 9.4 Activity Lease

Use one Kubernetes `coordination.k8s.io/Lease` per active sandbox. The holder
identity identifies the gateway, foreground operation, or managed-job
aggregator, not an untrusted arbitrary process.

### 9.5 Managed jobs

The implementation may use a `DevSandboxJob` CRD or persist live job state in
the supervisor plus summary status in DevSandbox. The chosen design must:

- provide stable job IDs;
- survive management API restarts;
- report running/completed/failed/stopped;
- avoid product-level storage of full command text and output;
- mark jobs terminated when the sandbox stops.

## 10. API Surface

The implementation must publish an OpenAPI document for the REST endpoints.
Exact paths may change before the first implementation task completes, but the
minimum operations are:

```text
POST   /v1/auth/device/start
POST   /v1/auth/device/poll
GET    /v1/me

GET    /v1/templates
GET    /v1/templates/{name}

POST   /v1/sandboxes
GET    /v1/sandboxes
GET    /v1/sandboxes/{name}
POST   /v1/sandboxes/{name}/stop
POST   /v1/sandboxes/{name}/resume
DELETE /v1/sandboxes/{name}

GET    /v1/sandboxes/{name}/events
POST   /v1/sandboxes/{name}/exec
GET    /v1/sandboxes/{name}/jobs
DELETE /v1/sandboxes/{name}/jobs/{jobId}
POST   /v1/sandboxes/{name}/vscode-url

WS     /v1/sandboxes/{name}/shell
WS     /v1/sandboxes/{name}/exec/stream
WS     /v1/sandboxes/{name}/ports/{port}
```

Every sandbox endpoint performs owner authorization before accessing the
corresponding DevSandbox resource or opening a Sandbox Router connection. API
error bodies include a stable code, user-safe message, and optional details.

## 11. Technical Implementation Assumptions

### 11.1 Primary implementation language

Use one Go module for:

- CLI;
- management API;
- sandbox web gateway;
- GitHub credential broker;
- operator;
- sandbox init;
- sandbox supervisor/agent.

Use:

- Cobra for CLI command structure;
- `controller-runtime` and `client-go` for Kubernetes;
- standard `net/http` with a small router;
- a maintained WebSocket library;
- `log/slog` structured logging;
- OpenTelemetry-compatible metrics/traces where practical;
- Go's standard testing package for focused tests.

Pin the current stable Go toolchain in `go.mod`, CI, and container builds.

### 11.2 Proposed repository layout

```text
/
  cmd/
    devsandbox/
    devsandbox-api/
    devsandbox-web/
    devsandbox-broker/
    devsandbox-operator/
    devsandbox-init/
    devsandbox-agent/
  api/
    v1alpha1/
  internal/
    api/
    auth/
    broker/
    cli/
    gateway/
    github/
    lifecycle/
    operator/
    repository/
    supervisor/
    templates/
  infra/
    main.bicep
    foundation.bicep
    bicepconfig.json
    modules/
      network.bicep
      observability.bicep
      acr.bicep
      key-vault.bicep
      dns.bicep
      identities.bicep
      aks.bicep
    environments/
      poc.bicepparam
  deploy/
    kustomize/
      base/
      overlays/poc/
  images/
    standard/
    vscode/
    copilot/
  tests/
    integration/
    e2e/
    ui/
      specs/
  scripts/
    validate.ps1
    deploy.ps1
    e2e.ps1
  .github/
    workflows/
      ci.yml
      deploy-poc.yml
  DEVSANDBOX_SPEC.md
```

### 11.3 Bicep organization

- `foundation.bicep` deploys the complete Azure foundation: network, Log
  Analytics, ACR, Key Vault, Azure DNS, identities, role assignments, private
  AKS, Kata pool, and required AKS add-ons.
- Container images are then built and pushed to ACR.
- `scripts/deploy.ps1` patches pinned image digests into the Kustomize overlay
  and deploys the AKS-hosted services through `az aks command invoke`.
- `main.bicep` composes the Azure modules for what-if and deployment; it does
  not deploy Kubernetes application workloads through ARM.
- Use Bicep modules, explicit parameters, typed outputs, deterministic names
  based on a short prefix and `uniqueString`, and resource tags.
- Do not place secrets in Bicep parameters, outputs, deployment history, or
  `.bicepparam` files.
- Use Workload Identity, user-assigned managed identities, federated identity
  credentials, and Key Vault references.
- The PoC parameter file must contain only non-secret defaults.
- Default network parameters are a `/16` VNet and `/20` AKS subnet.
- The default Kata pool is `Standard_D16s_v5`, `maxPods: 8`, autoscaler minimum
  one, and autoscaler maximum ten.
- Use `Microsoft.ContainerService/managedClusters@2026-03-01` or a newer stable
  API and set managed Gateway API installation to `Standard`, Application
  Routing enabled, and App Routing Istio mode to `Enabled`.
- Enable the Azure Key Vault provider for Secrets Store CSI Driver.
- Create an Application Routing DNS/TLS identity with `DNS Zone Contributor`
  on the selected Azure DNS zone and `Key Vault Secrets User` on the vault.
- Create separate federated identities for the API and credential broker
  ServiceAccounts. The API receives only the Key Vault secret and signing-key
  data actions it requires; the broker receives only the secret data actions
  required to read and rotate GitHub App credentials.
- Management application Deployments, Services, and routing resources belong
  to the Kubernetes/Kustomize layer rather than the Azure ARM deployment.

### 11.4 Kubernetes deployment

Use Kustomize for DevSandbox-owned Kubernetes resources. Pin the Agent Sandbox
release. Build Sandbox Router from the same pinned upstream source commit into
the project ACR and reference it by digest; do not use a mutable third-party
router tag. The deployment must create:

- trusted system namespaces;
- `devsandbox-system` as the default product system namespace;
- `devsandbox-workloads` as the default shared workload namespace;
- CRDs;
- dedicated API, web gateway, broker, operator, and ingress DNS/TLS
  ServiceAccounts;
- scoped API, broker, and operator RBAC, with no RBAC for the web gateway;
- API, web gateway, broker, and operator Deployments and ClusterIP Services,
  including a separate non-public API exchange Service/port;
- Sandbox Router;
- an `approuting-istio` public Gateway with separate HTTPS listeners;
- separate API and sandbox-web `HTTPRoute` resources;
- namespace-scoped Application Routing `ExternalDNS` and Key Vault TLS
  integration;
- NetworkPolicies preserving router-only workload ingress, Gateway-to-service
  ingress, web-to-internal-API exchange, and broker-only workload access to the
  management namespace;
- Standard SSD StorageClass if a suitable built-in class is not used;
- three immutable DevSandboxTemplate resources.
- one zero-RBAC ServiceAccount per DevSandbox, created and removed by the
  operator.

All trusted DevSandbox Deployments and the managed Gateway proxy must use
required node affinity or node selectors for
`kubernetes.azure.com/mode: system`. Sandbox Pods must instead select the Kata
pool and use `runtimeClassName: kata-vm-isolation`.

For the add-on-managed Gateway proxy, use the supported Gateway
`spec.infrastructure.parametersRef` ConfigMap customization to set required
node affinity. Do not patch the generated Deployment directly.

Sandbox Pods must:

- use `runtimeClassName: kata-vm-isolation`;
- run as non-root;
- set `allowPrivilegeEscalation: false`;
- drop Linux capabilities;
- use `seccompProfile: RuntimeDefault`;
- avoid privileged mode, hostPath, host networking, host PID, and host IPC;
- disable automatic service-account token mounting;
- mount only an explicit audience-restricted projected identity when required;
- set workload CPU and memory requests equal to the selected profile limits;
- define ephemeral storage and PID limits;
- reference the operator-created workspace PVC instead of using upstream
  `volumeClaimTemplates`.

### 11.5 Image builds

- Use multi-stage Dockerfiles.
- Pin base images by digest before MVP completion.
- Build the common platform binaries once and copy them into template images.
- Run templates as a non-root user.
- Keep token paths under a tmpfs/runtime directory.
- Include OCI labels for source revision, template version, and build time.
- The Copilot image pins official GitHub Copilot CLI, GitHub CLI,
  `@playwright/cli`, and Chromium versions available at implementation time.
- Build Sandbox Router from the pinned Agent Sandbox source commit and publish
  it to ACR by digest.

### 11.6 Configuration

Runtime configuration uses environment variables or mounted non-secret
configuration. Secrets use Key Vault references or runtime broker calls.
Configuration must include:

- primary GitHub organization;
- GitHub App client/application identifiers;
- API public base URL;
- sandbox web gateway public base URL;
- Key Vault URI;
- Azure DNS zone resource ID;
- unversioned Key Vault TLS certificate URI covering both public hostnames;
- internal credential-broker Service name, port, and projected-token audience;
- internal API exchange Service name and port;
- Sandbox Router Service name;
- route-signing Key Vault key identifier and public verification-key
  distribution;
- shared workload namespace;
- template definitions;
- default/max idle timeout;
- quota values;
- audit workspace settings.

## 12. MVP Scope and Delivery Slices

### Slice A: Infrastructure and one standard sandbox

Prove Bicep deployment, Agent Sandbox, Kata scheduling, workspace storage, and
basic create/delete behavior with no user-facing remote connectivity.

### Slice B: Product lifecycle and CLI

Add the DevSandbox CRDs, controller, management API, login, `up`, `list`,
`status`, `stop`, `resume`, and retention.

### Slice C: Repository and developer connectivity

Add exact repository cloning, Git credentials, shell, exec, managed jobs, and
local port tunnels.

### Slice D: Curated experiences

Add browser VS Code and the Copilot/Playwright template.

### Slice E: Automated validation and pilot readiness

Add CI, Bicep validation, AKS smoke tests, lifecycle tests, UI validation, audit
queries, and a repeatable PoC deployment workflow.

## 13. Work Breakdown for Copilot Autopilot

### Execution rules

The implementation agent must:

1. Execute tasks in dependency order.
2. Read this specification before each task and avoid unrelated scope.
3. Add only focused tests needed to validate core behavior.
4. Run every validation command listed for the task.
5. Fix validation failures before starting a dependent task.
6. Never claim cloud or UI validation if credentials, deployment, or the
   browser session were unavailable.
7. Never print or persist GitHub tokens, Key Vault values, kubeconfigs, or
   one-time access URLs in committed artifacts.
8. Invoke the `playwright-cli` skill for UI exploration, test generation,
   debugging, and validation. Use `.agents/skills/playwright-cli/SKILL.md` as
   the repository-local fallback path after MVP-00 installs it.
9. Preserve generated validation artifacts only when they are intentionally
   useful and contain no credentials.

### MVP-00: Bootstrap the repository

Dependencies: none.

Deliverables:

- Go module and initial package structure.
- CLI/API/web-gateway/broker/operator/init/agent entry points that build.
- Shared version package.
- PowerShell validation script with static and build modes.
- Base `.gitignore`, editor settings, and concise developer README.
- GitHub Actions CI skeleton.
- Repository-local `playwright-cli` skill installed under
  `.agents/skills/playwright-cli/` and pinned in `skills-lock.json`.
- Minimal `package.json` and lock file for the focused Playwright UI smoke test,
  pinning `@playwright/test` and `@playwright/cli`.

Automated checks:

```powershell
pwsh ./scripts/validate.ps1 -Mode Static
go vet ./...
go test ./...
go build ./cmd/...
npm ci
npx --no-install playwright-cli --help
```

Completion criteria:

- All binaries compile.
- CI runs on pull requests.
- Validation exits nonzero on any failed check.

### MVP-01: Implement Bicep foundation

Dependencies: MVP-00.

Deliverables:

- Modular Bicep for resource group, VNet/subnets, Log Analytics, ACR, Key Vault,
  Azure DNS, managed identities, federated identity credentials, AKS, Kata
  agent pool, managed Gateway API, Application Routing, Key Vault CSI, and
  required role assignments.
- Workload identities for the API, credential broker, and Application Routing
  DNS/TLS integration.
- PoC `.bicepparam` file with no secrets.
- Outputs needed by image build and deployment scripts.
- Staged deployment script.

Implementation requirements:

- Kata pool uses Azure Linux and `workloadRuntime: KataVmIsolation`.
- Default the Kata pool to `Standard_D16s_v5`, `maxPods: 8`, autoscaler
  minimum one, and maximum ten.
- Default the system pool to `Standard_D4s_v5`.
- Parameter changes to the Kata SKU require rerunning large-profile scheduling
  validation.
- Enable AKS OIDC issuer and workload identity.
- Enable the managed Gateway API standard installation, Application Routing
  operator, sidecar-less App Routing Istio implementation, and Key Vault CSI
  add-on through Bicep.
- Use the `approuting-istio` GatewayClass; do not use managed NGINX for new
  routes.
- Keep the Kata pool autoscaler minimum at one.
- Keep the AKS API private and grant the deployment identity the scoped
  `Microsoft.ContainerService/managedClusters/runcommand/action` and
  `Microsoft.ContainerService/managedClusters/commandResults/read`
  permissions.
- Do not introduce Terraform.

Automated checks:

```powershell
az bicep build --file ./infra/foundation.bicep
az bicep build --file ./infra/main.bicep
az deployment sub what-if --location <location> --template-file ./infra/foundation.bicep --parameters ./infra/environments/poc.bicepparam
```

The `what-if` check is required when an Azure identity is available. Static
Bicep builds are always required.

Completion criteria:

- Bicep compiles without errors.
- No secret values appear in deployment parameters or outputs.
- What-if contains only expected PoC resources.

### MVP-02: Bootstrap AKS, Agent Sandbox, and Kata validation

Dependencies: MVP-01.

Deliverables:

- Kustomize base/PoC overlay.
- Pinned Agent Sandbox controller installation.
- Sandbox Router built from the pinned Agent Sandbox source commit, pushed to
  ACR, and referenced by digest.
- Trusted system and shared workload namespaces with default-deny
  NetworkPolicies.
- Public API, internal API exchange, sandbox-web, and broker Service
  definitions.
- Application Routing DNS/TLS ServiceAccount.
- Public `approuting-istio` Gateway with separate API and sandbox-web HTTPS
  listeners.
- Separate API and sandbox-web HTTPRoutes plus namespace-scoped
  `ExternalDNS`.
- Management policies that permit Gateway-to-API/web, API/web-to-Router, and
  web-to-internal-API exchange, and workload-to-broker traffic while denying
  workload access to other management Services.
- A Gateway-proxy policy that permits only the public HTTPS listener and Azure
  Load Balancer health probes from outside the namespace.
- RuntimeClass verification script.
- Minimal upstream Sandbox manifest for Kata smoke testing.

Automated checks:

```powershell
kubectl kustomize ./deploy/kustomize/overlays/poc | Set-Content ./artifacts/poc.yaml
az aks command invoke --resource-group <rg> --name <aks> --file ./artifacts/poc.yaml --command "kubectl apply -f poc.yaml"
az aks command invoke --resource-group <rg> --name <aks> --command "kubectl get runtimeclass kata-vm-isolation"
az aks command invoke --resource-group <rg> --name <aks> --command "kubectl get gatewayclass approuting-istio && kubectl wait --for=condition=Programmed gateway/devsandbox -n devsandbox-system --timeout=5m"
az aks command invoke --resource-group <rg> --name <aks> --command "kubectl get gateway,httproute,externaldns -n devsandbox-system"
az aks command invoke --resource-group <rg> --name <aks> --file <kata-smoke-manifest> --command "kubectl apply -f <uploaded-name> && kubectl wait --for=condition=Ready sandbox/<name> --timeout=5m"
az aks command invoke --resource-group <rg> --name <aks> --command "kubectl exec <sandbox-pod> -- uname -r && kubectl get node <sandbox-node> -o jsonpath='{.status.nodeInfo.kernelVersion}'"
```

Completion criteria:

- Agent Sandbox reconciles a Sandbox resource.
- The `approuting-istio` Gateway is programmed with two HTTPS listeners.
- The API and sandbox-web HTTPRoutes attach to the expected listeners and use
  different hostnames.
- The generated Gateway proxy schedules on the system pool.
- The Pod uses `kata-vm-isolation`.
- Guest and node kernel versions differ.
- A sandbox rendered with the large profile schedules successfully on the
  default Kata SKU.
- The smoke resource can be deleted cleanly.

### MVP-03: Define CRDs and API contracts

Dependencies: MVP-00.

Deliverables:

- `DevSandboxTemplate`, `DevSandbox`, and internal `DevSandboxUserQuota` Go API
  types.
- Managed-job representation decision and implementation.
- Generated CRDs and deepcopy code.
- OpenAPI document for the public REST API.
- Validation/defaulting for immutable template versions, profiles, idle timeout,
  source types, and secret-free specs.

Focused tests:

- invalid idle timeout;
- immutable template fields;
- invalid profile;
- concurrent quota reservations;
- secret-like fields rejected from CR specs;
- expected JSON/OpenAPI serialization.

Automated checks:

```powershell
go test ./api/... ./internal/...
go vet ./...
go build ./cmd/...
az aks command invoke --resource-group <rg> --name <aks> --file <generated-crds> --command "kubectl apply --dry-run=server -f <uploaded-name>"
```

Completion criteria:

- CRDs are structural and accepted by the Kubernetes API.
- Public types contain no credential fields.
- OpenAPI describes every MVP API operation.

### MVP-04: Implement the DevSandbox operator

Dependencies: MVP-02, MVP-03.

Deliverables:

- Create and own the workspace PVC, then reconcile DevSandbox into an upstream
  Sandbox that references the existing claim.
- Render pinned template image digest, Kata RuntimeClass, security context, and
  selected profile.
- State and condition projection.
- Stop/resume reconciliation through `operatingMode`.
- Activity Lease processing.
- Upstream `shutdownTime` fail-safe.
- Seven-day stopped and 24-hour failed retention.
- Atomic per-user active, retained-count, and provisioned-storage reservations
  through `DevSandboxUserQuota`.
- Finalizers and deletion cascade.

Focused tests:

- create reconciliation;
- stop and resume;
- idle deadline calculation;
- active managed job suppresses idle stop;
- stale connection does not suppress idle stop;
- stopped/failed retention;
- quota rejection;
- concurrent create requests cannot exceed quota;
- PVC and managed disk cleanup after delete;
- idempotent deletion.

Automated checks:

```powershell
go test ./internal/operator/... ./internal/lifecycle/...
go vet ./...
go build ./cmd/devsandbox-operator
```

Completion criteria:

- Core lifecycle reconciliation works in unit and envtest coverage without
  manual Pod operations.
- Reconciliation is idempotent.
- Quotas cannot be bypassed by concurrent creates.
- Upstream `volumeClaimTemplates` are not used.

### MVP-05: Implement sandbox init and supervisor

Dependencies: MVP-03.

Deliverables:

- `devsandbox-init` binary.
- `devsandbox-agent` supervisor binary.
- Repository initialization marker.
- Internal authenticated health, shell, exec, job, and activity interfaces.
- Process-tree cleanup on stop.
- Runtime credential-file integration.
- One zero-RBAC ServiceAccount per sandbox and projected broker identity.
- End-to-end WebSocket ping/pong support.

Focused tests:

- first initialization versus resume;
- command exit-code propagation;
- foreground stream cancellation;
- detached-job lifecycle;
- no command/output in audit payloads;
- child process cleanup.

Automated checks:

```powershell
go test ./internal/repository/... ./internal/supervisor/...
go build ./cmd/devsandbox-init ./cmd/devsandbox-agent
```

Completion criteria:

- The supervisor runs as non-root.
- Resume does not overwrite `/workspace/repo`.
- Managed jobs have stable IDs and status.

### MVP-06: Build curated template images

Dependencies: MVP-01, MVP-04, MVP-05.

Deliverables:

- Standard image.
- VS Code/code-server image.
- Copilot image with `gh`, standalone `copilot`, `@playwright/cli`, Chromium,
  and required browser dependencies.
- Sandbox Router image built from pinned upstream source.
- Image metadata and pinned versions.
- Three immutable DevSandboxTemplate manifests.

Automated checks:

```powershell
docker build -f ./images/standard/Dockerfile -t devsandbox-standard:test .
docker build -f ./images/vscode/Dockerfile -t devsandbox-vscode:test .
docker build -f ./images/copilot/Dockerfile -t devsandbox-copilot:test .
docker run --rm devsandbox-standard:test git --version
docker run --rm devsandbox-vscode:test code-server --version
docker run --rm devsandbox-copilot:test gh --version
docker run --rm devsandbox-copilot:test copilot --version
docker run --rm devsandbox-copilot:test playwright-cli --help
```

Cloud smoke:

- Push the standard image and apply an immutable test template by digest.
- Create a DevSandbox with a short test idle timeout.
- Observe automatic suspension.
- Resume and verify the same PVC and initialization marker remain.
- Stop again and delete.

Completion criteria:

- Images start as the non-root development user.
- Expected tools are installed and versioned.
- No token, kubeconfig, or build secret exists in image history or layers.
- The operator lifecycle smoke passes with the real standard template image.

### MVP-07: Implement GitHub App auth and credential broker

Dependencies: MVP-01, MVP-03.

Deliverables:

- Device-flow login endpoints.
- Primary-organization membership check at login.
- Key Vault refresh-token storage keyed by immutable GitHub user ID.
- Short-lived platform session token.
- User access token refresh.
- Dedicated internal broker Service endpoint with no public route.
- TokenReview validation of the projected token's exact audience and binding
  to namespace, ServiceAccount UID, Pod UID, and DevSandbox UID.
- Separate API and broker Workload Identity clients for Key Vault.
- Redaction middleware and structured audit events.

Focused tests:

- device-flow state handling;
- membership rejection;
- owner mismatch;
- sandbox A projected identity cannot obtain sandbox B's credential;
- concurrent API/broker refresh requests serialize through one per-user Lease;
- refresh re-reads the latest Key Vault version and safely handles a rotated
  token or `invalid_grant`;
- expired platform token;
- Key Vault and GitHub API error propagation;
- token redaction.

Automated checks:

```powershell
go test ./internal/auth/... ./internal/broker/... ./internal/github/...
go vet ./...
go build ./cmd/devsandbox-api ./cmd/devsandbox-broker
```

Completion criteria:

- Tokens never appear in logs or Kubernetes resources.
- The broker cannot mutate DevSandbox resources and does not trust
  caller-supplied owner identifiers.
- Unit and integration tests cover refresh-token rotation, TokenReview
  validation, owner binding, and redaction without requiring a deployed API.

### MVP-08: Implement AKS-hosted management services

Dependencies: MVP-02, MVP-03, MVP-04, MVP-07.

Deliverables:

- Management API, sandbox web gateway, and credential broker Deployments on the
  AKS system pool.
- Public REST API through the API HTTPRoute.
- Separate sandbox web gateway Service, HTTPRoute, and hostname.
- Owner authorization middleware.
- Direct, scoped Kubernetes client and RBAC for the management API.
- No Kubernetes API token or RBAC for the sandbox web gateway.
- Scoped broker TokenReview/read RBAC and separate Workload Identity.
- Lease-only write RBAC for broker credential-refresh locks.
- A non-public API exchange Service/port reachable only by the sandbox web
  gateway.
- List/watch/status operations.
- Direct API/web-gateway connections to Sandbox Router.
- WebSocket proxy foundation through the public Gateway.
- One-time VS Code URL issuance and exchange.
- Workload Identity wiring for Key Vault access.
- Readiness/liveness probes, PodDisruptionBudgets, and at least two API/web
  replicas where the PoC budget permits.

Focused tests:

- owner can access their sandbox;
- another user receives forbidden/not-found behavior;
- invalid state transitions;
- API Kubernetes access is limited to the documented domain resources;
- web gateway has no Kubernetes API credentials or permissions;
- broker cannot mutate sandboxes or read unrelated Kubernetes Secrets;
- one-time URL cannot be reused;
- one-time credentials never appear in a URL path, query string, Referer,
  Gateway access log, or browser history after exchange;
- the bootstrap page is no-store, has a restrictive Content Security Policy,
  and has no third-party resources;
- sandbox web cookies are host-only and are not sent to the management API;
- route claims cannot be altered or used for a different sandbox;
- one-time URL consumption remains atomic across API and web-gateway replicas;
- WebSocket disconnect cleanup.

Automated checks:

```powershell
go test ./internal/gateway/... ./internal/api/... ./internal/broker/...
go vet ./...
go build ./cmd/devsandbox-api ./cmd/devsandbox-web ./cmd/devsandbox-broker
docker build -f <api-dockerfile> .
az aks command invoke --resource-group <rg> --name <aks> --command "kubectl rollout status deployment/devsandbox-api deployment/devsandbox-web deployment/devsandbox-broker -n devsandbox-system --timeout=5m"
```

Cloud smoke:

- Login with a test organization user.
- Verify a refresh credential exists in Key Vault without printing its value.
- Request a sandbox-scoped runtime token using the projected identity.
- Confirm invalid audience and owner claims are rejected.
- Confirm a projected identity from sandbox A cannot obtain sandbox B's
  credential.

Completion criteria:

- Public clients cannot reach Kubernetes directly.
- The API can create, list, watch, and mutate only the required DevSandbox
  domain resources through its Kubernetes ServiceAccount.
- WebSocket upgrades work through the AKS Application Routing Gateway API.
- API, web gateway, broker, operator, Sandbox Router, and Gateway proxy Pods
  run on the system pool.
- The workload namespace cannot reach the API or web gateway ClusterIP
  Services, but an authenticated sandbox projected identity can reach the
  broker.
- The public Gateway cannot route to the internal API exchange Service/port.
- The API and broker can access their required Key Vault data through separate
  Workload Identities; the web gateway has no Key Vault identity.

### MVP-09: Implement core CLI

Dependencies: MVP-08.

Deliverables:

- `login`.
- `templates` and `templates show`.
- `up`, `list`, `status`, `stop`, `resume`, and `delete`.
- TTY detection and prompts.
- Current-repository target resolution.
- `--json` output.
- OS credential-store integration.

Focused tests:

- missing template prompt versus noninteractive failure;
- ambiguous sandbox targeting;
- generated names;
- JSON output stability;
- exit-code mapping;
- idempotent stop/delete behavior.

Automated checks:

```powershell
go test ./internal/cli/...
go build ./cmd/devsandbox
go run ./cmd/devsandbox --help
go run ./cmd/devsandbox templates --help
```

Completion criteria:

- Every required core command works against a local/mock API and the deployed
  PoC API.
- Automation never hangs waiting for a prompt.

### MVP-10: Implement exact repository provisioning

Dependencies: MVP-05, MVP-07, MVP-09.

Deliverables:

- Local Git repository inspection.
- Clean and pushed HEAD validation.
- `origin` or TTY remote selection.
- Explicit repository/default-branch resolution.
- Partial clone, exact checkout, branch attachment, LFS, and submodules.
- Git author configuration.
- Git credential helper and `gh` wrapper.
- Push and pull-request creation for a dedicated test branch.

Focused tests:

- dirty worktree rejected;
- unpushed commit rejected;
- exact SHA retained when remote branch advances;
- unsupported private external repo rejected;
- public external repo cloned read-only;
- invalid submodule URL rejected;
- resume does not reclone.

Automated checks:

```powershell
go test ./internal/repository/...
pwsh ./scripts/e2e.ps1 -Scenario RepositoryClone
```

Completion criteria:

- `/workspace/repo` HEAD equals the commit recorded at creation.
- Private primary-organization clone, LFS, and same-organization submodule
  scenarios succeed.
- The test user can push a temporary branch and create then close a pull request
  through the sandbox identity.
- Token values are absent from `.git/config`, process arguments, and workspace
  files.

### MVP-11: Implement shell, exec, jobs, and port tunnels

Dependencies: MVP-05, MVP-08, MVP-09.

Deliverables:

- Interactive shell.
- Foreground exec with exit-code propagation.
- Detached managed jobs and job stop.
- Local TCP tunnel.
- Activity heartbeat integration.
- Application-level WebSocket ping/pong no slower than every 60 seconds.
- Connection termination when stopped/deleted.

Focused tests:

- binary and text stream handling;
- terminal resize;
- Ctrl+C/cancellation;
- remote exit codes;
- detached job survives CLI disconnect;
- arbitrary background process is not treated as managed;
- active job suppresses idle stop;
- tunnel closes when sandbox stops.
- a silent shell remains connected for at least ten minutes;
- the local TCP tunnel carries non-HTTP bytes through the WebSocket/agent path.

Automated checks:

```powershell
go test ./internal/gateway/... ./internal/supervisor/... ./internal/cli/...
pwsh ./scripts/e2e.ps1 -Scenario Connectivity
pwsh ./scripts/e2e.ps1 -Scenario ManagedJobIdle
```

Completion criteria:

- The user can complete a normal shell and command workflow without Kubernetes
  tooling.
- Idle behavior matches the functional specification.

### MVP-12: Implement VS Code browser experience

Dependencies: MVP-06, MVP-08, MVP-11.

Deliverables:

- code-server startup and readiness.
- Authenticated one-time browser URL.
- Gateway routing through Sandbox Router.
- Persistent settings/extensions directory.
- Template-default browser launch from `devsandbox up`.
- One focused Playwright UI smoke test.

Automated checks:

```powershell
pwsh ./scripts/e2e.ps1 -Scenario VSCode
$env:PLAYWRIGHT_HTML_OPEN='never'
npx playwright test ./tests/ui/vscode-smoke.spec.ts
```

UI validation must follow Section 14.6 and the repository's `playwright-cli`
skill.

Completion criteria:

- An owner can open code-server without seeing or setting routing headers.
- The cloned repository appears in the Explorer.
- An integrated terminal starts in `/workspace/repo`.
- Reusing or sharing the one-time URL fails.

### MVP-13: Implement Copilot template experience

Dependencies: MVP-06, MVP-07, MVP-09, MVP-11.

Deliverables:

- Template-default `copilot` launch.
- Runtime injection of `COPILOT_GITHUB_TOKEN`.
- `gh` authentication.
- Playwright CLI and Chromium readiness.
- Clear error when the user has no Copilot entitlement.

Automated checks:

```powershell
pwsh ./scripts/e2e.ps1 -Scenario CopilotTemplate
```

The scenario verifies:

- `git`, `gh`, `copilot`, and `playwright-cli` versions;
- `gh` identifies the expected user;
- Copilot authentication succeeds without interactive login;
- Chromium can launch a simple page;
- no credential remains on the workspace PVC after stop.

Completion criteria:

- A logged-in platform user can start the Copilot template without a second
  Copilot login.

### MVP-14: Add observability and operational behavior

Dependencies: MVP-04, MVP-08.

Deliverables:

- Structured audit events.
- Metrics listed in Section 6.9.
- Health/readiness endpoints.
- Azure Monitor queries or workbook snippets for PoC operation.
- Kubernetes operator runbook explaining top-level CR deletion.
- No product administrator API.

Automated checks:

```powershell
go test ./internal/... 
pwsh ./scripts/e2e.ps1 -Scenario Audit
```

Completion criteria:

- Expected lifecycle events can be queried in Log Analytics.
- Tokens, command output, and file content are absent from events.
- Health probes detect Kubernetes API, Sandbox Router, broker, Key Vault, and
  Gateway dependency failures.

### MVP-15: Complete CI/CD and end-to-end validation

Dependencies: MVP-01 through MVP-14.

Deliverables:

- Pull-request CI workflow.
- Manual `workflow_dispatch` PoC deployment workflow using GitHub OIDC for
  Azure authentication.
- Image build/push by digest.
- Bicep what-if and deployment.
- Kubernetes apply and rollout checks through `az aks command invoke`.
- Gateway, HTTPRoute, DNS, TLS, Workload Identity, and NetworkPolicy checks.
- End-to-end smoke suite.
- Playwright UI smoke suite.
- Explicit cleanup commands for test sandboxes.
- Deployment preflight for Azure role assignments, regional vCPU quota,
  required fixture repositories, test identities, and Playwright dependencies.

Required CI checks:

```powershell
pwsh ./scripts/validate.ps1 -Mode Static
pwsh ./scripts/validate.ps1 -Mode Build
```

Required deployed checks:

```powershell
pwsh ./scripts/e2e.ps1 -Scenario All
$env:PLAYWRIGHT_HTML_OPEN='never'
npx playwright test ./tests/ui/vscode-smoke.spec.ts
```

Completion criteria:

- Static/build CI is green.
- Bicep deploys an empty environment from scratch.
- Standard, VS Code, and Copilot scenarios pass on Kata.
- Idle stop, resume, workspace persistence, quotas, and retention are
  demonstrated.
- No failed validation is hidden or converted into a success-shaped fallback.

## 14. Validation Strategy

### 14.1 Validation prerequisites

Cloud and end-to-end validation scripts must run a preflight and report the
names of missing inputs without printing secret values. Required inputs are:

```text
AZURE_SUBSCRIPTION_ID
AZURE_LOCATION
DEVSANDBOX_RESOURCE_PREFIX
DEVSANDBOX_TEST_ORG
DEVSANDBOX_TEST_PRIVATE_REPO
DEVSANDBOX_TEST_PUBLIC_REPO
DEVSANDBOX_TEST_USER
DEVSANDBOX_TEST_USER_SESSION
DEVSANDBOX_TEST_SECOND_USER_SESSION
DEVSANDBOX_DNS_ZONE_RESOURCE_ID
DEVSANDBOX_API_HOSTNAME
DEVSANDBOX_WEB_HOSTNAME
DEVSANDBOX_TLS_CERTIFICATE_URI
```

The private fixture repository must contain:

- at least one Git LFS object;
- a private submodule in the primary organization;
- a branch on which the test user can push;
- permission for the test user to create and close a pull request.

The deployment identity must have:

- permission to deploy the Bicep resources and role assignments;
- scoped AKS `managedClusters/runcommand/action`;
- scoped AKS `managedClusters/commandResults/read`;
- permission to push images to ACR;
- permission to create federated identity credentials;
- permission to assign `DNS Zone Contributor` and the required Key Vault data
  roles to the workload identities;
- sufficient regional vCPU quota for the configured system and Kata pools.

The API and sandbox-web hostnames must be in the selected Azure DNS zone. The
unversioned Key Vault certificate URI must identify a CA-signed certificate
whose subject alternative names cover both hostnames. Certificate material is
not passed through Bicep parameters or committed files.

Missing prerequisites make the affected cloud scenario `Blocked`, not
`Passed`. Static validation must still run.

All automated `e2e.ps1` scenarios consume
`DEVSANDBOX_TEST_USER_SESSION`, a pre-issued platform session stored as a CI
secret and never printed. The GitHub device-flow login itself is validated once
with an explicitly manual `-Scenario Login` run; that run is also the supported
way to mint or refresh the primary automated test session.

### 14.2 Validation levels

#### Static

Must run without Azure credentials:

- `gofmt` check;
- `go vet ./...`;
- focused `go test ./...`;
- `go build ./cmd/...`;
- Bicep build/lint;
- Kustomize render;
- OpenAPI generation consistency;
- container Dockerfile syntax/build where Docker is available.

#### Cloud deployment

Requires an Azure identity:

- Bicep what-if;
- Bicep staged deployment;
- ACR push;
- Kubernetes installation;
- Application Routing Gateway API, DNS, and TLS;
- AKS-hosted management-service rollout and system-pool scheduling;
- Workload Identity and management/workload NetworkPolicy behavior;
- RuntimeClass and Kata isolation;
- management API and sandbox-web health and WebSocket upgrade.

#### Product end-to-end

Requires the deployed PoC and the pre-issued test-user session:

1. Validate the stored session with `GET /v1/me`.
2. List templates.
3. Create an empty standard sandbox.
4. Run a command and create a workspace marker.
5. List and inspect the sandbox.
6. Stop and resume it.
7. Verify the workspace marker remains.
8. Create from a test repository and verify exact commit, LFS, and submodules.
9. Test shell, foreground exec, detached job, and local port tunnel.
10. Test a short idle timeout instead of waiting two hours.
11. Validate active and retained quota rejection.
12. Validate VS Code through Playwright.
13. Validate the Copilot template.
14. Delete all test sandboxes.

### 14.3 Focused unit tests

Do not build an extensive test suite for the PoC. Unit tests are required only
for logic that is difficult or costly to validate manually:

- lifecycle state transitions;
- idle and retention time calculations;
- concurrent quota decisions;
- repository clean/pushed validation;
- owner authorization;
- token redaction;
- one-time URL consumption;
- one-time browser credential transport and access-log redaction;
- template validation.

### 14.4 Infrastructure validation

The agent must always run `az bicep build`. When Azure credentials and a target
subscription are available, it must also run `what-if` before deployment.
Because the AKS API is private, every cloud-side `kubectl` operation must run
through `az aks command invoke`.

After deployment, verify:

```text
AKS is private and healthy.
System and Kata pools are Ready.
Kata pool workloadRuntime is KataVmIsolation.
Default Kata pool SKU is Standard_D16s_v5 with maxPods 8.
kata-vm-isolation RuntimeClass exists.
A large-profile sandbox schedules successfully.
OIDC issuer and workload identity are enabled.
Managed Gateway API standard CRDs are installed.
Application Routing and the `approuting-istio` GatewayClass are Ready.
The public Gateway is Programmed with separate HTTPS listeners.
API and sandbox-web HTTPRoutes are attached to different hostnames.
Azure DNS resolves both hostnames to the Gateway address.
Both hostnames present the expected Key Vault-backed TLS certificate.
Agent Sandbox controller is Ready.
Sandbox Router is Ready.
API, web gateway, broker, operator, Sandbox Router, and Gateway proxy Pods run
on the system pool, not the Kata pool.
Management API is healthy over HTTPS.
Sandbox web gateway is healthy on a different origin.
Management API and web gateway can reach Sandbox Router.
The API can perform only its expected DevSandbox/Lease Kubernetes operations.
The web gateway has no Kubernetes API token or effective RBAC permissions.
The API and broker access Key Vault through separate Workload Identities.
The workload namespace cannot reach API, web, or operator ClusterIP Services
and can reach only the authenticated broker port in the management namespace.
Key Vault uses RBAC and contains no Bicep-supplied secret values.
```

### 14.5 Security validation

Automated checks must inspect rendered Kubernetes resources and deployed Pods
for:

- `runtimeClassName: kata-vm-isolation`;
- CPU and memory requests equal the selected profile limits;
- non-root execution;
- no privileged containers;
- no hostPath/host network/host PID/host IPC;
- no automatic service-account token;
- a unique zero-RBAC ServiceAccount per DevSandbox;
- a projected token with the broker-specific audience only;
- dropped capabilities;
- `RuntimeDefault` seccomp;
- owner-only gateway authorization;
- management API and sandbox web content use different origins and cookie
  scopes;
- API Kubernetes RBAC excludes generic resource, Secret, and node access;
- web gateway automatic token mounting is disabled and it has no effective
  Kubernetes RBAC;
- broker identity resolution is bound to TokenReview-authenticated namespace,
  ServiceAccount UID, Pod UID, and DevSandbox UID;
- broker has no DevSandbox mutation or unrelated Secret access;
- broker write access is limited to its credential-refresh Leases;
- management namespace ingress is denied from workloads except for the broker
  Service port;
- the Gateway proxy exposes only the expected listener and health-probe ports;
- one-time credentials are carried in URL fragments and POST bodies only, then
  removed from browser history before code-server loads;
- Gateway access logs, request URLs, and Referer headers contain no one-time
  credential;
- sandbox A cannot obtain sandbox B's GitHub credential;
- no token value in CRDs, Pod specs, logs, workspace, or command arguments.

Because unrestricted egress and raw token injection are explicit MVP choices,
tests should document them rather than falsely claim least-privilege network or
credential isolation.

### 14.6 UI validation with `playwright-cli`

The only MVP browser UI is browser-based code-server and the sandbox web
gateway's access/error pages. A custom dashboard is out of scope.

Before UI work, the implementation agent must invoke the `playwright-cli`
skill. The repository-local fallback instructions are:

```text
.agents/skills/playwright-cli/SKILL.md
```

The agent must use the skill's plan -> generate -> heal workflow:

1. Create `tests/ui/specs/vscode-smoke.plan.md`.
2. Create a seed test that navigates to the one-time URL supplied through an
   environment variable. Never commit the URL.
3. Start the seed with `--debug=cli`.
4. Attach with `playwright-cli`.
5. Explore using snapshots and semantic element references.
6. Generate one focused `tests/ui/vscode-smoke.spec.ts`.
7. Add explicit assertions.
8. Run the committed test without debug mode.
9. If it fails, attach with `playwright-cli`, inspect snapshots, console, and
   requests, then heal the test or implementation.

Required UI scenario:

```text
Given an owner-authenticated one-time VS Code URL
When the browser opens the URL
Then code-server loads successfully
And the Explorer shows the cloned repository
And an integrated terminal can be opened
And the terminal starts in /workspace/repo
And a reused one-time URL is rejected
```

Use Chromium for the MVP. Prefer role, label, and test-ID locators. Do not use
arbitrary sleeps or `networkidle`. Configure traces and screenshots on failure.
Set `PLAYWRIGHT_HTML_OPEN=never` for automated runs.

### 14.7 Validation reporting rules

- A failed command blocks completion of the task it validates.
- An unavailable cloud dependency is reported as blocked, not passed.
- Do not replace an unavailable real AKS/Kata check with a local container and
  claim equivalence.
- Redact one-time URLs and all credential material from logs.
- Keep command output in CI logs; do not create large committed validation
  transcripts.

## 15. MVP Acceptance Criteria

The MVP is complete when all of the following are demonstrated in the deployed
PoC:

1. Bicep creates the Azure environment from an empty resource group/subscription
   scope as designed, and the API, web gateway, broker, operator, Sandbox
   Router, and Gateway proxy run on the AKS system pool.
2. A sandbox Pod runs with `kata-vm-isolation`, and guest versus host kernels
   are demonstrably different.
3. A large-profile sandbox schedules on the default Kata pool.
4. `devsandbox login` authenticates an eligible organization member.
5. `devsandbox up` works for current repository, explicit repository, and empty
   workspace sources.
6. Omitted template selection prompts interactively and fails clearly in
   automation.
7. `devsandbox list` and `--all` return the correct owner-scoped states.
8. Exact clean/pushed repository commits are cloned to `/workspace/repo`.
9. Git push and pull-request creation work with the user identity.
10. Shell, exec, detached jobs, and local port tunnels work without Kubernetes
   credentials.
11. A silent shell remains connected for at least ten minutes through the AKS
    Gateway API/WebSocket path.
12. A sandbox automatically stops after a shortened test idle timeout and the
    production maximum is enforced at two hours.
13. Resume uses the same PVC and pinned template digest.
14. Stopped and failed retention logic is exercised with shortened test values.
15. Per-user active, retained, and storage quotas reject excess requests,
    including concurrent creates.
16. VS Code passes the Playwright smoke scenario on the separate sandbox web
    origin.
17. The Copilot template starts authenticated and launches Chromium through
    Playwright CLI.
18. Tokens are absent from CRDs, Pod specs, logs, images, and workspace storage.
19. CI, Bicep, build, cloud smoke, lifecycle, and UI checks report success.

## 16. Known Risks and Production Blockers

The following are accepted for the PoC but block a production designation:

1. The raw GitHub user token is readable by sandbox code.
2. Sandbox egress is unrestricted.
3. A shared namespace reduces isolation and prevents native per-user
   `ResourceQuota`.
4. Managed jobs may run indefinitely.
5. GitHub organization membership and repository access are not continuously
   revalidated.
6. There is no product administrator or audited break-glass workflow.
7. CRD-only state loses transactional history after deletion.
8. Kata workloads are not assessed by Microsoft Defender for Containers.
9. There is no multi-region disaster recovery.
10. Private package feeds and application secrets are unsupported.
11. The management plane and untrusted workloads share an AKS cluster failure
    domain; Kata, scoped RBAC, node-pool separation, and NetworkPolicies reduce
    but do not eliminate that blast radius.

The first production-hardening priorities are a repository-scoped credential
proxy, mandatory private/metadata destination blocking, audited administration,
continuous access revocation, and a durable product database.

## 17. References

- Initial design discussion: `INITIAL_DISCUSSION.md`
- Kubernetes Agent Sandbox: <https://agent-sandbox.sigs.k8s.io/>
- Agent Sandbox VS Code example:
  <https://agent-sandbox.sigs.k8s.io/docs/use-cases/examples/vscode-sandbox/>
- Agent Sandbox AKS Kata example:
  <https://agent-sandbox.sigs.k8s.io/docs/use-cases/examples/kata-aks-sandbox/>
- AKS Pod Sandboxing:
  <https://learn.microsoft.com/en-us/azure/aks/use-pod-sandboxing>
- AKS Application Routing with Gateway API:
  <https://learn.microsoft.com/en-us/azure/aks/app-routing-gateway-api>
- AKS Application Routing Gateway API DNS and TLS:
  <https://learn.microsoft.com/en-us/azure/aks/app-routing-gateway-api-dns-tls>
- GitHub App user access tokens:
  <https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-a-user-access-token-for-a-github-app>
- GitHub Copilot CLI authentication:
  <https://docs.github.com/en/copilot/how-tos/copilot-cli/set-up-copilot-cli/authenticate-copilot-cli>
