[CmdletBinding()]
param(
    [Parameter(Mandatory)]
    [string]$ResourceGroupName,

    [Parameter(Mandatory)]
    [string]$AksClusterName,

    [string]$SubscriptionId = $env:AZURE_SUBSCRIPTION_ID
)

$ErrorActionPreference = 'Stop'
$PSNativeCommandUseErrorActionPreference = $true

if (-not (Get-Command az -ErrorAction SilentlyContinue)) {
    throw 'RuntimeClass verification Blocked: missing tool: az'
}
if ([string]::IsNullOrWhiteSpace($SubscriptionId)) {
    throw 'RuntimeClass verification Blocked: AZURE_SUBSCRIPTION_ID is required.'
}

& az account set --subscription $SubscriptionId --only-show-errors
if ($LASTEXITCODE -ne 0) {
    throw 'RuntimeClass verification Blocked: Azure authentication or subscription access is unavailable.'
}

$command = "kubectl get runtimeclass kata-vm-isolation -o jsonpath='{.handler}'"

$rawResult = & az aks command invoke `
    --resource-group $ResourceGroupName `
    --name $AksClusterName `
    --command $command `
    --only-show-errors `
    --output json
if ($LASTEXITCODE -ne 0) {
    throw "RuntimeClass verification failed with exit code $LASTEXITCODE."
}

$result = $rawResult | ConvertFrom-Json
if ($result.provisioningState -ne 'Succeeded') {
    throw "RuntimeClass verification failed with provisioning state '$($result.provisioningState)'.`n$($result.logs)"
}
if ($null -ne $result.exitCode -and [int]$result.exitCode -ne 0) {
    throw "RuntimeClass verification command failed with exit code $($result.exitCode).`n$($result.logs)"
}
if ([string]::IsNullOrWhiteSpace([string]$result.logs)) {
    throw 'RuntimeClass verification returned an empty handler.'
}

Write-Host "kata-vm-isolation handler: $($result.logs)"
Write-Host 'AKS-provided kata-vm-isolation RuntimeClass verified through AKS Run Command.'
