[CmdletBinding()]
param(
    [ValidateSet('Foundation', 'Deployment', 'E2E', 'All')]
    [string]$Mode = 'All',

    [switch]$SkipAzureChecks,

    [string]$SubscriptionId = $env:AZURE_SUBSCRIPTION_ID,
    [string]$Location = $env:AZURE_LOCATION,
    [string]$ResourcePrefix = $env:DEVSANDBOX_RESOURCE_PREFIX,
    [string]$DeploymentPrincipalObjectId = $env:AZURE_DEPLOYMENT_PRINCIPAL_ID,
    [string]$SystemNodeVmSize = $(if ($env:DEVSANDBOX_SYSTEM_NODE_VM_SIZE) { $env:DEVSANDBOX_SYSTEM_NODE_VM_SIZE } else { 'Standard_D4s_v6' }),
    [string]$KataNodeVmSize = $(if ($env:DEVSANDBOX_KATA_NODE_VM_SIZE) { $env:DEVSANDBOX_KATA_NODE_VM_SIZE } else { 'Standard_D16s_v6' }),
    [string]$ResourceGroupName = $env:DEVSANDBOX_RESOURCE_GROUP,
    [string]$AksClusterName = $env:DEVSANDBOX_AKS_CLUSTER,
    [string]$ContainerRegistryName = $env:DEVSANDBOX_CONTAINER_REGISTRY
)

$ErrorActionPreference = 'Stop'
$PSNativeCommandUseErrorActionPreference = $false

function Stop-Blocked {
    param([Parameter(Mandatory)][string]$Reason)
    Write-Output "Blocked: $Reason"
    exit 2
}

function Get-MissingInputs {
    $deployment = @(
        'AZURE_SUBSCRIPTION_ID',
        'AZURE_LOCATION',
        'DEVSANDBOX_RESOURCE_PREFIX',
        'AZURE_DEPLOYMENT_PRINCIPAL_ID'
    )
    $foundation = @(
        'AZURE_SUBSCRIPTION_ID',
        'AZURE_LOCATION',
        'DEVSANDBOX_RESOURCE_PREFIX',
        'AZURE_DEPLOYMENT_PRINCIPAL_ID'
    )
    $e2e = @(
        'DEVSANDBOX_TEST_ORG',
        'DEVSANDBOX_TEST_PRIVATE_REPO',
        'DEVSANDBOX_TEST_PUBLIC_REPO',
        'DEVSANDBOX_TEST_USER',
        'DEVSANDBOX_TEST_USER_SESSION',
        'DEVSANDBOX_TEST_SECOND_USER_SESSION',
        'DEVSANDBOX_API_HOSTNAME',
        'DEVSANDBOX_WEB_HOSTNAME'
    )
    $required = switch ($Mode) {
        'Foundation' { $foundation }
        'Deployment' { $deployment }
        'E2E' { $e2e }
        'All' { $deployment + $e2e }
    }
    $parameterValues = @{
        AZURE_SUBSCRIPTION_ID = $SubscriptionId
        AZURE_LOCATION = $Location
        DEVSANDBOX_RESOURCE_PREFIX = $ResourcePrefix
        AZURE_DEPLOYMENT_PRINCIPAL_ID = $DeploymentPrincipalObjectId
    }
    $missing = @($required | Sort-Object -Unique | Where-Object {
        $value = if ($parameterValues.ContainsKey($_)) {
            $parameterValues[$_]
        }
        else {
            [Environment]::GetEnvironmentVariable($_)
        }
        [string]::IsNullOrWhiteSpace([string]$value)
    })
    return @($missing)
}

function Assert-Tools {
    $tools = @('az', 'kubectl')
    if ($Mode -in @('E2E', 'All')) {
        $tools += @('node', 'npm', 'npx')
    }
    if ($Mode -in @('Deployment', 'All')) {
        if ([string]::IsNullOrWhiteSpace($env:DEVSANDBOX_GITHUB_APP_CLIENT_ID) -and
            [string]::IsNullOrWhiteSpace($env:DEVSANDBOX_GITHUB_STATIC_TOKEN)) {
            $tools += 'gh'
        }
        if ([string]::IsNullOrWhiteSpace($env:DEVSANDBOX_BROKER_TLS_CERTIFICATE) -or
            [string]::IsNullOrWhiteSpace($env:DEVSANDBOX_BROKER_TLS_PRIVATE_KEY)) {
            $tools += 'openssl'
        }
    }
    $missing = @($tools | Sort-Object -Unique | Where-Object {
        -not (Get-Command $_ -ErrorAction SilentlyContinue)
    })
    if ($missing.Count -gt 0) {
        Stop-Blocked "missing tools: $($missing -join ', ')"
    }
    & az bicep version *> $null
    if ($LASTEXITCODE -ne 0) {
        Stop-Blocked 'Azure CLI Bicep is unavailable (tool: az bicep).'
    }
    & kubectl kustomize (Join-Path $PSScriptRoot '..\deploy\kustomize\overlays\poc') *> $null
    if ($LASTEXITCODE -ne 0) {
        Stop-Blocked 'kubectl Kustomize support is unavailable.'
    }
    if ($Mode -in @('E2E', 'All')) {
        & npx --no-install playwright test --list *> $null
        if ($LASTEXITCODE -ne 0) {
            Stop-Blocked 'Playwright dependencies are unavailable; run npm ci.'
        }
    }
}

