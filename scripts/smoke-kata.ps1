[CmdletBinding()]
param(
    [Parameter(Mandatory)]
    [string]$ResourceGroupName,

    [Parameter(Mandatory)]
    [string]$AksClusterName,

    [string]$SubscriptionId = $env:AZURE_SUBSCRIPTION_ID,

    [string]$ManifestPath = (Join-Path $PSScriptRoot '..\deploy\smoke\kata-sandbox.yaml')
)

$ErrorActionPreference = 'Stop'
$PSNativeCommandUseErrorActionPreference = $true

if (-not (Get-Command az -ErrorAction SilentlyContinue)) {
    throw 'Kata smoke test Blocked: missing tool: az'
}
if ([string]::IsNullOrWhiteSpace($SubscriptionId)) {
    throw 'Kata smoke test Blocked: AZURE_SUBSCRIPTION_ID is required.'
}
if (-not (Test-Path -LiteralPath $ManifestPath)) {
    throw "Kata smoke test manifest was not found: $ManifestPath"
}

& az account set --subscription $SubscriptionId --only-show-errors
if ($LASTEXITCODE -ne 0) {
    throw 'Kata smoke test Blocked: Azure authentication or subscription access is unavailable.'
}

$uploadedName = Split-Path -Leaf $ManifestPath
$command = @"
set -eu
cleanup() {
  kubectl delete -f '$uploadedName' --ignore-not-found=true --wait=true
}
trap cleanup EXIT
kubectl apply -f '$uploadedName'
kubectl wait --for=condition=Ready sandbox/devsandbox-kata-smoke -n devsandbox-workloads --timeout=5m
kubectl wait --for=condition=Ready pod/devsandbox-kata-smoke -n devsandbox-workloads --timeout=5m
test "`$(kubectl get pod devsandbox-kata-smoke -n devsandbox-workloads -o jsonpath='{.spec.runtimeClassName}')" = "kata-vm-isolation"
guest_kernel="`$(kubectl exec -n devsandbox-workloads devsandbox-kata-smoke -- uname -r)"
node="`$(kubectl get pod devsandbox-kata-smoke -n devsandbox-workloads -o jsonpath='{.spec.nodeName}')"
node_kernel="`$(kubectl get node "`$node" -o jsonpath='{.status.nodeInfo.kernelVersion}')"
echo "guest kernel: `$guest_kernel"
echo "node kernel:  `$node_kernel"
test "`$guest_kernel" != "`$node_kernel"
"@

$rawResult = & az aks command invoke `
    --resource-group $ResourceGroupName `
    --name $AksClusterName `
    --file $ManifestPath `
    --command $command `
    --only-show-errors `
    --output json
if ($LASTEXITCODE -ne 0) {
    throw "Kata smoke test failed with exit code $LASTEXITCODE."
}

$result = $rawResult | ConvertFrom-Json
if ($result.provisioningState -ne 'Succeeded') {
    throw "Kata smoke test failed with provisioning state '$($result.provisioningState)'.`n$($result.logs)"
}
if ($null -ne $result.exitCode -and [int]$result.exitCode -ne 0) {
    throw "Kata smoke command failed with exit code $($result.exitCode).`n$($result.logs)"
}

Write-Host $result.logs
Write-Host 'Agent Sandbox reconciliation, Kata guest isolation, and cleanup verified.'
