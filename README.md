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

After a successful local deployment, the repository root contains a native
`devsandbox.exe` and an ignored, secret-free `.env` generated from the Azure
Front Door outputs. On Windows, run the local binary from the repository root:

```powershell
.\devsandbox.exe login
.\devsandbox.exe doctor
.\devsandbox.exe templates
.\devsandbox.exe up
```

The examples below use `devsandbox` so they also apply when the executable is
installed on `PATH`. Replace it with `.\devsandbox.exe` when running the
repository-local Windows binary.

### First use

1. Authenticate. The generated local PoC configuration uses the GitHub CLI, so
   first run `gh auth login` for the deployed GitHub account and then run
   `devsandbox login`. A device-flow deployment instead prints a GitHub URL and
   one-time code.
2. Run `devsandbox doctor` from the Git repository you want to use. It checks
   the CLI configuration, authentication, API access, available templates, and
   local repository readiness.
3. Run `devsandbox templates` to choose an environment. Use
   `devsandbox templates show NAME` for its image, default resource profile,
   and entry action.
4. Run `devsandbox up`. From a clean Git repository with a pushed `HEAD`, this
   creates a sandbox from that exact commit. It waits for the sandbox to become
   ready and then performs the template's entry action: attach a shell, open
   VS Code in the browser, or start Copilot CLI.

Use `devsandbox list` to find sandbox names. Commands whose syntax contains
`[NAME]` can infer the target from the current Git repository. If exactly one
sandbox is associated with that repository, the name may be omitted. If there
are multiple matches, an interactive terminal prompts for one; automation must
pass the sandbox name explicitly. Outside a matching Git repository, `NAME` is
required.

### Configuration and authentication

Configuration precedence is:

1. command flags;
2. variables already set in the process environment;
3. a local environment file;
4. built-in defaults.

Set `DEVSANDBOX_ENV_FILE` to load a specific environment file. Otherwise, the
CLI looks for `.env` beside the executable, not in the current working
directory. Environment-file values never overwrite variables already present
in the process.

| Variable | Purpose |
| --- | --- |
| `DEVSANDBOX_ENV_FILE` | Select another CLI environment file. |
| `DEVSANDBOX_API_URL` | Set the management API URL. Defaults to `http://localhost:8080`; `--api-url` overrides it. |
| `DEVSANDBOX_DEFAULT_TEMPLATE` | Set the default for `up --template`. Without a default, an interactive terminal prompts and automation fails with the available choices. |
| `DEVSANDBOX_AUTH_MODE` | Select `device-flow` or `github-cli-static`. An unset value uses device flow. |
| `DEVSANDBOX_GITHUB_LOGIN` | Identify the deployed GitHub account used by `github-cli-static`. |
| `DEVSANDBOX_API_HOST_HEADER` | Override the HTTP host and TLS server name for validation environments. |
| `DEVSANDBOX_SKIP_TLS_VERIFY` | Set to `true` only in an isolated validation environment to disable API and WebSocket certificate verification. Never use it for normal deployments. |

In `device-flow` mode, `devsandbox login` completes GitHub device
authorization through the management API. In the local
`github-cli-static` PoC mode, it runs `gh auth token --user LOGIN` and exchanges
that credential for a DevSandbox platform session. Authenticated commands can
repeat that exchange automatically when the platform session is absent or
expired.

Only the short-lived DevSandbox platform session is stored, using Windows
Credential Manager, macOS Keychain, or Linux Secret Service. The GitHub
bootstrap credential remains in process memory. There is no file or plaintext
credential fallback.

### Global commands and flags

| Form | Behavior |
| --- | --- |
| `devsandbox --help`, `devsandbox -h` | Show the top-level command list and global flags. |
| `devsandbox help [COMMAND]` | Show help for the root command or a command path, such as `devsandbox help jobs stop`. |
| `devsandbox COMMAND --help` | Show help for a specific command. |
| `devsandbox --version`, `devsandbox -v` | Print the CLI version. |
| `devsandbox --api-url URL COMMAND` | Use an absolute HTTP(S) management API URL for this invocation. |
| `devsandbox --json COMMAND` | Request machine-readable results and structured errors. |

`--api-url` and `--json` are inherited by every subcommand. Commands that
return resources, lists, mutations, tunnel metadata, or managed-job IDs encode
those results as JSON. `shell` and foreground `exec` still carry their raw
interactive or streamed output; with `--json`, their DevSandbox errors are
structured.

Generate shell completion with any of these forms:

