[CmdletBinding()]
param(
    [ValidateSet('Static', 'Deployed', 'All')]
    [string]$Mode = 'Static',

    [string]$ResourceGroupName = $env:DEVSANDBOX_RESOURCE_GROUP,
    [string]$AksClusterName = $env:DEVSANDBOX_AKS_CLUSTER
)

$ErrorActionPreference = 'Stop'
$PSNativeCommandUseErrorActionPreference = $false
$root = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path

function Assert-Match {
    param(
        [Parameter(Mandatory)][string]$Text,
        [Parameter(Mandatory)][string]$Pattern,
        [Parameter(Mandatory)][string]$Assertion
    )
    if ($Text -notmatch $Pattern) {
        throw "Security validation failed: $Assertion"
    }
}

function Assert-NoMatch {
    param(
        [Parameter(Mandatory)][string]$Text,
        [Parameter(Mandatory)][string]$Pattern,
        [Parameter(Mandatory)][string]$Assertion
    )
    if ($Text -match $Pattern) {
        throw "Security validation failed: $Assertion"
    }
}

function Read-RepositoryFile {
    param([Parameter(Mandatory)][string]$Path)
    Get-Content -Raw -LiteralPath (Join-Path $root $Path)
}

function Invoke-StaticSecurityValidation {
    if (-not (Get-Command kubectl -ErrorAction SilentlyContinue)) {
        Write-Output 'Blocked: security static validation requires kubectl with Kustomize.'
        exit 2
    }
    $renderedLines = @(& kubectl kustomize (Join-Path $root 'deploy\kustomize\overlays\poc'))
    if ($LASTEXITCODE -ne 0) { throw 'Security validation failed: Kustomize render failed.' }
    $rendered = $renderedLines -join "`n"
    $runtime = Read-RepositoryFile 'deploy\runtime\sandbox-pod-fragment.yaml'
    $smoke = Read-RepositoryFile 'deploy\smoke\kata-sandbox.yaml'
    $operator = Read-RepositoryFile 'internal\operator\render.go'
    $apiRBAC = Read-RepositoryFile 'deploy\kustomize\base\api-rbac.yaml'
    $brokerRBAC = Read-RepositoryFile 'deploy\kustomize\base\broker-rbac.yaml'
    $allRBAC = @(
        Get-ChildItem (Join-Path $root 'deploy\kustomize\base') -Filter '*rbac.yaml' |
            ForEach-Object { Get-Content -Raw -LiteralPath $_.FullName }
    ) -join "`n"
    $serviceAccounts = Read-RepositoryFile 'deploy\kustomize\base\serviceaccounts.yaml'
    $network = Read-RepositoryFile 'deploy\kustomize\base\networkpolicies.yaml'
    $gateway = Read-RepositoryFile 'deploy\kustomize\base\gateway.yaml'
    $azureNetwork = Read-RepositoryFile 'infra\modules\network.bicep'
    $frontDoorBinding = Read-RepositoryFile 'infra\front-door-binding.bicep'
    $management = Read-RepositoryFile 'deploy\kustomize\base\management.yaml'
    $apiMain = Read-RepositoryFile 'cmd\devsandbox-api\main.go'
    $brokerMain = Read-RepositoryFile 'cmd\devsandbox-broker\main.go'
    $handler = Read-RepositoryFile 'internal\api\handler.go'
    $broker = Read-RepositoryFile 'internal\broker\broker.go'
    $web = Read-RepositoryFile 'internal\gateway\web.go'
    $gatewayTests = Read-RepositoryFile 'internal\gateway\gateway_test.go'
    $brokerTests = Read-RepositoryFile 'internal\broker\broker_test.go'
    $audit = Read-RepositoryFile 'internal\observability\audit.go'
    $auditTests = Read-RepositoryFile 'internal\observability\observability_test.go'
    $jobTests = Read-RepositoryFile 'internal\api\jobs_test.go'
    $credentialTests = Read-RepositoryFile 'internal\supervisor\credentials_test.go'

    Assert-Match $runtime 'runtimeClassName:\s+kata-vm-isolation' 'sandbox runtime fragment does not require Kata.'
    Assert-Match $smoke '(?s)requests:\s+cpu:\s+100m\s+memory:\s+128Mi\s+limits:\s+cpu:\s+100m\s+memory:\s+128Mi' 'Kata smoke requests and limits are not equal.'
    Assert-Match $operator '(?s)"requests".*sandbox\.Spec\.Profile\.CPU.*sandbox\.Spec\.Profile\.Memory.*"limits".*sandbox\.Spec\.Profile\.CPU.*sandbox\.Spec\.Profile\.Memory' 'operator does not render profile CPU/memory requests equal to limits.'
    Assert-Match $operator '"hostNetwork":\s+false' 'operator does not explicitly disable host networking.'
    Assert-Match $operator '"hostPID":\s+false' 'operator does not explicitly disable host PID.'
    Assert-Match $operator '"hostIPC":\s+false' 'operator does not explicitly disable host IPC.'
    Assert-NoMatch $runtime '(?m)^\s*hostPath:' 'sandbox runtime fragment contains hostPath.'
    Assert-NoMatch ($runtime + $operator) '(?i)privileged["'']?\s*[:=]\s*true' 'sandbox containers can be privileged.'
    Assert-Match $runtime 'runAsNonRoot:\s+true' 'sandbox runtime fragment does not require non-root.'
    Assert-Match $runtime 'automountServiceAccountToken:\s+false' 'automatic sandbox token mounting is not disabled.'
    Assert-Match $runtime 'name:\s+devsandbox-\$\{SANDBOX_UID\}' 'sandbox ServiceAccount is not unique per DevSandbox UID.'
    Assert-Match $runtime '(?s)projected:.*serviceAccountToken:.*audience:\s+devsandbox-credential-broker' 'broker projected token has the wrong or missing audience.'
    Assert-Match $runtime '(?s)capabilities:\s+drop:\s+\["ALL"\]' 'sandbox capabilities are not dropped.'
    Assert-Match $runtime '(?s)seccompProfile:\s+type:\s+RuntimeDefault' 'sandbox RuntimeDefault seccomp is absent.'

    Assert-Match $handler 'AuthorizeOwner\(requestClaims\(request\), value\.Spec\.Owner\.GitHubUserID\)' 'gateway/API routes are not owner-authorized.'
    Assert-Match $gateway '(?s)name:\s+api-http.*hostname:\s+api-origin\.example\.internal.*name:\s+web-http.*hostname:\s+web-origin\.example\.internal' 'API and web routes do not use separate internal HTTP origins.'
    Assert-Match $gateway '(?s)name:\s+devsandbox-api.*headers:.*type:\s+Exact.*name:\s+X-Azure-FDID.*value:\s+00000000-0000-0000-0000-000000000000.*name:\s+devsandbox-web.*headers:.*type:\s+Exact.*name:\s+X-Azure-FDID.*value:\s+00000000-0000-0000-0000-000000000000' 'API and web routes do not require the exact Front Door profile header.'
    Assert-NoMatch $rendered '(?m)^kind:\s+ExternalDNS\s*$' 'ExternalDNS remains in the default PoC render.'
    Assert-Match $management 'githubAuthMode:\s+replace-at-deploy' 'management configuration does not declare an explicit GitHub authentication mode.'
    Assert-Match $apiMain 'authMode == "static-validation"' 'API static credential selection is not gated by explicit authentication mode.'
    Assert-Match $brokerMain 'authMode == "static-validation"' 'broker static credential selection is not gated by explicit authentication mode.'
    Assert-Match $web 'const CookieName = "__Host-devsandbox-route"' 'web route cookie lacks host-only scope.'
    Assert-Match $web '(?s)Secure:\s+true,\s+HttpOnly:\s+true,\s+SameSite:\s+http\.SameSiteStrictMode' 'web route cookie flags are incomplete.'
    Assert-Match $gatewayTests 'cookie\.Domain != "" \|\| cookie\.Path != "/"' 'cookie scope lacks a focused test.'

    Assert-NoMatch $apiRBAC 'resources:\s+\[(?i:[^\]]*(?:secrets|nodes|\*))' 'API RBAC grants Secret, node, or wildcard resource access.'
    Assert-Match $apiRBAC 'resources:\s+\[devsandboxes\]' 'API RBAC is not scoped to DevSandbox resources.'
    Assert-NoMatch $allRBAC '(?s)subjects:.*?name:\s+devsandbox-web' 'web ServiceAccount has an RBAC binding.'
    Assert-Match $serviceAccounts '(?s)name:\s+devsandbox-web.*?automountServiceAccountToken:\s+false' 'web automatic token mounting is enabled.'
    Assert-Match $broker '(?s)review\.ServiceAccountUID.*pod\.UID.*pod\.ServiceAccountName.*SandboxUIDLabel.*sandbox\.UID.*sandbox\.OwnerUserID' 'broker identity binding omits SA, Pod, DevSandbox, or owner identity.'
    Assert-Match $brokerRBAC '(?s)resources:\s+\[tokenreviews\]\s+verbs:\s+\[create\]' 'broker TokenReview RBAC is not scoped.'
    Assert-Match $brokerRBAC '(?s)resources:\s+\[serviceaccounts, pods\]\s+verbs:\s+\[get\].*resources:\s+\[devsandboxes\]\s+verbs:\s+\[get\]' 'broker binding reads are broader than required.'
    Assert-NoMatch $brokerRBAC '(?i)resources:\s+\[[^\]]*secrets' 'broker has Kubernetes Secret access.'
    Assert-NoMatch $brokerRBAC '(?s)resources:\s+\[devsandboxes\]\s+verbs:\s+\[[^\]]*(?:create|update|patch|delete)' 'broker can mutate DevSandboxes.'
    Assert-Match $brokerRBAC '(?s)name:\s+devsandbox-broker-leases.*resources:\s+\[leases\]\s+verbs:\s+\[get, create, update\]' 'broker writes are not limited to credential-refresh Leases.'

    Assert-Match $network '(?s)name:\s+default-deny-ingress\s+namespace:\s+devsandbox-system' 'management default-deny ingress is absent.'
    Assert-Match $network '(?s)name:\s+workloads-to-broker.*kubernetes\.io/metadata\.name:\s+devsandbox-workloads.*port:\s+8443' 'workload ingress is not limited to the broker port.'
    Assert-Match $network '(?s)name:\s+gateway-frontdoor-http-and-health.*port:\s+80.*port:\s+15021' 'Gateway policy does not expose only the Front Door HTTP origin and health ports.'
    Assert-Match $azureNetwork "(?s)name:\s+'AllowFrontDoorHttpInbound'.*destinationPortRange:\s+'80'.*sourceAddressPrefix:\s+'AzureFrontDoor\.Backend'" 'AKS subnet NSG does not restrict origin traffic to AzureFrontDoor.Backend.'
    Assert-Match $azureNetwork "(?s)name:\s+'AllowAzureLoadBalancerHealthInbound'.*sourceAddressPrefix:\s+'AzureLoadBalancer'" 'AKS subnet NSG does not allow Azure Load Balancer health access.'
    Assert-NoMatch $azureNetwork "(?s)direction:\s+'Inbound'.*sourceAddressPrefix:\s+'Internet'" 'AKS subnet NSG permits unrestricted Internet ingress.'
    Assert-Match $frontDoorBinding "(?s)name:\s+'api'.*forwardingProtocol:\s+'HttpOnly'.*supportedProtocols:\s+\[\s+'Https'\s+\].*name:\s+'web'.*forwardingProtocol:\s+'HttpOnly'.*supportedProtocols:\s+\[\s+'Https'\s+\]" 'Front Door routes are not HTTPS-only with HTTP origin forwarding.'
    Assert-NoMatch $frontDoorBinding 'cacheConfiguration:' 'Front Door dynamic routes enable caching.'

    Assert-Match $web 'location\.hash\.slice\(1\)' 'one-time credential is not read from a URL fragment.'
    Assert-Match $web 'history\.replaceState\(null,"","/bootstrap"\)' 'one-time credential is not removed from browser history.'
    Assert-Match $web 'fetch\("/exchange",\{method:"POST"' 'one-time credential is not moved in a POST body.'
    Assert-Match $web 'Referrer-Policy", "no-referrer"' 'bootstrap does not suppress credential-bearing Referer headers.'
    Assert-Match $handler 'endpoint\.RawQuery = ""' 'one-time URL could place a credential in the request query.'
    Assert-Match $brokerTests 'caller influenced owner' 'cross-sandbox broker owner isolation lacks a focused test.'
    Assert-Match $brokerTests 'cross-sandbox error' 'sandbox A/B credential binding lacks a focused rejection test.'
    Assert-Match $audit 'cannot represent tokens, commands, process output, or file content' 'audit records are not structurally allow-listed.'
    Assert-Match $auditTests 'TestAuditAllowListAndRedaction' 'log redaction and audit allow-list lack a focused test.'
    Assert-Match $jobTests 'not-persisted' 'command arguments lack a non-persistence test.'
    Assert-Match $credentialTests 'expected workspace path rejection' 'workspace token persistence lacks a focused rejection test.'

    $credentialPattern = '(?i)(gh[pousr]_[A-Za-z0-9_]{20,}|github_pat_[A-Za-z0-9_]{20,}|Bearer\s+[A-Za-z0-9._~-]{20,}|eyJ[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{20,})'
    Assert-NoMatch ($rendered + "`n" + $runtime) $credentialPattern 'rendered resources contain token-like content.'
    Assert-NoMatch $management '(?i)(githubAppClientSecret|refreshToken|accessToken):' 'management manifests persist application credentials.'

    Write-Host 'Passed: static Section 14.5 security assertions.'
    Write-Output 'PoC blocker: sandbox egress is unrestricted; this validation does not claim least-privilege egress.'
    Write-Output 'PoC blocker: raw GitHub tokens are injected into sandbox memory; this validation does not claim token non-disclosure from the sandbox owner.'
    Write-Output 'PoC blocker: Front Door Standard uses a source-restricted HTTP origin; this validation does not claim end-to-end TLS.'
}

