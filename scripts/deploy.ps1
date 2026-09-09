[CmdletBinding()]
param(
    [ValidateSet('Preflight', 'Validate', 'WhatIf', 'Foundation', 'Bootstrap', 'Images', 'Kubernetes', 'FrontDoor', 'Verify', 'All')]
    [string]$Stage = 'Preflight',

    [string]$SubscriptionId = $env:AZURE_SUBSCRIPTION_ID,
    [string]$Location = $env:AZURE_LOCATION,
    [string]$ResourcePrefix = $env:DEVSANDBOX_RESOURCE_PREFIX,
    [string]$DeploymentPrincipalObjectId = $env:AZURE_DEPLOYMENT_PRINCIPAL_ID,
    [string]$ResourceGroupName = $env:DEVSANDBOX_RESOURCE_GROUP,
    [string]$AksClusterName = $env:DEVSANDBOX_AKS_CLUSTER,
    [string]$ContainerRegistryName = $env:DEVSANDBOX_CONTAINER_REGISTRY,
    [string]$ApiIdentityClientId = $env:DEVSANDBOX_API_IDENTITY_CLIENT_ID,
    [string]$BrokerIdentityClientId = $env:DEVSANDBOX_BROKER_IDENTITY_CLIENT_ID,
    [string]$BootstrapIdentityClientId = $env:DEVSANDBOX_BOOTSTRAP_IDENTITY_CLIENT_ID,
    [string]$KeyVaultUri = $env:DEVSANDBOX_KEY_VAULT_URI,
    [string]$ApiHostname = $env:DEVSANDBOX_API_HOSTNAME,
    [string]$WebHostname = $env:DEVSANDBOX_WEB_HOSTNAME,
    [string]$FrontDoorProfileName = $env:DEVSANDBOX_FRONT_DOOR_PROFILE,
    [string]$FrontDoorProfileResourceId = $env:DEVSANDBOX_FRONT_DOOR_PROFILE_RESOURCE_ID,
    [string]$FrontDoorId = $env:DEVSANDBOX_FRONT_DOOR_ID,
    [string]$FrontDoorApiEndpointName = $env:DEVSANDBOX_FRONT_DOOR_API_ENDPOINT,
    [string]$FrontDoorWebEndpointName = $env:DEVSANDBOX_FRONT_DOOR_WEB_ENDPOINT,
    [string]$ApiOriginHostname = $env:DEVSANDBOX_API_ORIGIN_HOSTNAME,
    [string]$WebOriginHostname = $env:DEVSANDBOX_WEB_ORIGIN_HOSTNAME,
    [switch]$SkipLocalConfiguration,
    [string]$ParametersFile = (Join-Path $PSScriptRoot '..\infra\environments\poc.bicepparam')
)

$ErrorActionPreference = 'Stop'
$PSNativeCommandUseErrorActionPreference = $true

function Invoke-Native {
    param(
        [Parameter(Mandatory)]
        [string]$FilePath,

        [Parameter(Mandatory)]
        [string[]]$ArgumentList,

        [switch]$DiscardOutput
    )

    if ($DiscardOutput) {
        & $FilePath @ArgumentList | Out-Null
    }
    else {
        & $FilePath @ArgumentList
    }

    if ($LASTEXITCODE -ne 0) {
        throw "$FilePath failed with exit code $LASTEXITCODE"
    }
}

function Test-AzureAuthentication {
    try {
        $accountId = & az account show --only-show-errors --query id --output tsv 2>$null
        return $LASTEXITCODE -eq 0 -and -not [string]::IsNullOrWhiteSpace($accountId)
    }
    catch {
        return $false
    }
}

function Get-MissingCloudInputs {
    $missing = [System.Collections.Generic.List[string]]::new()
    if ([string]::IsNullOrWhiteSpace($SubscriptionId)) {
        $missing.Add('AZURE_SUBSCRIPTION_ID')
    }
    if ([string]::IsNullOrWhiteSpace($Location)) {
        $missing.Add('AZURE_LOCATION')
    }
    if ([string]::IsNullOrWhiteSpace($ResourcePrefix)) {
        $missing.Add('DEVSANDBOX_RESOURCE_PREFIX')
    }
    if ([string]::IsNullOrWhiteSpace($DeploymentPrincipalObjectId)) {
        $missing.Add('AZURE_DEPLOYMENT_PRINCIPAL_ID')
    }
    return $missing
}

function Invoke-StaticValidation {
    Write-Host 'Validating Bicep templates...'
    Invoke-Native az @('bicep', 'build', '--file', '.\infra\foundation.bicep', '--stdout') -DiscardOutput
    Invoke-Native az @('bicep', 'build', '--file', '.\infra\main.bicep', '--stdout') -DiscardOutput
    Invoke-Native az @('bicep', 'build', '--file', '.\infra\front-door-binding.bicep', '--stdout') -DiscardOutput

    if (-not (Get-Command kubectl -ErrorAction SilentlyContinue)) {
        throw 'Kustomize validation Blocked: missing tool: kubectl'
    }
    Write-Host 'Rendering the PoC Kubernetes overlay...'
    Invoke-Native kubectl @('kustomize', '.\deploy\kustomize\overlays\poc') -DiscardOutput

    $pinPath = '.\deploy\kustomize\base\upstream\agent-sandbox\v1.0.0\pin.json'
    $manifestPath = '.\deploy\kustomize\base\upstream\agent-sandbox\v1.0.0\sandbox.yaml'
    $pin = Get-Content -Raw -LiteralPath $pinPath | ConvertFrom-Json
    $actualHash = (Get-FileHash -Algorithm SHA256 -LiteralPath $manifestPath).Hash.ToLowerInvariant()
    if ($actualHash -ne $pin.releaseAsset.sha256) {
        throw "Pinned Agent Sandbox manifest checksum mismatch: $actualHash"
    }

    Write-Host 'Bicep, Kustomize, and upstream pin validation succeeded.'
}

function Assert-AzureContext {
    if (-not (Get-Command az -ErrorAction SilentlyContinue)) {
        throw 'Cloud stage Blocked: missing tool: az'
    }
    if ([string]::IsNullOrWhiteSpace($SubscriptionId)) {
        throw 'Cloud stage Blocked: missing input: AZURE_SUBSCRIPTION_ID'
    }
    if (-not (Test-AzureAuthentication)) {
        throw 'Cloud stage Blocked: Azure authentication is unavailable. Run az login.'
    }

    Invoke-Native az @('account', 'set', '--subscription', $SubscriptionId) -DiscardOutput
}

