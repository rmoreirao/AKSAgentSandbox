#!/bin/sh
set -eu
run_kubeconfig=/tmp/devsandbox-run-command-kubeconfig
kubectl config set-cluster in-cluster \
  --server="https://${KUBERNETES_SERVICE_HOST}:${KUBERNETES_SERVICE_PORT_HTTPS}" \
  --certificate-authority=/var/run/secrets/kubernetes.io/serviceaccount/ca.crt \
  --embed-certs=true --kubeconfig="$run_kubeconfig" >/dev/null
kubectl config set-credentials run-command \
  --token="$(cat /var/run/secrets/kubernetes.io/serviceaccount/token)" \
  --kubeconfig="$run_kubeconfig" >/dev/null
kubectl config set-context run-command --cluster=in-cluster --user=run-command \
  --kubeconfig="$run_kubeconfig" >/dev/null
kubectl config use-context run-command --kubeconfig="$run_kubeconfig" >/dev/null
export KUBECONFIG="$run_kubeconfig"
system=devsandbox-system
workloads=devsandbox-workloads
kubectl get runtimeclass kata-vm-isolation >/dev/null
test "$(kubectl get serviceaccount devsandbox-web -n "$system" -o jsonpath='{.automountServiceAccountToken}')" = false
test "$(kubectl auth can-i '*' '*' --as=system:serviceaccount:$system:devsandbox-web -n "$workloads")" = no
test "$(kubectl auth can-i get secrets --as=system:serviceaccount:$system:devsandbox-api -n "$workloads")" = no
test "$(kubectl auth can-i get nodes --as=system:serviceaccount:$system:devsandbox-api)" = no
test "$(kubectl auth can-i create devsandboxes.devsandbox.io --as=system:serviceaccount:$system:devsandbox-broker -n "$workloads")" = no
test "$(kubectl auth can-i get secrets --as=system:serviceaccount:$system:devsandbox-broker -n "$workloads")" = no
kubectl get networkpolicy default-deny-ingress -n "$system" >/dev/null
kubectl get networkpolicy default-deny-ingress -n "$workloads" >/dev/null
test "$(kubectl get networkpolicy workloads-to-broker -n "$system" -o jsonpath='{.spec.ingress[0].ports[0].port}')" = 8443
test "$(kubectl get networkpolicy gateway-frontdoor-http-and-health -n "$system" -o jsonpath='{.spec.ingress[0].ports[0].port}')" = 80
test "$(kubectl get networkpolicy gateway-frontdoor-http-and-health -n "$system" -o jsonpath='{.spec.ingress[1].ports[0].port}')" = 15021
api_id="$(kubectl get serviceaccount devsandbox-api -n "$system" -o jsonpath='{.metadata.annotations.azure\.workload\.identity/client-id}')"
broker_id="$(kubectl get serviceaccount devsandbox-broker -n "$system" -o jsonpath='{.metadata.annotations.azure\.workload\.identity/client-id}')"
test -n "$api_id"
test -n "$broker_id"
test "$api_id" != "$broker_id"
for pod in $(kubectl get pods -n "$workloads" -l devsandbox.io/managed=true -o name); do
  test "$(kubectl get -n "$workloads" "$pod" -o jsonpath='{.spec.runtimeClassName}')" = kata-vm-isolation
  test "$(kubectl get -n "$workloads" "$pod" -o jsonpath='{.spec.automountServiceAccountToken}')" = false
  sa="$(kubectl get -n "$workloads" "$pod" -o jsonpath='{.spec.serviceAccountName}')"
  case "$sa" in devsandbox-*) ;; *) exit 31 ;; esac
  test "$(kubectl get serviceaccount "$sa" -n "$workloads" -o jsonpath='{.automountServiceAccountToken}')" = false
  test "$(kubectl auth can-i '*' '*' --as=system:serviceaccount:$workloads:$sa -n "$workloads")" = no
  test "$(kubectl get -n "$workloads" "$pod" -o jsonpath='{.spec.securityContext.runAsNonRoot}')" = true
  test "$(kubectl get -n "$workloads" "$pod" -o jsonpath='{.spec.securityContext.seccompProfile.type}')" = RuntimeDefault
  test "$(kubectl get -n "$workloads" "$pod" -o jsonpath='{.spec.hostNetwork}')" != true
  test "$(kubectl get -n "$workloads" "$pod" -o jsonpath='{.spec.hostPID}')" != true
  test "$(kubectl get -n "$workloads" "$pod" -o jsonpath='{.spec.hostIPC}')" != true
  test -z "$(kubectl get -n "$workloads" "$pod" -o jsonpath='{.spec.volumes[?(@.hostPath)].name}')"
  test "$(kubectl get -n "$workloads" "$pod" -o jsonpath='{.spec.volumes[?(@.name=="broker-identity")].projected.sources[0].serviceAccountToken.audience}')" = devsandbox-credential-broker
  test "$(kubectl get -n "$workloads" "$pod" -o jsonpath='{.spec.containers[?(@.name=="workspace")].securityContext.allowPrivilegeEscalation}')" = false
  test "$(kubectl get -n "$workloads" "$pod" -o jsonpath='{.spec.containers[?(@.name=="workspace")].securityContext.runAsNonRoot}')" = true
  test "$(kubectl get -n "$workloads" "$pod" -o jsonpath='{.spec.containers[?(@.name=="workspace")].securityContext.seccompProfile.type}')" = RuntimeDefault
  test "$(kubectl get -n "$workloads" "$pod" -o jsonpath='{.spec.containers[?(@.name=="workspace")].securityContext.capabilities.drop[0]}')" = ALL
  cpu_req="$(kubectl get -n "$workloads" "$pod" -o jsonpath='{.spec.containers[?(@.name=="workspace")].resources.requests.cpu}')"
  cpu_lim="$(kubectl get -n "$workloads" "$pod" -o jsonpath='{.spec.containers[?(@.name=="workspace")].resources.limits.cpu}')"
  mem_req="$(kubectl get -n "$workloads" "$pod" -o jsonpath='{.spec.containers[?(@.name=="workspace")].resources.requests.memory}')"
  mem_lim="$(kubectl get -n "$workloads" "$pod" -o jsonpath='{.spec.containers[?(@.name=="workspace")].resources.limits.memory}')"
  test "$cpu_req" = "$cpu_lim"
  test "$mem_req" = "$mem_lim"
done
if kubectl get devsandboxes,pods -n "$workloads" -o yaml | grep -Eqi 'gh[pousr]_[A-Za-z0-9_]{20,}|github_pat_[A-Za-z0-9_]{20,}|Bearer[[:space:]]+[A-Za-z0-9._~-]{20,}'; then
  exit 32
fi
echo "deployed Section 14.5 manifest and Pod assertions passed"
