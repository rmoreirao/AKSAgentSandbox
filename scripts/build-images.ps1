[CmdletBinding()]
param(
    [ValidateSet('Validate', 'Build', 'Push')]
    [string]$Action = 'Build',

    [string]$Registry,
    [string]$Version,
    [string]$OutputPath = (Join-Path $PSScriptRoot '..\artifacts\image-digests.json')
)

$ErrorActionPreference = 'Stop'
$PSNativeCommandUseErrorActionPreference = $true
$root = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
$metadataPath = Join-Path $root 'images\versions.json'
$metadata = Get-Content -Raw -LiteralPath $metadataPath | ConvertFrom-Json
if ([string]::IsNullOrWhiteSpace($Version)) {
    $Version = $metadata.templateVersion
}
$revision = (& git -C $root rev-parse HEAD).Trim()
if ($LASTEXITCODE -ne 0 -or $revision -notmatch '^[0-9a-f]{40}$') {
    throw 'Unable to resolve the source revision.'
}
$buildDate = [DateTime]::UtcNow.ToString('yyyy-MM-ddTHH:mm:ssZ')

function Invoke-Native {
    param([string]$FilePath, [string[]]$ArgumentList)
    & $FilePath @ArgumentList | Out-Host
    if ($LASTEXITCODE -ne 0) {
        throw "$FilePath failed with exit code $LASTEXITCODE"
    }
}

function Assert-StaticInputs {
    $allDockerfiles = ''
    foreach ($image in $metadata.baseImages.PSObject.Properties.Value) {
        if ($image -notmatch '@sha256:[0-9a-f]{64}$') {
            throw "Unpinned base image: $image"
        }
    }
    foreach ($path in @(
        'images\standard\Dockerfile',
        'images\vscode\Dockerfile',
        'images\copilot\Dockerfile',
        'images\management\Dockerfile',
        'deploy\router\Dockerfile'
    )) {
        $text = Get-Content -Raw -LiteralPath (Join-Path $root $path)
        $allDockerfiles += $text
        if ($text -notmatch '(?m)^FROM ' -or $text -notmatch 'org\.opencontainers\.image\.revision') {
            throw "$path is missing a build stage or required OCI revision label."
        }
        if (($text | Select-String -Pattern '(?m)^FROM ' -AllMatches).Matches.Count -lt 2) {
            throw "$path must be a multi-stage Dockerfile."
        }
        if ($text -match '(?i)(ghp_|github_pat_|BEGIN [A-Z ]*PRIVATE KEY|KUBECONFIG=)') {
            throw "$path contains secret-like material."
        }
    }
    foreach ($image in $metadata.baseImages.PSObject.Properties.Value) {
        if (-not $allDockerfiles.Contains($image)) {
            throw "Pinned base image metadata is not used by a Dockerfile: $image"
        }
    }
    $expectedPins = @(
        $metadata.tools.codeServer.version,
        $metadata.tools.githubCli.version,
        $metadata.tools.copilotCli.version,
        $metadata.tools.playwrightCli.version,
        $metadata.tools.playwrightCli.playwrightVersion,
        $metadata.tools.playwrightCli.sha256,
        $metadata.tools.sandboxRouter.sourceCommit
    )
    foreach ($pin in $expectedPins) {
        if (-not $allDockerfiles.Contains([string]$pin)) {
            throw "Version metadata pin is not present in a Dockerfile: $pin"
        }
    }
    foreach ($path in @('images\standard\Dockerfile', 'images\vscode\Dockerfile', 'images\copilot\Dockerfile')) {
        $text = Get-Content -Raw -LiteralPath (Join-Path $root $path)
        if (-not $text.Contains('USER 1000:1000') -or
            $text -notmatch 'DEVSANDBOX_GITHUB_TOKEN_FILE=/run/devsandbox/github-token') {
            throw "$path must run as UID/GID 1000 and use the memory-backed token path."
        }
    }
    $templates = Get-ChildItem (Join-Path $root 'deploy\kustomize\base\templates') -Filter '*.yaml' |
        Where-Object Name -ne 'kustomization.yaml'
    if ($templates.Count -ne 3) {
        throw 'Exactly three DevSandboxTemplate manifests are required.'
    }
    foreach ($template in $templates) {
        $text = Get-Content -Raw -LiteralPath $template.FullName
        if ($text -notmatch 'digest:\s+sha256:[abc]{64}' -or $text -match 'volumeClaimTemplates') {
            throw "$($template.Name) must retain a deploy-time digest placeholder and no volumeClaimTemplates."
        }
    }
}