```text
devsandbox completion bash [--no-descriptions]
devsandbox completion fish [--no-descriptions]
devsandbox completion powershell [--no-descriptions]
devsandbox completion zsh [--no-descriptions]
```

Load completion for the current shell:

```bash
# Bash
source <(devsandbox completion bash)

# Fish
devsandbox completion fish | source

# Zsh
source <(devsandbox completion zsh)
```

```powershell
# PowerShell
devsandbox completion powershell | Out-String | Invoke-Expression
```

Redirect the generated script into the shell's completion directory or profile
to load it in future sessions. `--no-descriptions` produces a smaller script
without command descriptions.

### Authentication and readiness

| Command | Behavior |
| --- | --- |
| `devsandbox login` | Authenticate through GitHub device flow or the configured GitHub CLI static profile, then store the platform session in the OS credential store. |
| `devsandbox doctor` | Check configuration, authentication, API access, templates, and the current repository. |

Both commands support `--json`. Device-flow login emits the authorization
details first and the authenticated result after approval. A static-profile
login emits only the authenticated result.

### Inspect templates

```text
devsandbox templates
devsandbox templates show NAME
devsandbox --json templates
devsandbox --json templates show NAME
```

`templates` lists each available template's name, version, default profile,
entry action, and description. `templates show` adds its display name and
pinned image digest. Template names come from the API; the PoC normally
provides `standard`, `vscode`, and `copilot`.

### Create or reuse a sandbox

Choose one source form:

```text
devsandbox up
devsandbox up --repo OWNER/REPOSITORY
devsandbox up --repo https://github.com/OWNER/REPOSITORY.git
devsandbox up --empty
```

- With no source flag, `up` uses the current Git repository. The worktree must
  be clean, `HEAD` must be pushed to a GitHub remote, and the remote commit must
  match exactly. If more than one suitable remote exists and `origin` cannot
  be selected, an interactive terminal asks which one to use.
- `--repo` accepts `OWNER/REPOSITORY` or a GitHub URL. It resolves the
  repository's default branch unless `--ref BRANCH_OR_TAG_OR_COMMIT` is also
  supplied.
- `--empty` creates an empty workspace. It is mutually exclusive with `--repo`
  and cannot be combined with `--ref`.
- Outside a Git repository, pass either `--repo` or `--empty`.

Customize creation by combining the source with these flags:

| Flag | Behavior |
| --- | --- |
| `--template NAME` | Select a template. Defaults to `DEVSANDBOX_DEFAULT_TEMPLATE`; otherwise prompts only in an interactive terminal. |
| `--profile small\|medium\|large` | Override the template's default resource profile. |
| `--name NAME` | Set a lowercase DNS-label name of at most 63 characters. Otherwise, the CLI generates one. |
| `--ref REF` | Resolve a branch, tag, or commit for an explicit `--repo`. It cannot be used with `--empty`. |
| `--idle-timeout DURATION` | Override the idle timeout with a Go duration greater than zero and no more than `2h`, such as `30m`, `90m`, or `1h30m`. |
| `--resume-existing` | Resume an equivalent stopped sandbox. Fails if none exists. |
| `--new` | Always create a new sandbox, even when an equivalent stopped sandbox exists. |
| `--no-attach` | Return after the sandbox reaches `Running` instead of performing the template entry action. |

`--resume-existing` and `--new` are mutually exclusive. Without either flag,
an interactive terminal asks what to do when an equivalent stopped sandbox
exists. Noninteractive use must choose explicitly.

Examples:

```text
# Current pushed repository, template defaults, and automatic entry action
devsandbox up

# Explicit repository and branch using a larger profile
devsandbox up --repo acme/widgets --ref feature/api --template standard --profile large

# Named empty Copilot workspace with a 90-minute idle timeout
devsandbox up --empty --template copilot --name investigation --idle-timeout 90m

# Automation-safe creation
devsandbox --json up --repo acme/widgets --template standard --new --no-attach

# Reuse matching retained storage and return without attaching
devsandbox up --empty --template standard --resume-existing --no-attach
```

### List and inspect sandboxes

```text
devsandbox list
devsandbox list --all
devsandbox status
devsandbox status NAME
```

`list` shows active sandboxes in `Provisioning`, `Initializing`, `Running`,
`Resuming`, or `Stopping`. Add `--all` to include `Stopped` and `Failed`
sandboxes. The table includes the template version, profile, source, age, last
activity, and applicable idle or retention deadline.