function Assert-AzureIdentityAndQuota {
    & az account set --subscription $SubscriptionId --only-show-errors
    if ($LASTEXITCODE -ne 0) {
        Stop-Blocked 'Azure authentication or subscription access is unavailable.'
    }

    $assignmentsJson = & az role assignment list --all --assignee $DeploymentPrincipalObjectId `
        --query '[].{role:roleDefinitionName,scope:scope}' --output json --only-show-errors 2>$null
    if ($LASTEXITCODE -ne 0) {
        Stop-Blocked 'deployment identity role assignments could not be inspected.'
    }
    $assignments = @($assignmentsJson | ConvertFrom-Json)
    if ($assignments.Count -eq 0) {
        Stop-Blocked 'deployment identity has no visible Azure role assignments.'
    }

    $roleNames = @($assignments | ForEach-Object role | Sort-Object -Unique)
    $roleDefinitions = @()
    foreach ($roleName in $roleNames) {
        $definitionJson = & az role definition list --name $roleName --output json --only-show-errors 2>$null
        if ($LASTEXITCODE -eq 0) {
            $roleDefinitions += @($definitionJson | ConvertFrom-Json)
        }
    }
    function Test-ActionAllowed {
        param([string]$Action)
        foreach ($definition in $roleDefinitions) {
            foreach ($permission in $definition.permissions) {
                $allowed = @($permission.actions | Where-Object { $Action -like $_ }).Count -gt 0
                $denied = @($permission.notActions | Where-Object { $Action -like $_ }).Count -gt 0
                if ($allowed -and -not $denied) { return $true }
            }
        }
        return $false
    }
    $requiredActions = @(
        'Microsoft.Resources/deployments/write',
        'Microsoft.Resources/subscriptions/resourceGroups/write',
        'Microsoft.Authorization/roleAssignments/write',
        'Microsoft.Authorization/roleDefinitions/write',
        'Microsoft.ManagedIdentity/userAssignedIdentities/federatedIdentityCredentials/write'
    )
    $missingActions = @($requiredActions | Where-Object { -not (Test-ActionAllowed $_) })
    if ($missingActions.Count -gt 0) {
        Stop-Blocked "deployment identity lacks required Azure actions: $($missingActions -join ', ')"
    }

    $usageJson = & az vm list-usage --location $Location --output json --only-show-errors 2>$null
    if ($LASTEXITCODE -ne 0) {
        Stop-Blocked 'regional vCPU quota could not be inspected.'
    }
    $usage = @($usageJson | ConvertFrom-Json)
    $regional = $usage | Where-Object { $_.name.value -eq 'cores' } | Select-Object -First 1
    $requiredCores = 28
    if ($null -eq $regional -or ($regional.limit - $regional.currentValue) -lt $requiredCores) {
        Stop-Blocked "regional vCPU quota has fewer than $requiredCores available cores."
    }

    $selectedSkus = @($SystemNodeVmSize, $KataNodeVmSize) | Sort-Object -Unique
    $familyRequirements = @{}
    foreach ($skuName in $selectedSkus) {
        $skuJson = & az vm list-skus --location $Location --size $skuName --all `
            --query '[0].{name:name,family:family,restrictions:restrictions,capabilities:capabilities}' `
            --output json --only-show-errors 2>$null
        if ($LASTEXITCODE -ne 0 -or [string]::IsNullOrWhiteSpace($skuJson)) {
            Stop-Blocked "VM SKU availability could not be inspected: $skuName"
        }
        $sku = $skuJson | ConvertFrom-Json
        if ($sku.name -ne $skuName -or @($sku.restrictions).Count -gt 0) {
            Stop-Blocked "VM SKU is unavailable for this subscription in ${Location}: $skuName"
        }
        $generation = @($sku.capabilities | Where-Object name -eq 'HyperVGenerations' |
            Select-Object -ExpandProperty value)
        if ($skuName -eq $KataNodeVmSize -and $generation -notmatch 'V2') {
            Stop-Blocked "Kata VM SKU must support Hyper-V generation 2: $skuName"
        }
        $cores = [int](($sku.capabilities | Where-Object name -eq 'vCPUs' |
            Select-Object -First 1).value)
        $nodes = if ($skuName -eq $KataNodeVmSize) { 1 } else { 3 }
        $familyRequirements[$sku.family] = [int]($familyRequirements[$sku.family] + ($cores * $nodes))
    }
    foreach ($family in $familyRequirements.Keys) {
        $familyUsage = $usage | Where-Object { $_.name.value -eq $family } | Select-Object -First 1
        $needed = $familyRequirements[$family]
        if ($null -eq $familyUsage -or ($familyUsage.limit - $familyUsage.currentValue) -lt $needed) {
            Stop-Blocked "$family quota has fewer than $needed available cores."
        }
    }

    if ($ResourceGroupName -and $AksClusterName) {
        $clusterId = & az aks show -g $ResourceGroupName -n $AksClusterName --query id -o tsv --only-show-errors 2>$null
        if ($LASTEXITCODE -eq 0 -and $clusterId) {
            $runRoles = @(& az role assignment list --assignee $DeploymentPrincipalObjectId `
                --scope $clusterId --query '[].roleDefinitionName' -o tsv --only-show-errors 2>$null)
            if ('DevSandbox AKS Run Command' -notin $runRoles -and 'Owner' -notin $roleNames) {
                Stop-Blocked 'deployment identity lacks the scoped DevSandbox AKS Run Command role.'
            }
        }
    }
    if ($ContainerRegistryName) {
        $registryId = & az acr show -n $ContainerRegistryName --query id -o tsv --only-show-errors 2>$null
        if ($LASTEXITCODE -eq 0 -and $registryId) {
            $acrRoles = @(& az role assignment list --assignee $DeploymentPrincipalObjectId `
                --scope $registryId --query '[].roleDefinitionName' -o tsv --only-show-errors 2>$null)
            if ('AcrPush' -notin $acrRoles -and 'Owner' -notin $roleNames) {
                Stop-Blocked 'deployment identity lacks scoped AcrPush.'
            }
        }
    }
}

function Invoke-PlatformRequest {
    param([string]$Session, [string]$Path)
    $headers = @{ Authorization = "Bearer $Session" }
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
    Invoke-RestMethod -Method Get -Uri "$baseURL$Path" -Headers $headers -TimeoutSec 30 `
        -SkipCertificateCheck:$skipTLS
}

function Assert-E2EFixtures {
    try {
        $primary = Invoke-PlatformRequest $env:DEVSANDBOX_TEST_USER_SESSION '/v1/me'
        $secondary = Invoke-PlatformRequest $env:DEVSANDBOX_TEST_SECOND_USER_SESSION '/v1/me'
        $primaryUserId = if ($primary.userId) { $primary.userId } else { $primary.githubUserId }
        $secondaryUserId = if ($secondary.userId) { $secondary.userId } else { $secondary.githubUserId }
        $primaryLogin = if ($primary.login) { $primary.login } else { $primary.githubLogin }
        if ([string]::IsNullOrWhiteSpace($primaryUserId) -or
            [string]::IsNullOrWhiteSpace($secondaryUserId) -or
            $primaryUserId -eq $secondaryUserId) {
            Stop-Blocked 'test sessions do not identify two distinct users.'
        }
        if (-not [string]::Equals($primaryLogin, $env:DEVSANDBOX_TEST_USER, [StringComparison]::OrdinalIgnoreCase)) {
            Stop-Blocked 'DEVSANDBOX_TEST_USER does not match the primary platform session.'
        }
        foreach ($repositoryVariable in @('DEVSANDBOX_TEST_PRIVATE_REPO', 'DEVSANDBOX_TEST_PUBLIC_REPO')) {
            $repository = [Uri]::EscapeDataString([Environment]::GetEnvironmentVariable($repositoryVariable))
            $resolved = Invoke-PlatformRequest $env:DEVSANDBOX_TEST_USER_SESSION "/v1/repositories/resolve?repository=$repository"
            if (-not $resolved.commitSha -or -not $resolved.repositoryUrl) {
                Stop-Blocked "$repositoryVariable could not be resolved to a pinned fixture commit."
            }
        }
    }
    catch {
        Stop-Blocked 'test identities or fixture repositories could not be validated through the deployed API.'
    }
    Write-Host 'Fixture reachability passed; LFS, submodule, push, and PR permissions are exercised by e2e.ps1.'
}

$missingInputs = @(Get-MissingInputs)
if ($missingInputs.Count -gt 0) {
    Stop-Blocked "missing inputs: $($missingInputs -join ', ')"
}

Assert-Tools

if ($Mode -in @('Deployment', 'All')) {
    $hasApp = -not [string]::IsNullOrWhiteSpace($env:DEVSANDBOX_GITHUB_APP_CLIENT_ID) -and
        -not [string]::IsNullOrWhiteSpace($env:DEVSANDBOX_GITHUB_APP_CLIENT_SECRET)
    $hasStatic = -not [string]::IsNullOrWhiteSpace($env:DEVSANDBOX_GITHUB_STATIC_TOKEN) -and
        $env:DEVSANDBOX_GITHUB_STATIC_USER_ID -match '^\d+$' -and
        $env:DEVSANDBOX_GITHUB_STATIC_LOGIN -match '^[A-Za-z0-9-]+$'
    $anyApp = -not [string]::IsNullOrWhiteSpace($env:DEVSANDBOX_GITHUB_APP_CLIENT_ID) -or
        -not [string]::IsNullOrWhiteSpace($env:DEVSANDBOX_GITHUB_APP_CLIENT_SECRET)
    $anyStatic = -not [string]::IsNullOrWhiteSpace($env:DEVSANDBOX_GITHUB_STATIC_TOKEN) -or
        -not [string]::IsNullOrWhiteSpace($env:DEVSANDBOX_GITHUB_STATIC_USER_ID) -or
        -not [string]::IsNullOrWhiteSpace($env:DEVSANDBOX_GITHUB_STATIC_LOGIN)
    if (($anyApp -and -not $hasApp) -or ($anyStatic -and -not $hasStatic) -or ($hasApp -and $hasStatic)) {
        Stop-Blocked 'configure one complete GitHub App or static validation credential set.'
    }
    if ((-not [string]::IsNullOrWhiteSpace($env:DEVSANDBOX_BROKER_TLS_CERTIFICATE) -and
            $env:DEVSANDBOX_BROKER_TLS_CERTIFICATE -notmatch '^-----BEGIN CERTIFICATE-----') -or
        (-not [string]::IsNullOrWhiteSpace($env:DEVSANDBOX_BROKER_TLS_PRIVATE_KEY) -and
            $env:DEVSANDBOX_BROKER_TLS_PRIVATE_KEY -notmatch '^-----BEGIN (?:RSA |EC )?PRIVATE KEY-----')) {
        Stop-Blocked 'broker TLS certificate/private key PEM input is malformed.'
    }
    if (-not $hasApp -and -not $hasStatic) {
        $activeLogin = ((& gh api user --jq '.login' 2>$null) -join '').Trim()
        if ($LASTEXITCODE -ne 0 -or $activeLogin -notmatch '^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$') {
            Stop-Blocked "GitHub CLI authentication is unavailable; run 'gh auth login'."
        }
        & gh auth token --user $activeLogin *> $null
        if ($LASTEXITCODE -ne 0) {
            Stop-Blocked "GitHub CLI authentication is unavailable; run 'gh auth login'."
        }
    }
    if (-not [string]::IsNullOrWhiteSpace($env:DEVSANDBOX_ROUTE_PUBLIC_KEY) -and
        $env:DEVSANDBOX_ROUTE_PUBLIC_KEY -notmatch '^-----BEGIN (?:RSA )?PUBLIC KEY-----') {
        Stop-Blocked 'optional route public key PEM input is malformed.'
    }
}

if ($Mode -in @('E2E', 'All')) {
    if ($env:DEVSANDBOX_API_HOSTNAME -eq $env:DEVSANDBOX_WEB_HOSTNAME -or
        $env:DEVSANDBOX_API_HOSTNAME -notmatch '^[a-z0-9.-]+\.azurefd\.net$' -or
        $env:DEVSANDBOX_WEB_HOSTNAME -notmatch '^[a-z0-9.-]+\.azurefd\.net$') {
        Stop-Blocked 'E2E requires distinct Azure Front Door endpoint hostnames.'
    }
    foreach ($repositoryVariable in @('DEVSANDBOX_TEST_PRIVATE_REPO', 'DEVSANDBOX_TEST_PUBLIC_REPO')) {
        if ([Environment]::GetEnvironmentVariable($repositoryVariable) -notmatch '^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$') {
            Stop-Blocked "$repositoryVariable must use OWNER/REPOSITORY syntax."
        }
    }
}

if ($Mode -in @('Foundation', 'Deployment', 'All') -and -not $SkipAzureChecks) {
    Assert-AzureIdentityAndQuota
}
if ($Mode -in @('E2E', 'All') -and -not $SkipAzureChecks) {
    Assert-E2EFixtures
}

Write-Host "Passed: $Mode preflight"
