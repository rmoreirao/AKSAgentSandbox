# Copilot Chat Conversation Export: Creating a self-hosted sandbox in AKS

**User:** @rmoreirao
**Thread URL:** https://github.com/copilot/c/87bfb7f1-949b-48c4-9341-282bcaba9590

## @rmoreirao

How can I create a sandbox self-hosted agent sandbox in AKS like the Copilot one? https://docs.github.com/en/copilot/concepts/about-cloud-and-local-sandboxes#starting-a-cloud-sandbox-session

I would like to create a dev platform on the top of AKS - where devs can easily spin up a new sandbox from command line and run agents from it. Users should be able to run their dev workflow also inside of it - for ex.: building JS Node or React Apps, executing playwright to validate e2e tests. It should also directly clone the repo and use the same credentials from the original repo where they execute the command line from

## Copilot

**You can build a Copilot-like sandbox platform on AKS, but you cannot redirect `copilot --cloud` to your AKS cluster.** GitHub’s cloud sandbox is GitHub-hosted and currently built on Azure Container Apps Sandboxes. Instead, run the regular Copilot CLI—or other coding agents—inside isolated AKS workloads managed by your own CLI and control plane. GitHub’s cloud mode is also currently limited to interactive sessions; `--cloud` cannot be combined with programmatic `-p` or `-i` execution. ([docs.github.com](https://docs.github.com/en/copilot/concepts/about-cloud-and-local-sandboxes?utm_source=openai))

## Recommended architecture

```text
Developer CLI
    │
    ├── Detect repo, branch, commit and devcontainer
    ├── Authenticate developer
    └── POST /sandboxes
              │
              ▼
      Sandbox Control Plane
      API + Kubernetes Operator
              │
              ├── Authorization / quotas / TTL
              ├── GitHub credential broker
              ├── Image/template resolver
              └── Creates Sandbox CRD
                         │
                         ▼
                 AKS Sandbox Namespace
                 ├── Kata-isolated workspace pod
                 ├── Per-sandbox PVC
                 ├── NetworkPolicy
                 ├── ResourceQuota
                 └── Terminal/port-forward gateway
```

### 1. Developer CLI

Build a CLI such as `devbox`:

- `devbox up` — detect the current Git repository and create a sandbox.
- `devbox shell` — open an interactive terminal.
- `devbox agent` — start Copilot CLI or another agent.
- `devbox exec -- npm test` — run a noninteractive command.
- `devbox port 3000` — forward a React development server.
- `devbox stop` — stop compute but retain the workspace disk.
- `devbox resume` — recreate the pod and reattach the disk.
- `devbox delete` — remove pod, credentials and storage.

When running `devbox up`, collect:

- Repository owner/name from `git remote get-url origin`.
- Current branch and commit.
- Whether the working tree has uncommitted changes.
- `.devcontainer/devcontainer.json`, if present.
- Requested CPU, memory, storage and TTL.

For uncommitted changes, either upload a generated Git patch or create a temporary Git bundle. Do not silently discard local changes.

### 2. Control plane and Sandbox CRD

Run a small API and Kubernetes operator in a protected management namespace. Model each environment as a custom resource resembling:

- Repository URL and commit.
- User identity.
- Image or devcontainer specification.
- CPU/memory/storage profile.
- Allowed outbound destinations.
- TTL and stop/delete policy.
- Requested forwarded ports.
- Agent policy.

The operator should create:

1. A namespace for the sandbox.
2. Resource quotas and limit ranges.
3. Default-deny network policies.
4. A PVC for `/workspace`.
5. An init container that clones the repository.
6. The main workspace container.
7. An optional terminal/connectivity sidecar.
8. TTL cleanup and credential-revocation records.

For stopping and resuming, delete the pod but retain its PVC. If you need immutable restore points, create CSI volume snapshots before stopping.

## Isolation: use AKS Pod Sandboxing

Do not treat a regular Kubernetes pod as a sufficient boundary for agent-executed, repository-controlled code. Use a dedicated AKS user node pool with **Pod Sandboxing/Kata Containers** and set `runtimeClassName: kata-vm-isolation` on sandbox pods.

AKS Pod Sandboxing gives each sandbox pod a lightweight VM with its own kernel, isolating it from the host kernel and other pod VMs. It requires supported Azure Linux node pools and Generation 2 VM sizes with nested virtualization. ([learn.microsoft.com](https://learn.microsoft.com/en-us/azure/aks/concepts-pod-sandboxing?utm_source=openai))

Also enforce:

- Non-root containers.
- No privileged containers.
- No `hostPath`.
- No host networking or host PID.
- Drop all Linux capabilities unless specifically required.
- `seccompProfile: RuntimeDefault`.
- Disable service-account token automount.
- No Kubernetes API permissions from sandbox pods.
- Per-sandbox CPU, memory, PID and storage limits.
- Separate system and sandbox node pools.
- Admission policies through Kyverno, Gatekeeper or equivalent.
- Signed, approved base images only.

Use Azure CNI Powered by Cilium for default-deny policies and controlled outbound access. AKS recommends Cilium and supports features including FQDN and Layer 7 filtering. ([learn.microsoft.com](https://learn.microsoft.com/en-us/azure/aks/use-network-policies?utm_source=openai))

Typical outbound allow-list entries would include:

- GitHub and GitHub API.
- npm registry.
- Your organization’s package registries.
- ACR.
- Required MCP endpoints.
- Application-specific test dependencies.

Block private address ranges and Azure Instance Metadata Service unless explicitly needed.

## GitHub credential design

This is the most important part: **do not copy the developer’s SSH private key, `~/.config/gh`, or long-lived PAT into the pod.**

### Preferred model: GitHub App user authorization

Create a GitHub App for the platform and enable OAuth device flow:

1. The developer runs `devbox login`.
2. The CLI starts the GitHub App device authorization flow.
3. GitHub returns a user access token.
4. The control plane stores the refresh credential encrypted in Azure Key Vault.
5. When creating a sandbox, the broker obtains an expiring token restricted to the target `repository_id`.
6. The token is delivered to the pod through a one-time credential endpoint or ephemeral secret.
7. Git uses a credential helper to request it when cloning, fetching or pushing.

A GitHub App user token has the intersection of the user’s permissions and the app’s permissions. It therefore preserves the developer’s effective repository access and can attribute supported operations to that user. Device flow is explicitly intended for headless applications such as CLI tools. ([docs.github.com](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-a-user-access-token-for-a-github-app?utm_source=openai))

Configure the App with the minimum required permissions, for example:

- Contents: read/write if pushing is allowed.
- Pull requests: read/write if agents create PRs.
- Issues: read/write only if needed.
- Metadata: read.
- Workflows: normally excluded unless explicitly approved.

### Alternative: installation tokens

If operations may be attributed to the platform rather than the developer, use GitHub App installation tokens. They can be scoped to selected repositories and permissions and expire after one hour. ([docs.github.com](https://docs.github.com/en/rest/apps/apps?apiVersion=2026-03-10&utm_source=openai))

This is simpler and safer operationally, but it does **not** represent exactly the same user identity.

### Git credential helper

Avoid embedding the token in the clone URL or `.git/config`. Configure a helper that contacts your credential broker:

- Input: sandbox identity, repository and requested Git operation.
- Broker validates sandbox ownership and repository scope.
- Broker returns a short-lived credential.
- Helper keeps it only in process memory.

This also makes immediate revocation possible when a sandbox stops or is deleted.

## Copilot CLI authentication

Treat repository credentials and Copilot credentials separately. A GitHub App token suitable for cloning a repository should not automatically be assumed to authorize Copilot requests.

Inside the sandbox you can:

- Run `copilot login --device-code` interactively; or
- Inject a supported token through `COPILOT_GITHUB_TOKEN`, `GH_TOKEN`, or `GITHUB_TOKEN`.

Copilot CLI supports environment-token authentication for containers and noninteractive environments, as well as programmatic execution using `copilot -p`. Supported token types and precedence are documented by GitHub. ([docs.github.com](https://docs.github.com/en/copilot/reference/copilot-cli-reference/cli-programmatic-reference?utm_source=openai))

If you inject a token:

- Keep it out of pod specifications and logs.
- Mark it as a secret environment variable for transcript redaction.
- Rotate or revoke it when the sandbox ends.
- Never write it into the workspace PVC.
- Do not make it available to arbitrary sidecars.

## Development environment images

Support two environment modes.

### Standard templates

Maintain prebuilt images in ACR, for example:

- Node LTS + npm/pnpm/yarn.
- React tooling.
- Playwright plus supported browsers and OS dependencies.
- Git, GitHub CLI and Copilot CLI.
- Common build tools and language servers.

For Playwright, mount a memory-backed `emptyDir` at `/dev/shm` with an appropriate size. This avoids common Chromium failures caused by an undersized shared-memory filesystem.

### Devcontainer mode

Use `.devcontainer/devcontainer.json` as the repository-owned environment contract:

1. Resolve the devcontainer definition.
2. Build it in a separate trusted builder environment.
3. Scan and sign the resulting image.
4. Push it to ACR.
5. Launch it as the sandbox image.

Do not mount the host Docker socket. If repositories need container builds, use rootless BuildKit, a remote builder, or ACR build services.

## Azure identity

Use **Microsoft Entra Workload ID** for control-plane components that access Azure resources such as Key Vault, ACR, Storage or Monitor. This avoids storing Azure client secrets in Kubernetes. ([learn.microsoft.com](https://learn.microsoft.com/en-us/azure/aks/workload-identity-deploy-cluster?utm_source=openai))

Sandbox pods should receive no Azure identity by default. Grant a separate, narrowly scoped workload identity only when a specific development workflow requires Azure access.

## Suggested implementation sequence

### Phase 1: MVP

- One approved Node/Playwright image.
- CLI with `up`, `shell`, `exec`, `port`, `stop` and `delete`.
- Sandbox operator and CRD.
- One namespace and PVC per sandbox.
- GitHub App device authentication.
- Kata-isolated pods.
- Default-deny networking.
- TTL cleanup.

### Phase 2: Developer experience

- Devcontainer support.
- Stop/resume and volume snapshots.
- IDE access using SSH or VS Code remote tunnels.
- Browser-based terminal.
- Named compute profiles.
- Cached package directories.
- Audit transcripts and cost reporting.

### Phase 3: Enterprise hardening

- Policy-as-code.
- Per-team quotas.
- Egress domain policies.
- Image signing and admission verification.
- Secret scanning and runtime detection.
- Approval gates for push/PR/workflow changes.
- Dedicated clusters for higher-trust-boundary tenants.
- Autoscaling or AKS Node Auto-Provisioning after validating its compatibility with your sandbox node/runtime configuration. AKS NAP is based on Karpenter and supports resource and VM-family constraints. ([learn.microsoft.com](https://learn.microsoft.com/en-us/azure/aks/node-auto-provisioning-node-pools?utm_source=openai))

The key design decision is to make the **sandbox pod disposable, the workspace optionally persistent, and every credential short-lived and repository-scoped**. This gives developers the Copilot-cloud-style experience without exposing their workstation credentials or allowing agent-controlled code to reach the AKS host or management plane.

## @rmoreirao

Is there any cncf or open source project that is already providing such funcitonality? solving this?

## Copilot

Yes — this is an active space, and there are two categories worth splitting: **agent-execution sandboxes** and **full dev-environment platforms**. The closest match to what you described (AKS + CLI-driven spin-up + dev workflow + agent execution) is a combination of the two.

## 1. Kubernetes-native agent sandboxes (closest to "Copilot cloud sandbox on your own cluster")

**`kubernetes-sigs/agent-sandbox`** — this is the most directly relevant project.
- SIG-affiliated Kubernetes project purpose-built for running AI/coding agents in isolated, ephemeral sandboxes.
- Provides a CRD/controller for sandbox lifecycle (create, pause/resume, delete, TTL).
- Pluggable secure runtimes via `runtimeClassName` — supports **gVisor**, **Kata Containers**, and Firecracker-style microVM isolation.
- Designed exactly for the "agent clones a repo, runs in isolation, gets torn down" pattern you're describing.
- You could realistically build your CLI + control plane on top of this instead of writing the operator from scratch.

**Kata Containers / gVisor** — not full platforms, but the isolation runtimes you'd plug into AKS Pod Sandboxing or into `agent-sandbox` as the `runtimeClass`. AKS already supports Kata-based Pod Sandboxing natively, so this pairs well.

**kagent** (CNCF Sandbox project) — more focused on agent orchestration/lifecycle management in Kubernetes generally, less on the "give me a full dev workflow with Playwright/Node" angle, but worth watching.

## 2. Full self-hosted dev-environment platforms (closest to the "devs spin up a workspace and run their dev workflow" angle)

**Coder** (`coder/coder`) — this is the strongest match for your platform requirements:
- Fully open-source, self-hosted.
- Workspaces defined as Terraform, deployable to Kubernetes (including AKS).
- CLI-first (`coder create`, `coder ssh`, `coder config-ssh` for VS Code Remote).
- Supports automatic repo cloning via workspace templates (`git clone` in the startup script, with Git credentials injected through Coder's built-in Git auth / external auth provider — including native GitHub OAuth App integration, so it can pass through the user's own GitHub identity rather than a shared token).
- Runs arbitrary dev workflows inside the workspace — Node builds, Playwright, whatever your container image supports.
- Has an explicit "Coder for agents" story now — you can launch coding agents (Copilot CLI, Claude Code, etc.) inside a workspace and attach to it, which is very close to what you want.
- Idle/TTL-based autostop, per-workspace isolation, audit logging, RBAC — much of the platform plumbing you'd otherwise build yourself.

**DevPod** (`loft-sh/devpod`) — open-source, devcontainer-native, can provision on Kubernetes. Lighter weight than Coder, less built-in policy/governance, but very good CLI ergonomics and devcontainer.json support (so JS/Node/Playwright toolchains "just work" if the repo has one).

**Eclipse Che / Okteto / DevSpace** — same category, but generally less agent-aware and more oriented toward classic remote-IDE workflows.

## Practical recommendation

Given your requirements (AKS-hosted, CLI-driven, clone-with-original-credentials, run Node/React/Playwright, and launch agents), I'd suggest:

- **Use Coder as the platform layer** — it already solves CLI provisioning, templates, credential passthrough (via external auth), TTL/autostop, and workspace access, on top of AKS.
- **Use `kubernetes-sigs/agent-sandbox` (or Kata/gVisor runtimeClass directly)** as the underlying isolation layer for the pods Coder provisions, instead of building your own operator.
- **Layer Copilot CLI / other agents on top** as a workspace startup script or an optional module, using the same GitHub credential passthrough Coder already supports.

This gets you ~80% of the custom platform I outlined earlier without having to write the control plane, operator, and credential broker yourself — you'd mainly need to build the Coder template(s), the isolation runtime integration, and any agent-specific wiring.