function Assert-Docker {
    try {
        & docker info --format '{{.ServerVersion}}' *> $null
    }
    catch {
        throw 'Image build Blocked: Docker daemon is unavailable.'
    }
    if ($LASTEXITCODE -ne 0) {
        throw 'Image build Blocked: Docker daemon is unavailable.'
    }
}

function Get-Tag([string]$name) {
    $repository = "devsandbox/$name"
    if (-not [string]::IsNullOrWhiteSpace($Registry)) {
        $repository = "$($Registry.TrimEnd('/'))/$repository"
    }
    return "${repository}:$Version"
}

function Build-Image([string]$name, [string]$dockerfile, [string[]]$ExtraArgs = @()) {
    $tag = Get-Tag $name
    $arguments = @(
        'build', '--pull',
        '--file', (Join-Path $root $dockerfile),
        '--tag', $tag,
        '--build-arg', "BUILD_DATE=$buildDate",
        '--build-arg', "REVISION=$revision",
        '--build-arg', "TEMPLATE_VERSION=$Version"
    ) + $ExtraArgs + @($root)
    Invoke-Native docker $arguments
    return $tag
}

function Build-Router {
    $commit = $metadata.tools.sandboxRouter.sourceCommit
    $source = Join-Path $root 'artifacts\agent-sandbox-router-source'
    Remove-Item -LiteralPath $source -Recurse -Force -ErrorAction SilentlyContinue
    New-Item -ItemType Directory -Force -Path (Split-Path -Parent $source) | Out-Null
    try {
        Invoke-Native git @('clone', '--quiet', '--filter=blob:none', '--no-checkout',
            'https://github.com/kubernetes-sigs/agent-sandbox.git', $source)
        Invoke-Native git @('-C', $source, 'checkout', '--quiet', '--detach', $commit)
        $actual = (& git -C $source rev-parse HEAD).Trim()
        if ($actual -ne $commit) {
            throw "Sandbox Router source mismatch: expected $commit, got $actual"
        }
        Remove-Item -LiteralPath (Join-Path $source '.git') -Recurse -Force
        Copy-Item (Join-Path $root 'deploy\router\Dockerfile') (Join-Path $source 'Dockerfile')
        $tag = Get-Tag 'sandbox-router'
        Invoke-Native docker @(
            'build', '--pull', '--file', (Join-Path $source 'Dockerfile'), '--tag', $tag,
            '--build-arg', "GIT_SHA=$commit", '--build-arg', "BUILD_DATE=$buildDate", $source
        )
        return $tag
    }
    finally {
        Remove-Item -LiteralPath $source -Recurse -Force -ErrorAction SilentlyContinue
    }
}

