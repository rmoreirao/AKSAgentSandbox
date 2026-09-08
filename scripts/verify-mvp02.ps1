[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$ResourceGroupName,
    [Parameter(Mandatory)][string]$AksClusterName,
    [string]$SubscriptionId = $env:AZURE_SUBSCRIPTION_ID,
    [string]$ApiHostname = $env:DEVSANDBOX_API_HOSTNAME,
    [string]$WebHostname = $env:DEVSANDBOX_WEB_HOSTNAME,
    [string]$FrontDoorProfileName = $env:DEVSANDBOX_FRONT_DOOR_PROFILE,
    [string]$FrontDoorProfileResourceId = $env:DEVSANDBOX_FRONT_DOOR_PROFILE_RESOURCE_ID,
    [string]$FrontDoorId = $env:DEVSANDBOX_FRONT_DOOR_ID,
    [string]$FrontDoorApiEndpointName = $env:DEVSANDBOX_FRONT_DOOR_API_ENDPOINT,
    [string]$FrontDoorWebEndpointName = $env:DEVSANDBOX_FRONT_DOOR_WEB_ENDPOINT,
    [string]$ApiOriginHostname = $env:DEVSANDBOX_API_ORIGIN_HOSTNAME,
    [string]$WebOriginHostname = $env:DEVSANDBOX_WEB_ORIGIN_HOSTNAME,
    [string]$TestUserId = $env:DEVSANDBOX_TEST_USER_ID,
    [string]$TestUserLogin = $env:DEVSANDBOX_TEST_USER
)

$ErrorActionPreference = 'Stop'
$PSNativeCommandUseErrorActionPreference = $false

function Stop-Blocked {
    param([string]$Reason)
    Write-Output "Blocked: $Reason"
    exit 2
}

function Invoke-Native {
    param([string]$Command, [string[]]$Arguments)
    & $Command @Arguments
    if ($LASTEXITCODE -ne 0) {
        throw "$Command failed with exit code $LASTEXITCODE"
    }
}

if (-not (Get-Command az -ErrorAction SilentlyContinue)) { Stop-Blocked 'verification requires tool: az' }
$curlCommand = Get-Command curl.exe -ErrorAction SilentlyContinue
if (-not $curlCommand) { $curlCommand = Get-Command curl -CommandType Application -ErrorAction SilentlyContinue }
if (-not $curlCommand) { Stop-Blocked 'verification requires tool: curl' }
$required = @{
    AZURE_SUBSCRIPTION_ID = $SubscriptionId
    DEVSANDBOX_API_HOSTNAME = $ApiHostname
    DEVSANDBOX_WEB_HOSTNAME = $WebHostname
    DEVSANDBOX_FRONT_DOOR_PROFILE = $FrontDoorProfileName
    DEVSANDBOX_FRONT_DOOR_PROFILE_RESOURCE_ID = $FrontDoorProfileResourceId
    DEVSANDBOX_FRONT_DOOR_ID = $FrontDoorId
    DEVSANDBOX_FRONT_DOOR_API_ENDPOINT = $FrontDoorApiEndpointName
    DEVSANDBOX_FRONT_DOOR_WEB_ENDPOINT = $FrontDoorWebEndpointName
    DEVSANDBOX_API_ORIGIN_HOSTNAME = $ApiOriginHostname
    DEVSANDBOX_WEB_ORIGIN_HOSTNAME = $WebOriginHostname
}
$missing = @($required.GetEnumerator() | Where-Object {
    [string]::IsNullOrWhiteSpace([string]$_.Value)
} | ForEach-Object Key)
if ($missing.Count -gt 0) { Stop-Blocked "verification missing inputs: $($missing -join ', ')" }
if ($TestUserId -notmatch '^\d+$' -and (Get-Command gh -ErrorAction SilentlyContinue)) {
    $githubIdentity = ((& gh api user --jq '{id: (.id|tostring), login: .login}' 2>$null) -join '') |
        ConvertFrom-Json
    if ($LASTEXITCODE -eq 0 -and $githubIdentity.id -match '^\d+$' -and
        ($TestUserLogin -eq '' -or [string]::Equals(
            $githubIdentity.login, $TestUserLogin, [StringComparison]::OrdinalIgnoreCase))) {
        $TestUserId = $githubIdentity.id
        if ([string]::IsNullOrWhiteSpace($TestUserLogin)) {
            $TestUserLogin = $githubIdentity.login
        }
    }
}
if ($TestUserId -notmatch '^\d+$' -or $TestUserLogin -notmatch '^[A-Za-z0-9-]+$') {
    Stop-Blocked 'verification requires DEVSANDBOX_TEST_USER_ID and DEVSANDBOX_TEST_USER.'
}
if ($ApiHostname -eq $WebHostname -or $ApiOriginHostname -eq $WebOriginHostname) {
    throw 'Client endpoint domains and internal origin Host headers must each be distinct.'
}

