[CmdletBinding()]
param(
    [Parameter(Mandatory)]
    [ValidateSet('Static', 'Build')]
    [string]$Mode
)

$ErrorActionPreference = 'Stop'
$PSNativeCommandUseErrorActionPreference = $true

function Invoke-Native {
    param(
        [Parameter(Mandatory)]
        [string]$Command,

        [Parameter(ValueFromRemainingArguments)]
        [string[]]$Arguments
    )

    & $Command @Arguments
    if ($LASTEXITCODE -ne 0) {
        throw "$Command failed with exit code $LASTEXITCODE"
    }
}

function Assert-Command {
    param([Parameter(Mandatory)][string]$Name)

    if (-not (Get-Command $Name -ErrorAction SilentlyContinue)) {
        throw "Static validation Blocked: missing tool: $Name"
    }
}

function Test-DockerDaemon {
    if (-not (Get-Command docker -ErrorAction SilentlyContinue)) {
        return $false
    }
    try {
        & docker info --format '{{.ServerVersion}}' *> $null
        return $LASTEXITCODE -eq 0
    }
    catch {
        return $false
    }
}

Push-Location (Join-Path $PSScriptRoot '..')
try {
    switch ($Mode) {
        'Static' {
            foreach ($tool in @('git', 'go', 'gofmt', 'az', 'kubectl', 'node', 'npm', 'npx')) {
                Assert-Command $tool
            }

            $requiredGo = ((Get-Content -LiteralPath '.\go.mod' |
                Where-Object { $_ -match '^go\s+' }) -split '\s+')[1]
            $actualGo = (& go env GOVERSION).Trim() -replace '^go', ''
            if ($LASTEXITCODE -ne 0 -or -not $actualGo.StartsWith("$requiredGo.")) {
                throw "Go $requiredGo.x is required by go.mod; found $actualGo."
            }

            $goFiles = @(Invoke-Native git ls-files --cached --others --exclude-standard '*.go' |
                Where-Object { $_ -notmatch '^"?(?:artifacts|\.tools)[/\\]' })
            $unformatted = @()
            if ($goFiles.Count -gt 0) {
                $unformatted = @(Invoke-Native gofmt -l @goFiles)
            }
            if ($unformatted.Count -gt 0) {
                throw "The following Go files are not formatted:`n$($unformatted -join "`n")"
            }

            Invoke-Native go vet ./...
            Invoke-Native go test ./...
            Invoke-Native go test ./api/v1alpha1 -run '^TestOpenAPICoversSectionTenOperations$' -count=1

            Invoke-Native az bicep version
            Invoke-Native az bicep build --file .\infra\foundation.bicep --stdout | Out-Null
            Invoke-Native az bicep build --file .\infra\main.bicep --stdout | Out-Null
            Invoke-Native az bicep build --file .\infra\front-door-binding.bicep --stdout | Out-Null

            $rendered = @(Invoke-Native kubectl kustomize .\deploy\kustomize\overlays\poc)
            $renderedText = $rendered -join "`n"
            if ($renderedText -match 'image:\s+\S+:latest(?:\s|$)') {
                throw 'Rendered Kubernetes resources contain a mutable latest image tag.'
            }
            if ($renderedText -match '(?m)^\s*type:\s+LoadBalancer\s*$') {
                throw 'DevSandbox manifests must not define a direct LoadBalancer Service.'
            }

            $pinPath = '.\deploy\kustomize\base\upstream\agent-sandbox\v1.0.0\pin.json'
            $upstreamPath = '.\deploy\kustomize\base\upstream\agent-sandbox\v1.0.0\sandbox.yaml'
            $pin = Get-Content -Raw -LiteralPath $pinPath | ConvertFrom-Json
            $actualHash = (Get-FileHash -Algorithm SHA256 -LiteralPath $upstreamPath).Hash.ToLowerInvariant()
            if ($actualHash -ne $pin.releaseAsset.sha256) {
                throw "Pinned Agent Sandbox manifest checksum mismatch: $actualHash"
            }

            foreach ($scriptPath in Get-ChildItem '.\scripts\*.ps1') {
                $parseErrors = $null
                [System.Management.Automation.Language.Parser]::ParseFile(
                    $scriptPath.FullName,
                    [ref]$null,
                    [ref]$parseErrors
                ) | Out-Null
                if ($parseErrors.Count -gt 0) {
                    throw "$($scriptPath.Name) has PowerShell parse errors: $($parseErrors -join '; ')"
                }
            }

            & (Join-Path $PSScriptRoot 'build-images.ps1') -Action Validate
            if ($LASTEXITCODE -ne 0) {
                throw "Image static validation failed with exit code $LASTEXITCODE."
            }
            & (Join-Path $PSScriptRoot 'security-validate.ps1') -Mode Static
            if ($LASTEXITCODE -ne 0) {
                throw "Security static validation failed with exit code $LASTEXITCODE."
            }
            Invoke-Native npx --no-install playwright-cli --help | Out-Null
            Invoke-Native npx --no-install playwright test --list
            Write-Host 'Passed: Static'
        }
        'Build' {
            Invoke-Native go build ./cmd/...
            if (Test-DockerDaemon) {
                & (Join-Path $PSScriptRoot 'build-images.ps1') -Action Build
                if ($LASTEXITCODE -ne 0) {
                    throw "Container image build failed with exit code $LASTEXITCODE."
                }
            }
            else {
                Write-Output 'Blocked: container image build (Docker daemon unavailable); Go command build passed.'
                $global:LASTEXITCODE = 0
            }
            Write-Host 'Passed: Build commands'
        }
    }
}
finally {
    Pop-Location
}