function Test-Images([hashtable]$tags) {
    $checks = @(
        @{ Image = $tags.standard; Arguments = @('git', '--version'); Expected = '^git version ' },
        @{ Image = $tags.vscode; Arguments = @('code-server', '--version'); Expected = "(?m)^$([regex]::Escape($metadata.tools.codeServer.version))" },
        @{ Image = $tags.copilot; Arguments = @('gh', '--version'); Expected = "gh version $([regex]::Escape($metadata.tools.githubCli.version))" },
        @{ Image = $tags.copilot; Arguments = @('copilot', '--version'); Expected = [regex]::Escape($metadata.tools.copilotCli.version) },
        @{ Image = $tags.copilot; Arguments = @('playwright-cli', '--help'); Expected = 'playwright-cli' }
    )
    foreach ($check in $checks) {
        $output = (& docker run --rm $check.Image @($check.Arguments)) -join "`n"
        $output | Out-Host
        if ($LASTEXITCODE -ne 0 -or $output -notmatch $check.Expected) {
            throw "Tool version check failed in $($check.Image)."
        }
    }
    $browserScript = "const {chromium}=require('/usr/local/lib/node_modules/@playwright/cli/node_modules/playwright'); chromium.launch({headless:true}).then(async b=>{console.log(b.version());await b.close()})"
    $browserVersion = (& docker run --rm $tags.copilot node -e $browserScript).Trim()
    if ($LASTEXITCODE -ne 0 -or $browserVersion -ne $metadata.tools.playwrightCli.chromiumVersion) {
        throw "Chromium version check failed: $browserVersion"
    }
    foreach ($name in @('standard', 'vscode', 'copilot')) {
        $uid = (& docker run --rm $tags[$name] id -u).Trim()
        if ($LASTEXITCODE -ne 0 -or $uid -ne '1000') {
            throw "$name image does not start as UID 1000."
        }
        $history = (& docker history --no-trunc --format '{{.CreatedBy}}' $tags[$name]) -join "`n"
        if ($history -match '(?i)(ghp_[A-Za-z0-9]+|github_pat_[A-Za-z0-9_]+|BEGIN [A-Z ]*PRIVATE KEY|KUBECONFIG=)') {
            throw "$name image history contains secret-like material."
        }
    }
}

Push-Location $root
try {
    Assert-StaticInputs
    if ($Action -eq 'Validate') {
        Write-Host 'Image metadata, Dockerfile pins, and template placeholders are valid.'
        return
    }
    Assert-Docker
    if ($Action -eq 'Push' -and [string]::IsNullOrWhiteSpace($Registry)) {
        throw '-Registry is required for Push. Authenticate Docker before running this script.'
    }

    $tags = @{}
    $tags.standard = Build-Image 'standard' 'images\standard\Dockerfile'
    $tags.vscode = Build-Image 'vscode' 'images\vscode\Dockerfile'
    $tags.copilot = Build-Image 'copilot' 'images\copilot\Dockerfile'
    foreach ($binary in @('devsandbox-api', 'devsandbox-broker', 'devsandbox-operator', 'devsandbox-web')) {
        $tags[$binary] = Build-Image $binary 'images\management\Dockerfile' @(
            '--build-arg', "BINARY=$binary", '--build-arg', "IMAGE_VERSION=$Version"
        )
    }
    $tags['sandbox-router'] = Build-Router
    Test-Images $tags

    $result = [ordered]@{
        schemaVersion = 1
        version = $Version
        revision = $revision
        buildDate = $buildDate
        images = [ordered]@{}
    }
    foreach ($entry in $tags.GetEnumerator() | Sort-Object Key) {
        if ($Action -eq 'Push') {
            Invoke-Native docker @('push', $entry.Value)
            $repoDigest = (& docker image inspect --format '{{index .RepoDigests 0}}' $entry.Value).Trim()
            if ($LASTEXITCODE -ne 0 -or $repoDigest -notmatch '@sha256:[0-9a-f]{64}$') {
                throw "No immutable digest was recorded for $($entry.Key)."
            }
            $result.images[$entry.Key] = $repoDigest
        }
        else {
            $imageId = (& docker image inspect --format '{{.Id}}' $entry.Value).Trim()
            $result.images[$entry.Key] = $imageId
        }
    }
    New-Item -ItemType Directory -Force -Path (Split-Path -Parent $OutputPath) | Out-Null
    $result | ConvertTo-Json -Depth 6 | Set-Content -LiteralPath $OutputPath -Encoding utf8NoBOM
    Write-Host "Image results recorded in $OutputPath"
}
finally {
    Pop-Location
}
