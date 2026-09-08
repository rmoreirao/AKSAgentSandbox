[CmdletBinding()]
param(
    [ValidateSet('Auto', 'API', 'RunCommand')]
    [string]$Method = 'Auto',

    [string]$ResourceGroupName = $env:DEVSANDBOX_RESOURCE_GROUP,
    [string]$AksClusterName = $env:DEVSANDBOX_AKS_CLUSTER
)

$ErrorActionPreference = 'Stop'
$patterns = @(
    'baseline-e2e-*',
    'repo-e2e-*',
    'connect-e2e-*',
    'job-idle-e2e-*',
    'quota-e2e-*',
    'retention-e2e-*',
    'vscode-e2e-*',
    'copilot-e2e-*'
)

function Remove-ThroughAPI {
    if ([string]::IsNullOrWhiteSpace($env:DEVSANDBOX_API_HOSTNAME) -or
        [string]::IsNullOrWhiteSpace($env:DEVSANDBOX_TEST_USER_SESSION)) {
        return $false
    }
    $headers = @{ Authorization = "Bearer $env:DEVSANDBOX_TEST_USER_SESSION" }
    if (-not [string]::IsNullOrWhiteSpace($env:DEVSANDBOX_API_HOST_HEADER)) {
        $headers.Host = $env:DEVSANDBOX_API_HOST_HEADER
    }
    $baseURL = if ($env:DEVSANDBOX_API_URL) {
        $env:DEVSANDBOX_API_URL.TrimEnd('/')
    }
    else {
        "https://$env:DEVSANDBOX_API_HOSTNAME"
    }
    $skipTLS = [string]::Equals($env:DEVSANDBOX_SKIP_TLS_VERIFY, 'true', [StringComparison]::OrdinalIgnoreCase)
    try {
        $items = @((
            Invoke-RestMethod -Method Get -Uri "$baseURL/v1/sandboxes" `
                -Headers $headers -TimeoutSec 30 -SkipCertificateCheck:$skipTLS
        ).items)
        foreach ($item in $items) {
            if ($patterns | Where-Object { $item.name -like $_ }) {
                try {
                    Invoke-RestMethod -Method Delete `
                        -Uri "$baseURL/v1/sandboxes/$($item.name)" `
                        -Headers $headers -TimeoutSec 30 -SkipCertificateCheck:$skipTLS | Out-Null
                }
                catch {
                    if ($_.Exception.Response.StatusCode.value__ -ne 404) { throw }
                }
            }
        }
        return $true
    }
    catch {
        if ($Method -eq 'API') { throw }
        return $false
    }
}

function Remove-ThroughRunCommand {
    if (-not (Get-Command az -ErrorAction SilentlyContinue) -or
        [string]::IsNullOrWhiteSpace($ResourceGroupName) -or
        [string]::IsNullOrWhiteSpace($AksClusterName)) {
        Write-Output 'Blocked: cleanup requires API session inputs or Azure Run Command inputs.'
        exit 2
    }
    $command = "kubectl get devsandboxes -n devsandbox-workloads -o name | " +
        "grep -E '/(baseline|repo|connect|job-idle|quota|retention|vscode|copilot)-e2e-' | " +
        "xargs -r kubectl delete -n devsandbox-workloads --ignore-not-found --wait=false"
    $raw = & az aks command invoke -g $ResourceGroupName -n $AksClusterName `
        --command $command --only-show-errors --output json
    if ($LASTEXITCODE -ne 0) { throw 'AKS Run Command cleanup failed.' }
    $result = $raw | ConvertFrom-Json
    if ($result.provisioningState -ne 'Succeeded' -or
        ($null -ne $result.exitCode -and [int]$result.exitCode -ne 0)) {
        throw 'AKS Run Command cleanup did not succeed.'
    }
}

$removed = $false
if ($Method -in @('Auto', 'API')) {
    $removed = Remove-ThroughAPI
}
if (-not $removed -and $Method -in @('Auto', 'RunCommand')) {
    Remove-ThroughRunCommand
}
Write-Host 'Passed: E2E cleanup is complete (already-absent resources are accepted).'