function Get-FoundationOutputs {
    if ([string]::IsNullOrWhiteSpace($ResourcePrefix)) {
        throw 'Post-foundation stage Blocked: DEVSANDBOX_RESOURCE_PREFIX is required to find the foundation deployment.'
    }

    $deploymentName = "devsandbox-$($ResourcePrefix.ToLowerInvariant())-foundation"
    $json = & az deployment sub show `
        --name $deploymentName `
        --subscription $SubscriptionId `
        --query properties.outputs `
        --only-show-errors `
        --output json
    if ($LASTEXITCODE -ne 0) {
        throw "Post-foundation stage Blocked: unable to read outputs from deployment '$deploymentName'."
    }

    $rawOutputs = $json | ConvertFrom-Json
    $outputs = @{}
    foreach ($property in $rawOutputs.PSObject.Properties) {
        $outputs[$property.Name] = [string]$property.Value.value
    }
    return $outputs
}

function Ensure-ContainerInsightsCollection {
    $outputs = Get-FoundationOutputs
    $clusterId = $outputs.aksClusterResourceId
    $workspaceId = $outputs.logAnalyticsWorkspaceId
    if ([string]::IsNullOrWhiteSpace($clusterId) -or [string]::IsNullOrWhiteSpace($workspaceId)) {
        throw 'Container Insights bootstrap failed: foundation outputs are incomplete.'
    }
    $association = (& az monitor data-collection rule association list --resource $clusterId `
        --query "[?name=='ContainerInsightsExtension'].name | [0]" --output tsv --only-show-errors) -join ''
    if ($LASTEXITCODE -ne 0) {
        throw 'Container Insights DCR association could not be inspected.'
    }
    if ($association -eq 'ContainerInsightsExtension') {
        Write-Host 'Container Insights DCR association is ready.'
        return
    }
    & az aks disable-addons --resource-group $outputs.resourceGroupName --name $outputs.aksClusterName `
        --addons monitoring --only-show-errors | Out-Null
    if ($LASTEXITCODE -ne 0) {
        throw 'Container Insights add-on reset failed.'
    }
    & az aks enable-addons --resource-group $outputs.resourceGroupName --name $outputs.aksClusterName `
        --addons monitoring --workspace-resource-id $workspaceId `
        --enable-msi-auth-for-monitoring true --only-show-errors | Out-Null
    if ($LASTEXITCODE -ne 0) {
        throw 'Container Insights managed-identity onboarding failed.'
    }
    Write-Host 'Container Insights DCR association created.'
}

function Remove-LegacyAppRoutingDnsZones {
    $outputs = Get-FoundationOutputs
    $zoneIds = @(& az aks show --resource-group $outputs.resourceGroupName `
        --name $outputs.aksClusterName `
        --query 'ingressProfile.webAppRouting.dnsZoneResourceIds[]' --output tsv --only-show-errors)
    if ($LASTEXITCODE -ne 0) {
        throw 'Legacy Application Routing DNS integration could not be inspected.'
    }
    $zoneIds = @($zoneIds | Where-Object { -not [string]::IsNullOrWhiteSpace($_) })
    if ($zoneIds.Count -eq 0) {
        Write-Host 'Application Routing has no legacy Azure DNS zone attachments.'
        return
    }
    & az aks approuting zone delete --resource-group $outputs.resourceGroupName `
        --name $outputs.aksClusterName --ids ($zoneIds -join ',') --yes --only-show-errors --output none
    if ($LASTEXITCODE -ne 0) {
        throw 'Legacy Application Routing DNS zone detach failed.'
    }
    Write-Host 'Detached legacy Azure DNS zones from Application Routing.'
}

