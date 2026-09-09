# DevSandbox PoC operations

This runbook covers supported lifecycle operations, health, metrics, and audit
investigation for the deployed PoC. Start with
[Architecture](architecture.md) for component relationships,
[Security](security.md) for trust boundaries and residual risks, and
[Validation and deployment](validation.md) for deployment gates.

## Safe lifecycle operations

The `DevSandbox` custom resource is the top-level owner and the only supported
operator entry point. To remove a sandbox and its upstream Sandbox, workspace
PVC/PV, Lease, ServiceAccount, jobs, and quota reservation, delete the
top-level resource:

```powershell
kubectl delete devsandbox -n devsandbox-workloads <name>
```

Do not delete child resources first and do not remove the
`devsandbox.io/finalizer`. If deletion is slow, inspect the resource,
controller logs, events, and child finalizers. Escalate rather than bypassing
cleanup. Stopping retains the workspace; deletion permanently removes it.

There is deliberately **no product administrator API** and no MVP break-glass
endpoint. Operators use scoped Kubernetes access, and every user-facing
operation remains owner-authorized at the public API.

## Health and metrics

`/healthz` reports process liveness only. `/readyz` returns 503 when a required
dependency is unavailable: API checks Kubernetes, Sandbox Router, Key Vault,
and the public Gateway; web checks Router and API exchange; broker checks
Kubernetes and Key Vault; operator checks Kubernetes; supervisor checks the
broker. Do not restart a live process to hide a dependency incident.

Prometheus-compatible metrics are on `/metrics`. Alert on readiness failures,
`reconciliation_failures_total`, `token_refresh_failures_total`, and
`clone_failures_total`; use active sandbox/PVC/storage gauges for capacity.

## Troubleshooting path

Follow the dependency path rather than restarting healthy processes:

| Layer | Check |
| --- | --- |
| Azure Front Door | API and web endpoint health, TLS, access logs, and origin probe results. |
| AKS Gateway | Listener and route readiness, origin hostnames, and the exact `X-Azure-FDID` match. |
| Management services | `/readyz`, pod events, service endpoints, workload identity, Key Vault, and Router dependencies. |
| Operator | `DevSandbox` status and conditions, controller logs, quota, activity Lease, PVC, service account, and upstream Sandbox. |
| Sandbox | Kata node placement, agent readiness, initialization output, Router reachability, broker identity, and workspace mount. |

Use AKS Run Command for cluster inspection because the AKS API is private.
Treat a failed dependency check as the incident signal; do not convert it into
success by relying only on `/healthz`.

## Audit investigation

Audit records are JSON `slog` entries with `msg == "audit"`. They contain only
allow-listed identity, sandbox, repository, lifecycle, and session metadata.
They never include tokens, commands, stdout/stderr, or file content. Import or
copy queries from [log-analytics.kql](log-analytics.kql), adjust the table name
for the cluster's Container Insights configuration, and narrow by
`sandbox_uid` or `user_id`.

## Related documentation

- [Architecture](architecture.md)
- [Security](security.md)
- [Development guide](development.md)
- [Validation and deployment](validation.md)
- [CLI reference](../README.md#use-the-devsandbox-cli)