`status` shows the selected sandbox's desired and current states, source,
timestamps, deadlines, and recent conditions. Both commands support `--json`.

### Connect and run commands

Open an interactive shell:

```text
devsandbox shell
devsandbox shell NAME
```

Run a foreground command. The `--` separator is required so arguments after it
are sent to the sandbox rather than parsed as DevSandbox flags:

```text
devsandbox exec -- COMMAND [ARGUMENTS...]
devsandbox exec NAME -- COMMAND [ARGUMENTS...]

devsandbox exec -- go test ./...
devsandbox exec my-sandbox -- git status --short
```

Stdout and stderr are streamed separately, and the local CLI returns the
remote process's exit code unchanged. A foreground command and an attached
shell renew sandbox activity while connected.

Run a command as a managed detached job:

```text
devsandbox exec --detach -- COMMAND [ARGUMENTS...]
devsandbox exec -d NAME -- COMMAND [ARGUMENTS...]
```

The command returns a job ID. Managed jobs continue after the CLI exits and
suppress idle stopping; arbitrary background processes started with `&` or
`nohup` are not managed jobs.

List or stop managed jobs:

```text
devsandbox jobs
devsandbox jobs NAME
devsandbox jobs stop JOB_ID
devsandbox jobs stop NAME JOB_ID
```

When stopping a job, omit `NAME` only when the current repository identifies
the sandbox. `jobs`, detached `exec`, and `jobs stop` support `--json`.

### Forward a port

```text
devsandbox port REMOTE_PORT
devsandbox port NAME REMOTE_PORT
devsandbox port [NAME] REMOTE_PORT --local-port LOCAL_PORT
devsandbox port [NAME] REMOTE_PORT --local-port 0
```

Without `--local-port`, the local port equals `REMOTE_PORT`. Passing
`--local-port 0` selects an available local port. Both port values must be from
1 through 65535, except for the explicit local value `0`. The CLI listens only
on `127.0.0.1`, prints the selected address, and carries raw TCP traffic through
an authenticated WebSocket; it does not expose a public Kubernetes Service or
Ingress. The command runs until interrupted and renews sandbox activity while
connections are active. With `--json`, the initial local address and port
metadata are machine-readable.

### Stop, resume, or delete

```text
devsandbox stop
devsandbox stop NAME

devsandbox resume
devsandbox resume NAME

devsandbox delete
devsandbox delete NAME
devsandbox delete [NAME] --yes
devsandbox delete [NAME] -y
```

`stop` terminates sandbox processes and managed jobs but retains the workspace
for the retention period. `resume` recreates compute around the retained
workspace, waits for `Running`, and returns without attaching. `delete`
permanently removes the sandbox and workspace. It prompts for confirmation
unless `--yes` or `-y` is supplied; noninteractive deletion must use that flag.
All three commands support `--json`.

### Automation and exit codes

Prompts are used only when stdin is a terminal. For reliable automation:

- pass `--template` when no default template is configured;
- pass `--resume-existing` or `--new` when a stopped equivalent may exist;
- pass an explicit sandbox `NAME` when repository-based selection may be
  unavailable or ambiguous;
- pass `--yes` to `delete`;
- pass `--no-attach` to `up`;
- use `--json` instead of parsing human-readable tables.

For example:

```powershell
$sandbox = devsandbox --json up --empty --template standard --new --no-attach |
    ConvertFrom-Json
devsandbox --json status $sandbox.name
devsandbox --json exec --detach $sandbox.name -- pwsh -File ./build.ps1
```

Structured errors contain `code`, `message`, and optional `details` fields.
The CLI uses these process exit codes:

| Code | Meaning |
| --- | --- |
| `0` | Success. |
| `2` | Invalid arguments or a required noninteractive choice. |
| `3` | Authentication is required or expired. |
| `4` | A sandbox, repository, or template was not found, or a target is ambiguous. |
| `5` | Conflict, invalid state transition, or quota exceeded. |
| `6` | Provisioning or infrastructure failure. |
| `7` | A remote transport failed before an exit code was available. |
| `10` | Unexpected internal error. |

`devsandbox exec` returns the remote process exit code instead when the agent
reported one.

## Operations and observability

See [the PoC operator runbook](docs/operations.md) for safe top-level
`DevSandbox` deletion, dependency-aware probes, metrics, and the explicit lack
of an administrator API. [Log Analytics queries](docs/log-analytics.kql) and
the separate [`scripts/audit-e2e.ps1`](scripts/audit-e2e.ps1) audit validator
support Azure Monitor operations.