function Invoke-DeployedSecurityValidation {
    if (-not (Get-Command az -ErrorAction SilentlyContinue)) {
        Write-Output 'Blocked: deployed security validation requires tool: az'
        exit 2
    }
    $missing = @()
    if ([string]::IsNullOrWhiteSpace($ResourceGroupName)) { $missing += 'DEVSANDBOX_RESOURCE_GROUP' }
    if ([string]::IsNullOrWhiteSpace($AksClusterName)) { $missing += 'DEVSANDBOX_AKS_CLUSTER' }
    if ($missing.Count -gt 0) {
        Write-Output "Blocked: deployed security validation missing inputs: $($missing -join ', ')"
        exit 2
    }

    $command = @'
set -eu
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
if kubectl get devsandboxes,pods -n "$workloads" -o yaml | grep -Eqi 'gh[pousr]_[A-Za-z0-9_]{20,}|github_pat_[A-Za-z0-9_]{20,}|Bearer [A-Za-z0-9._~-]{20,}'; then
  exit 32
fi
echo "deployed Section 14.5 manifest and Pod assertions passed"
'@
    $scriptSource = Resolve-Path (Join-Path $root 'deploy\bootstrap\security-validate.sh')
    $artifactDirectory = Join-Path $root 'artifacts'
    New-Item -ItemType Directory -Force -Path $artifactDirectory | Out-Null
    $scriptPath = Join-Path $artifactDirectory "devsandbox-security-validate-$PID.sh"
    [IO.File]::WriteAllText(
        $scriptPath,
        ([IO.File]::ReadAllText($scriptSource).Replace("`r`n", "`n")),
        [Text.UTF8Encoding]::new($false)
    )
    $scriptName = Split-Path -Leaf $scriptPath
    $command = ". './$scriptName'"
    try {
        $raw = & az aks command invoke --resource-group $ResourceGroupName --name $AksClusterName `
            --file $scriptPath --command $command --only-show-errors --output json
    }
    finally {
        Remove-Item -LiteralPath $scriptPath -Force -ErrorAction SilentlyContinue
    }
    if ($LASTEXITCODE -ne 0) { throw 'Deployed security validation Run Command failed.' }
    $result = ($raw -join "`n") | ConvertFrom-Json
    if ($result.provisioningState -ne 'Succeeded' -or
        ($null -ne $result.exitCode -and [int]$result.exitCode -ne 0)) {
        if (-not [string]::IsNullOrWhiteSpace([string]$result.logs)) {
            Write-Error "Deployed security logs:`n$($result.logs)" -ErrorAction Continue
        }
        throw "Deployed security assertions failed (state=$($result.provisioningState), exit=$($result.exitCode))."
    }
    Write-Host 'Passed: deployed Section 14.5 security assertions.'
    Write-Output 'PoC blocker: sandbox egress remains unrestricted.'
    Write-Output 'PoC blocker: raw GitHub tokens remain available in sandbox memory.'
    Write-Output 'PoC blocker: Front Door Standard origin traffic is source-restricted but not end-to-end TLS encrypted.'
}

Push-Location $root
try {
    if ($Mode -in @('Static', 'All')) { Invoke-StaticSecurityValidation }
    if ($Mode -in @('Deployed', 'All')) { Invoke-DeployedSecurityValidation }
}
finally {
    Pop-Location
}