function Ensure-RouteSigningKey {
    $outputs = Get-FoundationOutputs
    $vaultName = $outputs.keyVaultName
    $resourceGroup = $outputs.resourceGroupName
    $clusterName = $outputs.aksClusterName
    $bootstrapClientId = $outputs.bootstrapIdentityClientId
    $tenantId = (& az account show --query tenantId --output tsv --only-show-errors).Trim()
    if ([string]::IsNullOrWhiteSpace($vaultName) -or
        [string]::IsNullOrWhiteSpace($resourceGroup) -or [string]::IsNullOrWhiteSpace($clusterName) -or
        [string]::IsNullOrWhiteSpace($bootstrapClientId) -or [string]::IsNullOrWhiteSpace($tenantId)) {
        throw 'Route-signing key bootstrap failed: foundation outputs are incomplete.'
    }

    $artifactDirectory = Join-Path $PSScriptRoot '..\artifacts'
    New-Item -ItemType Directory -Force -Path $artifactDirectory | Out-Null
    $manifestPath = Join-Path $artifactDirectory "devsandbox-keyvault-bootstrap-$PID.yaml"
    try {
        $templatePath = Join-Path $PSScriptRoot '..\deploy\bootstrap\keyvault-bootstrap.yaml'
        $manifest = (Get-Content -Raw -LiteralPath $templatePath).
            Replace('REPLACE_BOOTSTRAP_CLIENT_ID', $bootstrapClientId).
            Replace('REPLACE_AZURE_TENANT_ID', $tenantId).
            Replace('REPLACE_KEY_VAULT_NAME', $vaultName)
        [IO.File]::WriteAllText($manifestPath, $manifest, [Text.UTF8Encoding]::new($false))
        $manifestName = Split-Path -Leaf $manifestPath
        $command = "set -eu; kubectl delete job devsandbox-keyvault-bootstrap -n devsandbox-system --ignore-not-found >/dev/null; " +
            "kubectl apply -f '$manifestName' >/dev/null; " +
            "if ! kubectl wait --for=condition=complete job/devsandbox-keyvault-bootstrap -n devsandbox-system --timeout=600s >/dev/null; " +
            "then kubectl logs job/devsandbox-keyvault-bootstrap -n devsandbox-system --all-containers=true || true; " +
            "kubectl describe job devsandbox-keyvault-bootstrap -n devsandbox-system; exit 1; fi; " +
            "kubectl logs job/devsandbox-keyvault-bootstrap -n devsandbox-system --all-containers=true"
        $logs = (& az aks command invoke --resource-group $resourceGroup --name $clusterName `
            --file $manifestPath --command $command --query logs --only-show-errors --output tsv) -join "`n"
        if ($LASTEXITCODE -ne 0) {
            throw 'Key Vault bootstrap AKS Run Command failed.'
        }
        if ($logs -notmatch '(?s)BOOTSTRAP_OK.*?PUBLIC_KEY_BEGIN\s+(?<key>-----BEGIN PUBLIC KEY-----.*?-----END PUBLIC KEY-----)\s+PUBLIC_KEY_END') {
            if (-not [string]::IsNullOrWhiteSpace($logs)) {
                Write-Error "Key Vault bootstrap logs:`n$logs" -ErrorAction Continue
            }
            throw 'Key Vault bootstrap failed; the in-cluster job did not return its completion marker.'
        }
        $script:GeneratedRoutePublicKey = $Matches.key.Trim()
        Write-Host 'Route-signing public key is ready through private Key Vault access.'
    }
    finally {
        Remove-Item -LiteralPath $manifestPath -Force -ErrorAction SilentlyContinue
    }
}

function Resolve-PostFoundationConfiguration {
    param(
        [switch]$ImagesOnly,
        [switch]$FrontDoorOnly,
        [switch]$VerificationOnly
    )

    Assert-AzureContext
    $outputs = Get-FoundationOutputs
    if ([string]::IsNullOrWhiteSpace($ResourceGroupName)) { $script:ResourceGroupName = $outputs.resourceGroupName }
    if ([string]::IsNullOrWhiteSpace($AksClusterName)) { $script:AksClusterName = $outputs.aksClusterName }
    if ([string]::IsNullOrWhiteSpace($ContainerRegistryName)) { $script:ContainerRegistryName = $outputs.containerRegistryName }
    if ([string]::IsNullOrWhiteSpace($ApiIdentityClientId)) { $script:ApiIdentityClientId = $outputs.apiIdentityClientId }
    if ([string]::IsNullOrWhiteSpace($BrokerIdentityClientId)) { $script:BrokerIdentityClientId = $outputs.brokerIdentityClientId }
    if ([string]::IsNullOrWhiteSpace($BootstrapIdentityClientId)) { $script:BootstrapIdentityClientId = $outputs.bootstrapIdentityClientId }
    if ([string]::IsNullOrWhiteSpace($KeyVaultUri)) { $script:KeyVaultUri = $outputs.keyVaultUri }
    if ([string]::IsNullOrWhiteSpace($FrontDoorProfileName)) { $script:FrontDoorProfileName = $outputs.frontDoorProfileName }
    if ([string]::IsNullOrWhiteSpace($FrontDoorProfileResourceId)) { $script:FrontDoorProfileResourceId = $outputs.frontDoorProfileResourceId }
    if ([string]::IsNullOrWhiteSpace($FrontDoorId)) { $script:FrontDoorId = $outputs.frontDoorProfileId }
    if ([string]::IsNullOrWhiteSpace($FrontDoorApiEndpointName)) { $script:FrontDoorApiEndpointName = $outputs.frontDoorApiEndpointName }
    if ([string]::IsNullOrWhiteSpace($FrontDoorWebEndpointName)) { $script:FrontDoorWebEndpointName = $outputs.frontDoorWebEndpointName }
    if ([string]::IsNullOrWhiteSpace($ApiHostname)) { $script:ApiHostname = $outputs.frontDoorApiHostname }
    if ([string]::IsNullOrWhiteSpace($WebHostname)) { $script:WebHostname = $outputs.frontDoorWebHostname }
    if ([string]::IsNullOrWhiteSpace($ApiOriginHostname)) { $script:ApiOriginHostname = $outputs.apiOriginHostname }
    if ([string]::IsNullOrWhiteSpace($WebOriginHostname)) { $script:WebOriginHostname = $outputs.webOriginHostname }

    $required = @{}
    if ($ImagesOnly) {
        $required.ContainerRegistryName = $ContainerRegistryName
    }
    elseif ($FrontDoorOnly) {
        $required.ResourceGroupName = $ResourceGroupName
        $required.AksClusterName = $AksClusterName
        $required.FrontDoorProfileName = $FrontDoorProfileName
        $required.FrontDoorApiEndpointName = $FrontDoorApiEndpointName
        $required.FrontDoorWebEndpointName = $FrontDoorWebEndpointName
        $required.ApiOriginHostname = $ApiOriginHostname
        $required.WebOriginHostname = $WebOriginHostname
    }
    elseif ($VerificationOnly) {
        $required.ResourceGroupName = $ResourceGroupName
        $required.AksClusterName = $AksClusterName
        $required.FrontDoorProfileName = $FrontDoorProfileName
        $required.FrontDoorProfileResourceId = $FrontDoorProfileResourceId
        $required.FrontDoorId = $FrontDoorId
        $required.FrontDoorApiEndpointName = $FrontDoorApiEndpointName
        $required.FrontDoorWebEndpointName = $FrontDoorWebEndpointName
        $required.ApiHostname = $ApiHostname
        $required.WebHostname = $WebHostname
        $required.ApiOriginHostname = $ApiOriginHostname
        $required.WebOriginHostname = $WebOriginHostname
    }
    else {
        $required.ResourceGroupName = $ResourceGroupName
        $required.AksClusterName = $AksClusterName
        $required.ContainerRegistryName = $ContainerRegistryName
        $required.ApiIdentityClientId = $ApiIdentityClientId
        $required.BrokerIdentityClientId = $BrokerIdentityClientId
        $required.BootstrapIdentityClientId = $BootstrapIdentityClientId
        $required.KeyVaultUri = $KeyVaultUri
        $required.FrontDoorProfileName = $FrontDoorProfileName
        $required.FrontDoorId = $FrontDoorId
        $required.FrontDoorApiEndpointName = $FrontDoorApiEndpointName
        $required.FrontDoorWebEndpointName = $FrontDoorWebEndpointName
        $required.ApiHostname = $ApiHostname
        $required.WebHostname = $WebHostname
        $required.ApiOriginHostname = $ApiOriginHostname
        $required.WebOriginHostname = $WebOriginHostname
    }
    $missing = @($required.GetEnumerator() | Where-Object {
        [string]::IsNullOrWhiteSpace([string]$_.Value)
    } | ForEach-Object Key)
    if ($missing.Count -gt 0) {
        throw "Post-foundation stage Blocked: missing resolved values: $($missing -join ', ')"
    }

    if (-not $ImagesOnly -and -not $FrontDoorOnly -and -not $VerificationOnly) {
        if ([string]::IsNullOrWhiteSpace($env:DEVSANDBOX_ROUTE_PUBLIC_KEY) -and
            [string]::IsNullOrWhiteSpace($GeneratedRoutePublicKey)) {
            Ensure-RouteSigningKey
        }
    }
}

function Build-Images {
    $metadata = Get-Content -Raw -LiteralPath (Join-Path $PSScriptRoot '..\images\versions.json') |
        ConvertFrom-Json
    $version = $metadata.templateVersion
    $imageNames = @('standard', 'vscode', 'copilot',
        'devsandbox-api', 'devsandbox-broker', 'devsandbox-operator', 'devsandbox-web', 'sandbox-router')
    $imageVersions = @{}
    foreach ($imageName in $imageNames) {
        $specificVersion = $metadata.templateVersions.PSObject.Properties[$imageName].Value
        $imageVersions[$imageName] = if ([string]::IsNullOrWhiteSpace([string]$specificVersion)) {
            $version
        }
        else {
            [string]$specificVersion
        }
    }
    $published = @()
    foreach ($imageName in $imageNames) {
        try {
            $digest = (& az acr repository show --name $ContainerRegistryName `
                --image "devsandbox/${imageName}:$($imageVersions[$imageName])" --query digest `
                --output tsv --only-show-errors 2>$null) -join ''
            if ($LASTEXITCODE -eq 0 -and $digest -match '^sha256:[0-9a-f]{64}$') {
                $published += $imageName
            }
        }
        catch {}
    }
    if ($published.Count -eq $imageNames.Count) {
        Write-Host 'All immutable image releases are already published; skipping rebuild.'
        return
    }
    $missingImages = @($imageNames | Where-Object { $_ -notin $published })
    Write-Host "Publishing missing immutable images: $($missingImages -join ', ')"

    $loginServer = & az acr show `
        --name $ContainerRegistryName `
        --query loginServer `
        --only-show-errors `
        --output tsv
    if ($LASTEXITCODE -ne 0 -or [string]::IsNullOrWhiteSpace($loginServer)) {
        throw "Image build Blocked: unable to resolve ACR login server for '$ContainerRegistryName'."
    }
    $dockerReady = $false
    if (Get-Command docker -ErrorAction SilentlyContinue) {
        try {
            $dockerOutput = (& docker info --format '{{.ServerVersion}}' 2>&1) -join "`n"
            $dockerReady = $LASTEXITCODE -eq 0 -and $dockerOutput -match '\d+\.\d+'
        }
        catch {
            $dockerReady = $false
        }
    }
    if ($dockerReady -and $missingImages.Count -eq $imageNames.Count) {
        Invoke-Native az @('acr', 'login', '--name', $ContainerRegistryName, '--only-show-errors') -DiscardOutput
        & (Join-Path $PSScriptRoot 'build-images.ps1') `
            -Action Push `
            -Registry $loginServer `
            -OutputPath (Join-Path $PSScriptRoot '..\artifacts\image-digests.json')
        if ($LASTEXITCODE -ne 0) {
            throw "Image build and push failed with exit code $LASTEXITCODE."
        }
        return
    }

    Write-Host 'Publishing missing images with ACR cloud builds.'
    $revision = (& git rev-parse HEAD).Trim()
    $buildDate = [DateTime]::UtcNow.ToString('yyyy-MM-ddTHH:mm:ssZ')
    $common = @(
        'acr', 'build', '--registry', $ContainerRegistryName, '--platform', 'linux/amd64',
        '--no-logs',
        '--build-arg', 'TARGETOS=linux', '--build-arg', 'TARGETARCH=amd64',
        '--build-arg', "BUILD_DATE=$buildDate", '--build-arg', "REVISION=$revision",
        '--build-arg', "TEMPLATE_VERSION=$version", '--only-show-errors'
    )
    foreach ($image in @(
        @{ Name = 'standard'; Dockerfile = 'images\standard\Dockerfile' },
        @{ Name = 'vscode'; Dockerfile = 'images\vscode\Dockerfile' },
        @{ Name = 'copilot'; Dockerfile = 'images\copilot\Dockerfile' }
    )) {
        if ($image.Name -notin $missingImages) {
            continue
        }
        $imageVersion = $imageVersions[$image.Name]
        Invoke-Native az ($common + @(
            '--build-arg', "TEMPLATE_VERSION=$imageVersion",
            '--image', "devsandbox/$($image.Name):$imageVersion",
            '--file', $image.Dockerfile, '.'
        ))
    }
    foreach ($binary in @('devsandbox-api', 'devsandbox-broker', 'devsandbox-operator', 'devsandbox-web')) {
        if ($binary -notin $missingImages) {
            continue
        }
        $imageVersion = $imageVersions[$binary]
        Invoke-Native az ($common + @(
            '--build-arg', "BINARY=$binary", '--build-arg', "IMAGE_VERSION=$imageVersion",
            '--image', "devsandbox/${binary}:$imageVersion",
            '--file', 'images\management\Dockerfile', '.'
        ))
    }

    if ('sandbox-router' -notin $missingImages) {
        return
    }
    $routerVersion = $imageVersions['sandbox-router']
    $routerSource = Join-Path $PSScriptRoot '..\artifacts\agent-sandbox-router-source'
    Remove-Item -LiteralPath $routerSource -Recurse -Force -ErrorAction SilentlyContinue
    try {
        Invoke-Native git @('clone', '--quiet', '--filter=blob:none', '--no-checkout',
            'https://github.com/kubernetes-sigs/agent-sandbox.git', $routerSource)
        Invoke-Native git @('-C', $routerSource, 'checkout', '--quiet', '--detach',
            [string]$metadata.tools.sandboxRouter.sourceCommit)
        Copy-Item (Join-Path $PSScriptRoot '..\deploy\router\Dockerfile') (Join-Path $routerSource 'Dockerfile')
        Invoke-Native az @(
            'acr', 'build', '--registry', $ContainerRegistryName, '--platform', 'linux/amd64',
            '--no-logs',
            '--build-arg', 'TARGETARCH=amd64',
            '--build-arg', "GIT_SHA=$($metadata.tools.sandboxRouter.sourceCommit)",
            '--build-arg', "BUILD_DATE=$buildDate",
            '--image', "devsandbox/sandbox-router:$routerVersion",
            '--file', (Join-Path $routerSource 'Dockerfile'), '--only-show-errors', $routerSource
        )
    }
    finally {
        Remove-Item -LiteralPath $routerSource -Recurse -Force -ErrorAction SilentlyContinue
    }
}

function Get-PublishedImageReference {
    param([Parameter(Mandatory)][string]$Name)

    $versions = Get-Content -Raw -LiteralPath (Join-Path $PSScriptRoot '..\images\versions.json') |
        ConvertFrom-Json
    $specificVersion = $versions.templateVersions.PSObject.Properties[$Name].Value
    $version = if ([string]::IsNullOrWhiteSpace([string]$specificVersion)) {
        [string]$versions.templateVersion
    }
    else {
        [string]$specificVersion
    }
    $imageRepository = "devsandbox/$Name"
    $digest = & az acr repository show `
        --name $ContainerRegistryName `
        --image "${imageRepository}:$version" `
        --query digest `
        --only-show-errors `
        --output tsv
    if ($LASTEXITCODE -ne 0 -or $digest -notmatch '^sha256:[0-9a-f]{64}$') {
        throw "Image deployment Blocked: no valid digest found for ${imageRepository}:$version."
    }

    $loginServer = & az acr show `
        --name $ContainerRegistryName `
        --query loginServer `
        --only-show-errors `
        --output tsv
    if ($LASTEXITCODE -ne 0 -or [string]::IsNullOrWhiteSpace($loginServer)) {
        throw "Image deployment Blocked: unable to resolve ACR login server for '$ContainerRegistryName'."
    }
    return "$loginServer/$imageRepository@$digest"
}

function Resolve-ValidationCredentials {
    $hasApp = -not [string]::IsNullOrWhiteSpace($env:DEVSANDBOX_GITHUB_APP_CLIENT_ID) -and
        -not [string]::IsNullOrWhiteSpace($env:DEVSANDBOX_GITHUB_APP_CLIENT_SECRET)
    $hasStatic = -not [string]::IsNullOrWhiteSpace($env:DEVSANDBOX_GITHUB_STATIC_TOKEN) -and
        -not [string]::IsNullOrWhiteSpace($env:DEVSANDBOX_GITHUB_STATIC_USER_ID) -and
        -not [string]::IsNullOrWhiteSpace($env:DEVSANDBOX_GITHUB_STATIC_LOGIN)
    $anyApp = -not [string]::IsNullOrWhiteSpace($env:DEVSANDBOX_GITHUB_APP_CLIENT_ID) -or
        -not [string]::IsNullOrWhiteSpace($env:DEVSANDBOX_GITHUB_APP_CLIENT_SECRET)
    $anyStatic = -not [string]::IsNullOrWhiteSpace($env:DEVSANDBOX_GITHUB_STATIC_TOKEN) -or
        -not [string]::IsNullOrWhiteSpace($env:DEVSANDBOX_GITHUB_STATIC_USER_ID) -or
        -not [string]::IsNullOrWhiteSpace($env:DEVSANDBOX_GITHUB_STATIC_LOGIN)
    if (($anyApp -and -not $hasApp) -or ($anyStatic -and -not $hasStatic)) {
        throw 'Kubernetes stage Blocked: GitHub App and static validation credentials must each be complete sets.'
    }
    if ($hasApp -and $hasStatic) {
        throw 'Kubernetes stage Blocked: configure either GitHub App or static validation credentials, not both.'
    }
    if (-not $hasApp -and -not $hasStatic) {
        if (-not (Get-Command gh -ErrorAction SilentlyContinue)) {
            throw 'Kubernetes stage Blocked: GitHub App credentials or authenticated gh CLI are required.'
        }
        $userJson = ((& gh api user --jq '{id: (.id|tostring), login: .login}' 2>$null) -join '') |
            ConvertFrom-Json
        if ($LASTEXITCODE -ne 0 -or $userJson.id -notmatch '^\d+$' -or
            $userJson.login -notmatch '^[A-Za-z0-9-]+$') {
            throw 'Kubernetes stage Blocked: authenticated gh CLI credentials could not be resolved.'
        }
        $token = (& gh auth token --user $userJson.login 2>$null) -join ''
        if ($LASTEXITCODE -ne 0 -or [string]::IsNullOrWhiteSpace($token)) {
            throw "Kubernetes stage Blocked: gh has no stored account credential for $($userJson.login). Run 'gh auth login'."
        }
        $env:DEVSANDBOX_GITHUB_STATIC_TOKEN = $token.Trim()
        $env:DEVSANDBOX_GITHUB_STATIC_USER_ID = $userJson.id.Trim()
        $env:DEVSANDBOX_GITHUB_STATIC_LOGIN = $userJson.login.Trim()
        $env:DEVSANDBOX_GITHUB_APP_CLIENT_ID = 'static-validation'
        $env:DEVSANDBOX_GITHUB_APP_CLIENT_SECRET = ''
        $script:GitHubAuthMode = 'static-validation'
        Write-Host 'Using owner-bound static GitHub credential for the local PoC profile.'
    }
    elseif ($hasStatic) {
        $env:DEVSANDBOX_GITHUB_APP_CLIENT_ID = 'static-validation'
        $env:DEVSANDBOX_GITHUB_APP_CLIENT_SECRET = ''
        $script:GitHubAuthMode = 'static-validation'
    }
    else {
        $script:GitHubAuthMode = 'github-app'
    }

    if ([string]::IsNullOrWhiteSpace($env:DEVSANDBOX_BROKER_TLS_CERTIFICATE) -or
        [string]::IsNullOrWhiteSpace($env:DEVSANDBOX_BROKER_TLS_PRIVATE_KEY)) {
        if (-not (Get-Command openssl -ErrorAction SilentlyContinue)) {
            throw 'Kubernetes stage Blocked: openssl is required to generate internal broker TLS.'
        }
        $artifactDirectory = Join-Path $PSScriptRoot '..\artifacts'
        New-Item -ItemType Directory -Force -Path $artifactDirectory | Out-Null
        $certificatePath = Join-Path $artifactDirectory "devsandbox-broker-$PID.crt"
        $keyPath = Join-Path $artifactDirectory "devsandbox-broker-$PID.key"
        try {
            & openssl req -x509 -newkey rsa:2048 -sha256 -nodes -days 365 `
                -subj '/CN=devsandbox-broker.devsandbox-system.svc.cluster.local' `
                -addext 'subjectAltName=DNS:devsandbox-broker.devsandbox-system.svc.cluster.local,DNS:devsandbox-broker' `
                -keyout $keyPath -out $certificatePath 2>$null
            if ($LASTEXITCODE -ne 0) {
                throw 'Internal broker TLS generation failed.'
            }
            $env:DEVSANDBOX_BROKER_TLS_CERTIFICATE = Get-Content -Raw -LiteralPath $certificatePath
            $env:DEVSANDBOX_BROKER_TLS_PRIVATE_KEY = Get-Content -Raw -LiteralPath $keyPath
        }
        finally {
            Remove-Item -LiteralPath $certificatePath, $keyPath -Force -ErrorAction SilentlyContinue
        }
        Write-Host 'Generated ephemeral internal broker TLS material.'
    }
}

function Deploy-KubernetesResources {
    if (-not (Get-Command kubectl -ErrorAction SilentlyContinue)) {
        throw 'Kubernetes stage Blocked: missing tool: kubectl'
    }
    if ($ApiOriginHostname -notmatch '^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$' -or
        $WebOriginHostname -notmatch '^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$' -or
        $ApiOriginHostname -eq $WebOriginHostname) {
        throw 'Kubernetes stage Blocked: API and web origin Host headers must be distinct lowercase DNS names.'
    }
    if ($FrontDoorId -notmatch '^[0-9a-fA-F-]{36}$') {
        throw 'Kubernetes stage Blocked: Azure Front Door profile ID is invalid.'
    }
    Resolve-ValidationCredentials
    $secretInputs = @('DEVSANDBOX_GITHUB_APP_CLIENT_ID',
        'DEVSANDBOX_BROKER_TLS_CERTIFICATE', 'DEVSANDBOX_BROKER_TLS_PRIVATE_KEY')
    $missingSecretInputs = @($secretInputs | Where-Object {
        [string]::IsNullOrWhiteSpace([Environment]::GetEnvironmentVariable($_))
    })
    if ($missingSecretInputs.Count -gt 0) {
        throw "Kubernetes stage Blocked: missing inputs: $($missingSecretInputs -join ', ')"
    }

    $routerImage = Get-PublishedImageReference -Name sandbox-router
    $operatorImage = Get-PublishedImageReference -Name devsandbox-operator
    $managementImages = @{
        'devsandbox-api' = Get-PublishedImageReference -Name devsandbox-api
        'devsandbox-web' = Get-PublishedImageReference -Name devsandbox-web
        'devsandbox-broker' = Get-PublishedImageReference -Name devsandbox-broker
    }
    $templateImages = @{
        standard = Get-PublishedImageReference -Name standard
        vscode = Get-PublishedImageReference -Name vscode
        copilot = Get-PublishedImageReference -Name copilot
    }
    $rendered = & kubectl kustomize '.\deploy\kustomize\overlays\poc'
    if ($LASTEXITCODE -ne 0) {
        throw "kubectl kustomize failed with exit code $LASTEXITCODE"
    }
    $manifest = $rendered -join "`n"
    $manifest = $manifest.Replace('api-origin.example.internal', $ApiOriginHostname)
    $manifest = $manifest.Replace('web-origin.example.internal', $WebOriginHostname)
    $manifest = $manifest.Replace('https://web-endpoint.azurefd.net', "https://$WebHostname")
    $manifest = $manifest.Replace('value: 00000000-0000-0000-0000-000000000000', "value: $FrontDoorId")
    $manifest = $manifest.Replace('azure.workload.identity/client-id: 00000000-0000-0000-0000-000000000001', "azure.workload.identity/client-id: $ApiIdentityClientId")
    $manifest = $manifest.Replace('azure.workload.identity/client-id: 00000000-0000-0000-0000-000000000002', "azure.workload.identity/client-id: $BrokerIdentityClientId")
    $manifest = $manifest.Replace('example.invalid/devsandbox/sandbox-router@sha256:1111111111111111111111111111111111111111111111111111111111111111', $routerImage)
    $manifest = $manifest.Replace('example.invalid/devsandbox/devsandbox-operator@sha256:1111111111111111111111111111111111111111111111111111111111111111', $operatorImage)
    foreach ($imageName in $managementImages.Keys) {
        $manifest = $manifest.Replace(
            "example.invalid/devsandbox/${imageName}@sha256:1111111111111111111111111111111111111111111111111111111111111111",
            $managementImages[$imageName]
        )
    }
    $vaultBase = $KeyVaultUri.TrimEnd('/')
    $manifest = $manifest.Replace('https://example-vault.vault.azure.net/keys/devsandbox-route-signing', "$vaultBase/keys/devsandbox-route-signing")
    $manifest = $manifest.Replace('https://example-vault.vault.azure.net', $vaultBase)
    $manifest = $manifest.Replace('githubAppClientId: replace-at-deploy', "githubAppClientId: `"$env:DEVSANDBOX_GITHUB_APP_CLIENT_ID`"")
    $manifest = $manifest.Replace('githubAuthMode: replace-at-deploy', "githubAuthMode: `"$GitHubAuthMode`"")
    $manifest = $manifest.Replace('primaryGithubOrg: replace-at-deploy', "primaryGithubOrg: `"$env:DEVSANDBOX_PRIMARY_GITHUB_ORG`"")
    $routePublicKey = if (-not [string]::IsNullOrWhiteSpace($env:DEVSANDBOX_ROUTE_PUBLIC_KEY)) {
        $env:DEVSANDBOX_ROUTE_PUBLIC_KEY
    }
    else {
        $GeneratedRoutePublicKey
    }
    if ([string]::IsNullOrWhiteSpace($routePublicKey)) {
        throw 'Kubernetes stage Blocked: route public key bootstrap did not return a key.'
    }
    $publicKeyLines = ($routePublicKey.Trim() -split "`r?`n") |
        ForEach-Object { "    $_" }
    $manifest = $manifest.Replace('    REPLACE_WITH_KEY_VAULT_RSA_PUBLIC_KEY', ($publicKeyLines -join "`n"))
    foreach ($templateName in $templateImages.Keys) {
        $parts = $templateImages[$templateName] -split '@', 2
        $placeholder = switch ($templateName) {
            standard { 'sha256:' + ('a' * 64) }
            vscode { 'sha256:' + ('b' * 64) }
            copilot { 'sha256:' + ('c' * 64) }
        }
        $manifest = $manifest.Replace("example.invalid/devsandbox/$templateName", $parts[0])
        $manifest = $manifest.Replace($placeholder, $parts[1])
    }
    if ($manifest -match 'example\.invalid/devsandbox|sha256:[abc]{64}|replace-at-deploy|REPLACE_WITH_') {
        throw 'Kubernetes stage Blocked: a deploy-time placeholder remains after replacement.'
    }

    $artifactDirectory = Join-Path $PSScriptRoot '..\artifacts'
    $manifestPath = Join-Path $artifactDirectory 'poc.yaml'
    New-Item -ItemType Directory -Force -Path $artifactDirectory | Out-Null
    [System.IO.File]::WriteAllText(
        $manifestPath,
        "$manifest`n",
        [System.Text.UTF8Encoding]::new($false)
    )

    $sensitivePath = Join-Path $artifactDirectory 'poc-deploy-sensitive.yaml'
    try {
        $encode = {
            param([string]$Value)
            [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($Value))
        }
        $brokerCertificateLines = ($env:DEVSANDBOX_BROKER_TLS_CERTIFICATE.Trim() -split "`r?`n") |
            ForEach-Object { "    $_" }
        $secretManifest = @"
---
apiVersion: v1
kind: Secret
metadata:
  name: devsandbox-runtime-secrets
  namespace: devsandbox-system
type: Opaque
data:
  github-app-client-secret: $(& $encode $env:DEVSANDBOX_GITHUB_APP_CLIENT_SECRET)
  github-static-user-id: $(& $encode $env:DEVSANDBOX_GITHUB_STATIC_USER_ID)
  github-static-login: $(& $encode $env:DEVSANDBOX_GITHUB_STATIC_LOGIN)
  github-static-token: $(& $encode $env:DEVSANDBOX_GITHUB_STATIC_TOKEN)
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: devsandbox-broker-ca
  namespace: devsandbox-system
data:
  ca.crt: |
$($brokerCertificateLines -join "`n")
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: devsandbox-broker-ca
  namespace: devsandbox-workloads
data:
  ca.crt: |
$($brokerCertificateLines -join "`n")
---
apiVersion: v1
kind: Secret
metadata:
  name: devsandbox-broker-tls
  namespace: devsandbox-system
type: kubernetes.io/tls
data:
  tls.crt: $(& $encode $env:DEVSANDBOX_BROKER_TLS_CERTIFICATE)
  tls.key: $(& $encode $env:DEVSANDBOX_BROKER_TLS_PRIVATE_KEY)
"@
        [System.IO.File]::WriteAllText(
            $sensitivePath,
            "$manifest`n$secretManifest`n",
            [System.Text.UTF8Encoding]::new($false)
        )
        Write-Host 'Applying Kubernetes resources and checking rollouts through AKS Run Command...'
        $runCommand = "set -eu; kubectl apply -f poc-deploy-sensitive.yaml >/dev/null 2>&1 || true; " +
            "kubectl wait --for=condition=Established crd/devsandboxtemplates.devsandbox.io --timeout=2m; " +
            "kubectl apply -f poc-deploy-sensitive.yaml; " +
            "kubectl rollout restart deployment/devsandbox-broker -n devsandbox-system"
        $rawResult = (& az aks command invoke `
            --resource-group $ResourceGroupName `
            --name $AksClusterName `
            --file $sensitivePath `
            --command $runCommand `
            --query '{id:id,provisioningState:provisioningState,exitCode:exitCode,logs:logs}' `
            --only-show-errors `
            --output json) -join "`n"
        if ($LASTEXITCODE -ne 0) {
            throw "AKS Run Command failed with exit code $LASTEXITCODE."
        }
        $result = $rawResult | ConvertFrom-Json
        if ($result.provisioningState -ne 'Succeeded' -or
            ($null -ne $result.exitCode -and [int]$result.exitCode -ne 0)) {
            if (-not [string]::IsNullOrWhiteSpace($result.logs)) {
                Write-Error "Kubernetes rollout logs:`n$($result.logs)" -ErrorAction Continue
            }
            throw "Kubernetes apply or rollout failed (state=$($result.provisioningState), exitCode=$($result.exitCode))."
        }

        $rolloutCheck = 'set -eu; ' +
            'kubectl wait --for=condition=Available deployment/agent-sandbox-controller -n agent-sandbox-system --timeout=5s; ' +
            'kubectl wait --for=condition=Available deployment/devsandbox-api -n devsandbox-system --timeout=5s; ' +
            'kubectl wait --for=condition=Available deployment/devsandbox-web -n devsandbox-system --timeout=5s; ' +
            'kubectl wait --for=condition=Available deployment/devsandbox-broker -n devsandbox-system --timeout=5s; ' +
            'kubectl wait --for=condition=Available deployment/devsandbox-operator -n devsandbox-system --timeout=5s; ' +
            'kubectl wait --for=condition=Available deployment/sandbox-router -n devsandbox-system --timeout=5s'
        $deadline = [DateTime]::UtcNow.AddMinutes(12)
        do {
            $rolloutExit = (& az aks command invoke --resource-group $ResourceGroupName --name $AksClusterName `
                --command $rolloutCheck --query exitCode --only-show-errors --output tsv) -join ''
            if ($LASTEXITCODE -ne 0) {
                throw 'AKS rollout status check failed.'
            }
            if ($rolloutExit -eq '0') {
                break
            }
            Start-Sleep -Seconds 20
        } while ([DateTime]::UtcNow -lt $deadline)
        if ($rolloutExit -ne '0') {
            throw 'Kubernetes deployments did not become ready before the rollout deadline.'
        }
        Write-Host 'Kubernetes apply and rollouts passed.'
    }
    finally {
        Remove-Item -LiteralPath $sensitivePath -Force -ErrorAction SilentlyContinue
    }
}

function Get-DeploymentParameters {
    $parameters = [System.Collections.Generic.List[string]]::new()
    $parameters.Add($ParametersFile)
    $parameters.Add("resourcePrefix=$ResourcePrefix")
    $parameters.Add("location=$Location")
    $parameters.Add("deploymentPrincipalObjectId=$DeploymentPrincipalObjectId")
    return $parameters.ToArray()
}

function Get-GatewayAddress {
    $deadline = [DateTime]::UtcNow.AddMinutes(10)
    do {
        $logs = (& az aks command invoke --resource-group $ResourceGroupName --name $AksClusterName `
            --command "kubectl get gateway devsandbox -n devsandbox-system -o jsonpath='{.status.addresses[0].value}'" `
            --query logs --only-show-errors --output tsv 2>$null) -join ''
        if ($LASTEXITCODE -eq 0 -and $logs.Trim() -match '^[A-Za-z0-9.-]+$') {
            return $logs.Trim()
        }
        Start-Sleep -Seconds 20
    } while ([DateTime]::UtcNow -lt $deadline)
    throw 'FrontDoor stage Blocked: the AKS Gateway did not report a usable public address.'
}

function Deploy-FrontDoorBinding {
    $gatewayAddress = Get-GatewayAddress
    Write-Host 'Binding Azure Front Door endpoints to the AKS HTTP Gateway origin...'
    $state = & az deployment group create `
        --resource-group $ResourceGroupName `
        --name 'devsandbox-front-door-binding' `
        --template-file '.\infra\front-door-binding.bicep' `
        --parameters "profileName=$FrontDoorProfileName" `
            "apiEndpointName=$FrontDoorApiEndpointName" `
            "webEndpointName=$FrontDoorWebEndpointName" `
            "gatewayAddress=$gatewayAddress" `
            "apiOriginHostname=$ApiOriginHostname" `
            "webOriginHostname=$WebOriginHostname" `
        --only-show-errors `
        --query properties.provisioningState `
        --output tsv
    if ($LASTEXITCODE -ne 0 -or $state -ne 'Succeeded') {
        throw "Azure Front Door origin binding failed (state=$state)."
    }
    Write-Host 'Azure Front Door origin groups and HTTPS-only routes are ready.'
}

function Get-DeployedAuthenticationMode {
    $mode = (& az aks command invoke --resource-group $ResourceGroupName --name $AksClusterName `
        --command 'kubectl get configmap devsandbox-management-config -n devsandbox-system -o jsonpath={.data.githubAuthMode}' `
        --query logs --only-show-errors --output tsv) -join ''
    if ($LASTEXITCODE -ne 0 -or $mode.Trim() -notin @('static-validation', 'github-app')) {
        throw 'Local configuration failed: deployed authentication mode could not be determined.'
    }
    if ($mode.Trim() -eq 'static-validation') {
        return 'github-cli-static'
    }
    return 'device-flow'
}

function Write-LocalConfiguration {
    if ($SkipLocalConfiguration) {
        Write-Host 'Skipping workstation-local configuration and CLI build.'
        return
    }
    $authenticationMode = Get-DeployedAuthenticationMode
    $githubLogin = $null
    if ($authenticationMode -eq 'github-cli-static') {
        $githubLogin = (& az aks command invoke --resource-group $ResourceGroupName --name $AksClusterName `
            --command 'kubectl get secret devsandbox-runtime-secrets -n devsandbox-system -o jsonpath={.data.github-static-login} | base64 -d' `
            --query logs --only-show-errors --output tsv) -join ''
        if ($LASTEXITCODE -ne 0 -or $githubLogin.Trim() -notmatch '^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$') {
            throw 'Local configuration failed: deployed GitHub login could not be determined.'
        }
    }
    & (Join-Path $PSScriptRoot 'configure-local.ps1') `
        -ApiUrl "https://$ApiHostname" `
        -WebUrl "https://$WebHostname" `
        -AuthMode $authenticationMode `
        -GitHubLogin $githubLogin
    if ($LASTEXITCODE -ne 0) {
        throw "Local configuration failed with exit code $LASTEXITCODE."
    }
}

function Remove-LegacyIngressResources {
    $outputs = Get-FoundationOutputs
    $tenantId = (& az account show --query tenantId --output tsv --only-show-errors).Trim()
    $artifactDirectory = Join-Path $PSScriptRoot '..\artifacts'
    New-Item -ItemType Directory -Force -Path $artifactDirectory | Out-Null
    $manifestPath = Join-Path $artifactDirectory "devsandbox-legacy-ingress-cleanup-$PID.yaml"
    try {
        $templatePath = Join-Path $PSScriptRoot '..\deploy\bootstrap\legacy-ingress-cleanup.yaml'
        $manifest = (Get-Content -Raw -LiteralPath $templatePath).
            Replace('REPLACE_BOOTSTRAP_CLIENT_ID', $outputs.bootstrapIdentityClientId).
            Replace('REPLACE_AZURE_TENANT_ID', $tenantId).
            Replace('REPLACE_KEY_VAULT_NAME', $outputs.keyVaultName)
        [IO.File]::WriteAllText($manifestPath, $manifest, [Text.UTF8Encoding]::new($false))
        $manifestName = Split-Path -Leaf $manifestPath
        $command = "set -eu; " +
            "kubectl delete externaldns devsandbox -n devsandbox-system --ignore-not-found; " +
            "kubectl delete deployment devsandbox-external-dns-external-dns -n devsandbox-system --ignore-not-found; " +
            "kubectl delete serviceaccount devsandbox-external-dns-external-dns -n devsandbox-system --ignore-not-found; " +
            "kubectl delete role devsandbox-external-dns-external-dns -n devsandbox-system --ignore-not-found; " +
            "kubectl delete rolebinding devsandbox-external-dns-external-dns -n devsandbox-system --ignore-not-found; " +
            "kubectl delete clusterrole devsandbox-external-dns-external-dns-list-ns --ignore-not-found; " +
            "kubectl delete clusterrolebinding devsandbox-external-dns-external-dns-list-ns --ignore-not-found; " +
            "kubectl delete job devsandbox-legacy-ingress-cleanup -n devsandbox-system --ignore-not-found; " +
            "kubectl apply -f '$manifestName' >/dev/null; " +
            "kubectl wait --for=condition=complete job/devsandbox-legacy-ingress-cleanup -n devsandbox-system --timeout=300s >/dev/null; " +
            "kubectl logs job/devsandbox-legacy-ingress-cleanup -n devsandbox-system"
        $logs = (& az aks command invoke --resource-group $outputs.resourceGroupName `
            --name $outputs.aksClusterName --file $manifestPath --command $command `
            --query logs --only-show-errors --output tsv) -join "`n"
        if ($LASTEXITCODE -ne 0 -or $logs -notmatch 'LEGACY_INGRESS_CLEANUP_OK') {
            throw 'Legacy Kubernetes ingress and certificate cleanup failed.'
        }
    }
    finally {
        Remove-Item -LiteralPath $manifestPath -Force -ErrorAction SilentlyContinue
    }

    $legacyIdentities = @(((& az identity list --resource-group $outputs.resourceGroupName `
        --query "[?ends_with(name, '-routing-id')].{id:id,principalId:principalId}" `
        --output json --only-show-errors) -join "`n") | ConvertFrom-Json) |
        Where-Object { $null -ne $_ -and -not [string]::IsNullOrWhiteSpace($_.id) }
    foreach ($identity in $legacyIdentities) {
        $assignments = @(& az role assignment list --assignee $identity.principalId --all `
            --query '[].id' --output tsv --only-show-errors)
        foreach ($assignment in $assignments) {
            if (-not [string]::IsNullOrWhiteSpace($assignment)) {
                & az role assignment delete --ids $assignment --only-show-errors
                if ($LASTEXITCODE -ne 0) {
                    throw "Legacy ingress role assignment could not be deleted: $assignment"
                }
            }
        }
        & az identity delete --ids $identity.id --only-show-errors
        if ($LASTEXITCODE -ne 0) {
            throw "Legacy ingress identity could not be deleted: $($identity.id)"
        }
    }

    $legacyZoneId = (& az network dns zone list --resource-group $outputs.resourceGroupName `
        --query "[?name=='devsandbox.invalid'].id | [0]" --output tsv --only-show-errors) -join ''
    if ($LASTEXITCODE -ne 0) {
        throw 'Legacy Azure DNS zones could not be inspected.'
    }
    if (-not [string]::IsNullOrWhiteSpace($legacyZoneId)) {
        & az network dns zone delete --ids $legacyZoneId --yes --only-show-errors
        if ($LASTEXITCODE -ne 0) {
            throw 'Legacy devsandbox.invalid Azure DNS zone could not be deleted.'
        }
    }
    Write-Host 'Legacy .invalid DNS, ExternalDNS, ingress identity, and certificate resources are retired.'
}

function Assert-CloudPreflight {
    param([switch]$Full)
    if (-not (Get-Command az -ErrorAction SilentlyContinue)) {
        throw 'Cloud stage Blocked: missing tool: az'
    }
    if (-not (Test-Path -LiteralPath $ParametersFile)) {
        throw "Cloud stage Blocked: parameters file was not found: $ParametersFile"
    }

    $missing = @(Get-MissingCloudInputs)
    if ($missing.Count -gt 0) {
        throw "Cloud stage Blocked: missing inputs: $($missing -join ', ')"
    }
    if (-not (Test-AzureAuthentication)) {
        throw 'Cloud stage Blocked: Azure authentication is unavailable. Run az login.'
    }

    Invoke-Native az @('account', 'set', '--subscription', $SubscriptionId) -DiscardOutput
    $preflightMode = if ($Full) { 'Deployment' } else { 'Foundation' }
    & (Join-Path $PSScriptRoot 'preflight.ps1') -Mode $preflightMode `
        -SubscriptionId $SubscriptionId `
        -Location $Location `
        -ResourcePrefix $ResourcePrefix `
        -DeploymentPrincipalObjectId $DeploymentPrincipalObjectId `
        -ResourceGroupName $ResourceGroupName `
        -AksClusterName $AksClusterName `
        -ContainerRegistryName $ContainerRegistryName
    if ($LASTEXITCODE -ne 0) {
        exit $LASTEXITCODE
    }
    Write-Host 'Azure preflight succeeded.'
}

Push-Location (Join-Path $PSScriptRoot '..')
try {
    if ($Stage -eq 'Preflight') {
        Assert-CloudPreflight -Full
        return
    }

    if ($Stage -in @('Validate', 'All')) {
        Invoke-StaticValidation
    }

    if ($Stage -in @('WhatIf', 'Foundation', 'All')) {
        Assert-CloudPreflight -Full:($Stage -eq 'All')
        $deploymentParameters = @(Get-DeploymentParameters)
        $deploymentName = "devsandbox-$($ResourcePrefix.ToLowerInvariant())-foundation"
    }

    if ($Stage -eq 'Bootstrap') {
        Assert-AzureContext
        Ensure-RouteSigningKey
        return
    }

    if ($Stage -in @('WhatIf', 'All')) {
        Write-Host 'Running subscription what-if...'
        $whatIfArguments = @(
            'deployment', 'sub', 'what-if',
            '--name', $deploymentName,
            '--location', $Location,
            '--template-file', '.\infra\foundation.bicep',
            '--parameters'
        ) + $deploymentParameters
        Invoke-Native az $whatIfArguments
    }

    if ($Stage -in @('Foundation', 'All')) {
        Write-Host 'Deploying Azure foundation...'
        $state = & az deployment sub create `
            --name $deploymentName `
            --location $Location `
            --template-file '.\infra\foundation.bicep' `
            --parameters @deploymentParameters `
            --only-show-errors `
            --query properties.provisioningState `
            --output tsv
        if ($LASTEXITCODE -ne 0) {
            throw "Azure foundation deployment failed with exit code $LASTEXITCODE"
        }
        if ($state -ne 'Succeeded') {
            throw "Azure foundation deployment finished in unexpected state: $state"
        }
        Write-Host 'Azure foundation deployment succeeded.'
        Ensure-ContainerInsightsCollection
        Remove-LegacyAppRoutingDnsZones
        Ensure-RouteSigningKey
    }

    if ($Stage -in @('Images', 'Kubernetes', 'FrontDoor', 'Verify', 'All')) {
        Resolve-PostFoundationConfiguration `
            -ImagesOnly:($Stage -eq 'Images') `
            -FrontDoorOnly:($Stage -eq 'FrontDoor') `
            -VerificationOnly:($Stage -eq 'Verify')
    }

    if ($Stage -in @('Images', 'All')) {
        Build-Images
    }

    if ($Stage -in @('Kubernetes', 'All')) {
        Deploy-KubernetesResources
    }

    if ($Stage -in @('FrontDoor', 'All')) {
        Deploy-FrontDoorBinding
    }

    if ($Stage -in @('Verify', 'All')) {
        & (Join-Path $PSScriptRoot 'verify-mvp02.ps1') `
            -ResourceGroupName $ResourceGroupName `
            -AksClusterName $AksClusterName `
            -SubscriptionId $SubscriptionId `
            -ApiHostname $ApiHostname `
            -WebHostname $WebHostname `
            -FrontDoorProfileName $FrontDoorProfileName `
            -FrontDoorProfileResourceId $FrontDoorProfileResourceId `
            -FrontDoorId $FrontDoorId `
            -FrontDoorApiEndpointName $FrontDoorApiEndpointName `
            -FrontDoorWebEndpointName $FrontDoorWebEndpointName `
            -ApiOriginHostname $ApiOriginHostname `
            -WebOriginHostname $WebOriginHostname
        if ($LASTEXITCODE -ne 0) {
            throw "Deployment verification failed with exit code $LASTEXITCODE."
        }
        Remove-LegacyIngressResources
        Write-LocalConfiguration
    }
}
finally {
    Pop-Location
}
