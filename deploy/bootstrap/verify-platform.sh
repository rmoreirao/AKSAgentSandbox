#!/bin/sh
set -eu
run_kubeconfig=./devsandbox-run-command-kubeconfig
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
namespace=devsandbox-system
workloads=devsandbox-workloads
owner_id="${1:-validation}"
owner_login="${2:-validation}"
frontdoor_id="${3:-}"
large="validation-large-e2e-$(date +%s)"
cleanup() { kubectl delete devsandbox "$large" -n "$workloads" --ignore-not-found --wait=false >/dev/null 2>&1 || true; }
trap cleanup EXIT

kubectl wait --for=condition=Ready node --all --timeout=10m
kubectl get gatewayclass approuting-istio
kubectl wait --for=condition=Programmed gateway/devsandbox -n "$namespace" --timeout=10m
kubectl rollout status deployment/agent-sandbox-controller -n agent-sandbox-system --timeout=10m
for deployment in devsandbox-api devsandbox-web devsandbox-broker devsandbox-operator sandbox-router; do
  kubectl rollout status "deployment/$deployment" -n "$namespace" --timeout=10m
done
kubectl get crd gateways.gateway.networking.k8s.io httproutes.gateway.networking.k8s.io >/dev/null
gateway_address="$(kubectl get gateway devsandbox -n "$namespace" -o jsonpath='{.status.addresses[0].value}')"
test -n "$gateway_address"
echo "GATEWAY_ADDRESS=$gateway_address"

test "$(kubectl get gateway devsandbox -n "$namespace" -o jsonpath='{.spec.listeners[?(@.name=="api-http")].protocol}')" = HTTP
test "$(kubectl get gateway devsandbox -n "$namespace" -o jsonpath='{.spec.listeners[?(@.name=="web-http")].protocol}')" = HTTP
api_host="$(kubectl get httproute devsandbox-api -n "$namespace" -o jsonpath='{.spec.hostnames[0]}')"
web_host="$(kubectl get httproute devsandbox-web -n "$namespace" -o jsonpath='{.spec.hostnames[0]}')"
test -n "$api_host"
test -n "$web_host"
test "$api_host" != "$web_host"
test "$(kubectl get httproute devsandbox-api -n "$namespace" -o jsonpath='{.spec.parentRefs[0].sectionName}')" = api-http
test "$(kubectl get httproute devsandbox-web -n "$namespace" -o jsonpath='{.spec.parentRefs[0].sectionName}')" = web-http
test -n "$frontdoor_id"
test "$(kubectl get httproute devsandbox-api -n "$namespace" -o jsonpath='{.spec.rules[0].matches[0].headers[0].name}')" = X-Azure-FDID
test "$(kubectl get httproute devsandbox-api -n "$namespace" -o jsonpath='{.spec.rules[0].matches[0].headers[0].value}')" = "$frontdoor_id"
test "$(kubectl get httproute devsandbox-web -n "$namespace" -o jsonpath='{.spec.rules[0].matches[0].headers[0].name}')" = X-Azure-FDID
test "$(kubectl get httproute devsandbox-web -n "$namespace" -o jsonpath='{.spec.rules[0].matches[0].headers[0].value}')" = "$frontdoor_id"

check_system_nodes() {
  check_namespace="$1"
  selector="$2"
  nodes="$(kubectl get pod -n "$check_namespace" -l "$selector" -o jsonpath='{range .items[*]}{.spec.nodeName}{"\n"}{end}' | sort -u)"
  test -n "$nodes"
  for node in $nodes; do
    test "$(kubectl get node "$node" -o jsonpath='{.metadata.labels.kubernetes\.azure\.com/mode}')" = system
  done
}
check_system_nodes agent-sandbox-system app=agent-sandbox-controller
check_system_nodes "$namespace" app.kubernetes.io/name=devsandbox-api
check_system_nodes "$namespace" app.kubernetes.io/name=devsandbox-web
check_system_nodes "$namespace" app.kubernetes.io/name=devsandbox-broker
check_system_nodes "$namespace" app.kubernetes.io/name=devsandbox-operator
check_system_nodes "$namespace" app.kubernetes.io/name=sandbox-router
check_system_nodes "$namespace" gateway.networking.k8s.io/gateway-name=devsandbox

test "$(kubectl get serviceaccount devsandbox-api -n "$namespace" -o jsonpath='{.metadata.annotations.azure\.workload\.identity/client-id}')" != \
     "$(kubectl get serviceaccount devsandbox-broker -n "$namespace" -o jsonpath='{.metadata.annotations.azure\.workload\.identity/client-id}')"

version="$(kubectl get devsandboxtemplate standard -o jsonpath='{.spec.version}')"
digest="$(kubectl get devsandboxtemplate standard -o jsonpath='{.spec.image.digest}')"
cat <<EOF | kubectl apply -f -
apiVersion: devsandbox.io/v1alpha1
kind: DevSandbox
metadata:
  name: $large
  namespace: $workloads
spec:
  owner: {githubUserId: "$owner_id", githubLogin: "$owner_login"}
  source: {type: empty}
  template: {name: standard, version: "$version", imageDigest: "$digest"}
  profile: {name: large, cpu: "8", memory: 16Gi, storage: 80Gi}
  lifecycle: {idleTimeoutSeconds: 600, stoppedRetentionSeconds: 60, failedRetentionSeconds: 60}
  desiredState: Running
EOF
deadline=$(($(date +%s)+900))
while [ "$(date +%s)" -lt "$deadline" ]; do
  phase="$(kubectl get devsandbox "$large" -n "$workloads" -o jsonpath='{.status.phase}')"
  [ "$phase" = Running ] && break
  [ "$phase" = Failed ] && exit 41
  sleep 10
done
test "$(kubectl get devsandbox "$large" -n "$workloads" -o jsonpath='{.status.phase}')" = Running
pod="$(kubectl get pods -n "$workloads" -l devsandbox.io/sandbox="$large" -o jsonpath='{.items[0].metadata.name}')"
test -n "$pod"
test "$(kubectl get pod "$pod" -n "$workloads" -o jsonpath='{.spec.runtimeClassName}')" = kata-vm-isolation
test "$(kubectl get node "$(kubectl get pod "$pod" -n "$workloads" -o jsonpath='{.spec.nodeName}')" -o jsonpath='{.metadata.labels.devsandbox\.github\.com/runtime}')" = kata