& az account set --subscription $SubscriptionId --only-show-errors
if ($LASTEXITCODE -ne 0) { Stop-Blocked 'Azure authentication or subscription access is unavailable.' }

$cluster = ((& az aks show -g $ResourceGroupName -n $AksClusterName --only-show-errors -o json) -join "`n") |
    ConvertFrom-Json
if ($LASTEXITCODE -ne 0) { throw 'AKS resource could not be read.' }
if (-not $cluster.apiServerAccessProfile.enablePrivateCluster -or
    $cluster.apiServerAccessProfile.enablePrivateClusterPublicFQDN -or
    -not $cluster.oidcIssuerProfile.enabled -or
    -not $cluster.securityProfile.workloadIdentity.enabled -or
    -not $cluster.ingressProfile.webAppRouting.enabled -or
    $null -ne $cluster.ingressProfile.webAppRouting.dnsZoneResourceIds) {
    throw 'AKS private API, identity, Gateway API, or DNS-free Application Routing configuration is incorrect.'
}

$systemPool = ((& az aks nodepool show -g $ResourceGroupName --cluster-name $AksClusterName `
    -n system --only-show-errors -o json) -join "`n") | ConvertFrom-Json
$kataPool = ((& az aks nodepool show -g $ResourceGroupName --cluster-name $AksClusterName `
    -n kata --only-show-errors -o json) -join "`n") | ConvertFrom-Json
if ($LASTEXITCODE -ne 0 -or $systemPool.provisioningState -ne 'Succeeded' -or
    $kataPool.provisioningState -ne 'Succeeded' -or
    $kataPool.workloadRuntime -ne 'KataVmIsolation' -or [int]$kataPool.maxPods -ne 50) {
    throw 'System/Kata pools or the Kata profile are incorrect.'
}

& (Join-Path $PSScriptRoot 'verify-runtimeclass.ps1') `
    -ResourceGroupName $ResourceGroupName -AksClusterName $AksClusterName -SubscriptionId $SubscriptionId
if ($LASTEXITCODE -ne 0) { throw 'RuntimeClass verification failed.' }

$verificationSource = Resolve-Path (Join-Path $PSScriptRoot '..\deploy\bootstrap\verify-platform.sh')
$artifactDirectory = Join-Path $PSScriptRoot '..\artifacts'
New-Item -ItemType Directory -Force -Path $artifactDirectory | Out-Null
$verificationScript = Join-Path $artifactDirectory "verify-platform-$PID.sh"
[IO.File]::WriteAllText(
    $verificationScript,
    ([IO.File]::ReadAllText($verificationSource).Replace("`r`n", "`n")),
    [Text.UTF8Encoding]::new($false)
)
$verificationScriptName = Split-Path -Leaf $verificationScript
$command = ". './$verificationScriptName' '$TestUserId' '$TestUserLogin' '$FrontDoorId'"
try {
    $rawResult = & az aks command invoke -g $ResourceGroupName -n $AksClusterName `
        --file $verificationScript --command $command --only-show-errors --output json
}
finally {
    Remove-Item -LiteralPath $verificationScript -Force -ErrorAction SilentlyContinue
}
if ($LASTEXITCODE -ne 0) { throw 'AKS deployment verification Run Command failed.' }
$result = ($rawResult -join "`n") | ConvertFrom-Json
if ($result.provisioningState -eq 'Running' -and $result.id) {
    $deadline = [DateTime]::UtcNow.AddMinutes(20)
    do {
        Start-Sleep -Seconds 20
        $candidate = (& az aks command result -g $ResourceGroupName -n $AksClusterName `
            --command-id $result.id --only-show-errors --output json 2>$null) -join "`n"
        try {
            $parsed = $candidate | ConvertFrom-Json
            if ($parsed.id) { $result = $parsed }
        }
        catch {}
    } while ($result.provisioningState -eq 'Running' -and [DateTime]::UtcNow -lt $deadline)
}
if ($result.provisioningState -ne 'Succeeded' -or
    ($null -ne $result.exitCode -and [int]$result.exitCode -ne 0)) {
    if (-not [string]::IsNullOrWhiteSpace([string]$result.logs)) {
        Write-Error "AKS verification logs:`n$($result.logs)" -ErrorAction Continue
    }
    throw "AKS deployment verification failed (state=$($result.provisioningState), exit=$($result.exitCode))."
}
$gatewayAddressMatch = [regex]::Match([string]$result.logs, '(?m)^GATEWAY_ADDRESS=(\S+)$')
if (-not $gatewayAddressMatch.Success) { throw 'Gateway did not report a public address.' }
$gatewayAddress = $gatewayAddressMatch.Groups[1].Value

$profile = ((& az resource show --ids $FrontDoorProfileResourceId --api-version 2025-06-01 `
    --only-show-errors -o json) -join "`n") | ConvertFrom-Json
if ($LASTEXITCODE -ne 0 -or $profile.sku.name -ne 'Standard_AzureFrontDoor' -or
    $profile.properties.frontDoorId -ne $FrontDoorId) {
    throw 'Azure Front Door Standard profile or exact profile ID is incorrect.'
}

$apiEndpointId = "$FrontDoorProfileResourceId/afdEndpoints/$FrontDoorApiEndpointName"
$webEndpointId = "$FrontDoorProfileResourceId/afdEndpoints/$FrontDoorWebEndpointName"
$apiEndpoint = ((& az resource show --ids $apiEndpointId --api-version 2025-06-01 `
    --only-show-errors -o json) -join "`n") | ConvertFrom-Json
$webEndpoint = ((& az resource show --ids $webEndpointId --api-version 2025-06-01 `
    --only-show-errors -o json) -join "`n") | ConvertFrom-Json
if ($apiEndpoint.properties.hostName -ne $ApiHostname -or $webEndpoint.properties.hostName -ne $WebHostname) {
    throw 'Front Door endpoint output hostnames do not match the deployed endpoints.'
}

foreach ($routeSpec in @(
    @{ EndpointId = $apiEndpointId; Route = 'api'; Origin = 'devsandbox-api'; Host = $ApiOriginHostname },
    @{ EndpointId = $webEndpointId; Route = 'web'; Origin = 'devsandbox-web'; Host = $WebOriginHostname }
)) {
    $route = ((& az resource show --ids "$($routeSpec.EndpointId)/routes/$($routeSpec.Route)" `
        --api-version 2025-06-01 --only-show-errors -o json) -join "`n") | ConvertFrom-Json
    if (@($route.properties.supportedProtocols).Count -ne 1 -or
        $route.properties.supportedProtocols[0] -ne 'Https' -or
        $route.properties.forwardingProtocol -ne 'HttpOnly' -or
        $null -ne $route.properties.cacheConfiguration) {
        throw "Front Door route '$($routeSpec.Route)' is not HTTPS-only, HTTP-to-origin, and uncached."
    }
    $originId = "$FrontDoorProfileResourceId/originGroups/$($routeSpec.Origin)/origins/aks-gateway"
    $origin = ((& az resource show --ids $originId --api-version 2025-06-01 `
        --only-show-errors -o json) -join "`n") | ConvertFrom-Json
    if ($origin.properties.hostName -ne $gatewayAddress -or
        $origin.properties.originHostHeader -ne $routeSpec.Host -or
        [int]$origin.properties.httpPort -ne 80) {
        throw "Front Door origin '$($routeSpec.Origin)' is not bound to the expected AKS HTTP origin."
    }
}

$diagnosticResult = ((& az monitor diagnostic-settings list --resource $FrontDoorProfileResourceId `
    --only-show-errors -o json) -join "`n") | ConvertFrom-Json
$diagnostics = @($diagnosticResult | Where-Object {
    $_.PSObject.Properties.Name -contains 'workspaceId'
})
if ($diagnostics.Count -eq 0 -and $null -ne $diagnosticResult.value) {
    $diagnostics = @($diagnosticResult.value)
}
if (@($diagnostics | Where-Object {
    $_.workspaceId -and @($_.logs | Where-Object { $_.enabled -and $_.category -eq 'FrontDoorAccessLog' }).Count
}).Count -eq 0) {
    throw 'Front Door access diagnostics are not connected to Log Analytics.'
}

$nsgs = @(((& az network nsg list -g $ResourceGroupName --only-show-errors -o json) -join "`n") |
    ConvertFrom-Json)
$rules = @($nsgs.securityRules)
if (@($rules | Where-Object {
    $_.access -eq 'Allow' -and $_.direction -eq 'Inbound' -and
    $_.sourceAddressPrefix -eq 'AzureFrontDoor.Backend' -and $_.destinationPortRange -eq '80'
}).Count -eq 0 -or
    @($rules | Where-Object {
        $_.access -eq 'Allow' -and $_.direction -eq 'Inbound' -and
        $_.sourceAddressPrefix -eq 'AzureLoadBalancer'
    }).Count -eq 0 -or
    @($rules | Where-Object {
        $_.access -eq 'Allow' -and $_.direction -eq 'Inbound' -and $_.sourceAddressPrefix -eq 'Internet'
    }).Count -ne 0) {
    throw 'AKS subnet NSG is not restricted to AzureFrontDoor.Backend on the origin port.'
}

$directExit = 0
& $curlCommand.Source --noproxy '*' --connect-timeout 8 --max-time 12 -sS `
    -H "Host: $ApiOriginHostname" "http://$gatewayAddress/readyz" *> $null
$directExit = $LASTEXITCODE
if ($directExit -eq 0) {
    throw 'Direct public access to the AKS Gateway origin unexpectedly succeeded.'
}

foreach ($healthUri in @("https://$ApiHostname/readyz", "https://$WebHostname/healthz")) {
    $deadline = [DateTime]::UtcNow.AddMinutes(10)
    do {
        & $curlCommand.Source --noproxy '*' --fail --silent --show-error --max-time 30 $healthUri *> $null
        if ($LASTEXITCODE -eq 0) { break }
        Start-Sleep -Seconds 15
    } while ([DateTime]::UtcNow -lt $deadline)
    if ($LASTEXITCODE -ne 0) { throw "Front Door health request did not succeed: $healthUri" }
}

foreach ($httpUri in @("http://$ApiHostname/readyz", "http://$WebHostname/healthz")) {
    $responseHeaders = (& $curlCommand.Source --noproxy '*' --silent --head --max-time 30 $httpUri) -join "`n"
    if ($responseHeaders -match '(?m)^HTTP/\S+\s+2\d\d\b') {
        throw "Front Door accepted insecure client traffic: $httpUri"
    }
}

& (Join-Path $PSScriptRoot 'security-validate.ps1') -Mode Deployed `
    -ResourceGroupName $ResourceGroupName -AksClusterName $AksClusterName
if ($LASTEXITCODE -ne 0) { throw 'Deployed security validation failed.' }

Write-Host 'Passed: private AKS, Front Door isolation, HTTPS-only uncached routes, HTTP origin, identities, placement, health, and security.'
