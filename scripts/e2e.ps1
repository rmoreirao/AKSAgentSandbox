[CmdletBinding()]
param(
    [ValidateSet('All', 'Baseline', 'RepositoryClone', 'Connectivity', 'ManagedJobIdle', 'Quota', 'Retention', 'VSCode', 'CopilotTemplate')]
    [string]$Scenario = 'All'
)

$ErrorActionPreference = 'Stop'
if ($Scenario -eq 'All') {
    & (Join-Path $PSScriptRoot 'preflight.ps1') -Mode E2E
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
}
$required = @('DEVSANDBOX_API_HOSTNAME', 'DEVSANDBOX_TEST_USER_SESSION')
if ($Scenario -in @('All', 'RepositoryClone')) {
    $required += @('DEVSANDBOX_TEST_ORG', 'DEVSANDBOX_TEST_PRIVATE_REPO', 'DEVSANDBOX_TEST_PUBLIC_REPO')
}
elseif ($Scenario -eq 'VSCode') {
    $required += 'DEVSANDBOX_TEST_PUBLIC_REPO'
}
$missing = @($required | Where-Object { [string]::IsNullOrWhiteSpace([Environment]::GetEnvironmentVariable($_)) })
if ($missing.Count -gt 0) {
    Write-Output "Blocked: missing required environment variables: $($missing -join ', ')"
    exit 2
}

$api = if ([string]::IsNullOrWhiteSpace($env:DEVSANDBOX_API_URL)) {
    "https://$env:DEVSANDBOX_API_HOSTNAME"
}
else {
    $env:DEVSANDBOX_API_URL.TrimEnd('/')
}
$headers = @{ Authorization = "Bearer $env:DEVSANDBOX_TEST_USER_SESSION" }

$headers.Authorization = "Bearer $env:DEVSANDBOX_TEST_USER_SESSION"

$skipTls = [string]::Equals($env:DEVSANDBOX_SKIP_TLS_VERIFY, 'true', [StringComparison]::OrdinalIgnoreCase)
if ($skipTls -or -not $api.StartsWith('https://', [StringComparison]::OrdinalIgnoreCase)) {
    Write-Output 'Blocked: E2E must use the certificate-validated HTTPS Azure Front Door endpoint.'
    exit 2
}
if (-not [string]::IsNullOrWhiteSpace($env:DEVSANDBOX_API_HOST_HEADER)) {
    Write-Output 'Blocked: E2E Host overrides bypass Azure Front Door endpoint validation.'
    exit 2
}
if ($skipTls -and -not ('DevSandboxValidationTls' -as [type])) {
    Add-Type -TypeDefinition @'
using System;
using System.Net.Http;
using System.Net.Security;
using System.Security.Cryptography.X509Certificates;

public static class DevSandboxValidationTls
{
    public static readonly RemoteCertificateValidationCallback WebSocketCallback =
        delegate(object sender, X509Certificate certificate, X509Chain chain, SslPolicyErrors errors) { return true; };

    public static readonly Func<HttpRequestMessage, X509Certificate2, X509Chain, SslPolicyErrors, bool> HttpCallback =
        delegate(HttpRequestMessage request, X509Certificate2 certificate, X509Chain chain, SslPolicyErrors errors) { return true; };
}
'@
}
function Invoke-API {
    param([string]$Method, [string]$Path, [object]$Body)
    $parameters = @{
        Method = $Method
        Uri = "$api$Path"
        Headers = $headers
    }
    if ($null -ne $Body) {
        $parameters.ContentType = 'application/json'
        $parameters.Body = ($Body | ConvertTo-Json -Depth 10 -Compress)
    }
    if ($skipTls) { $parameters.SkipCertificateCheck = $true }
    Invoke-RestMethod @parameters
}

function Get-WebResponseText {
    param([object]$Content)
    if ($Content -is [byte[]]) {
        return [Text.Encoding]::UTF8.GetString($Content)
    }
    return [string]$Content
}

function Invoke-SandboxWebRequest {
    param([string]$Uri, [string]$Body)
    $deadline = [DateTimeOffset]::UtcNow.AddMinutes(5)
    do {
        try {
            return Invoke-WebRequest -Method Post -Uri $Uri -Headers $headers `
                -ContentType 'application/json' -Body $Body -SkipCertificateCheck:$skipTls
        }
        catch {
            $status = [int]$_.Exception.Response.StatusCode
            if ($status -ne 502 -or [DateTimeOffset]::UtcNow -ge $deadline) {
                throw
            }
            Start-Sleep -Seconds 2
        }
    } while ($true)
}

try {
    Invoke-RestMethod -Method Get -Uri "$api/readyz" -Headers $headers -TimeoutSec 15 `
        -SkipCertificateCheck:$skipTls | Out-Null
    Invoke-API Get '/v1/me' $null | Out-Null
    $templates = Invoke-API Get '/v1/templates' $null
    if ($Scenario -in @('All', 'RepositoryClone', 'Connectivity', 'ManagedJobIdle') -and
        'standard' -notin @($templates.items.name)) { throw 'standard template is unavailable' }
    if ($Scenario -in @('All', 'CopilotTemplate') -and
        'copilot' -notin @($templates.items.name)) { throw 'copilot template is unavailable' }
    if ($Scenario -in @('All', 'VSCode')) {
        if ('vscode' -notin @($templates.items.name)) { throw 'vscode template is unavailable' }
        if (-not (Get-Command npx -ErrorAction SilentlyContinue)) { throw 'npx is unavailable' }
        if (-not (Get-Command node -ErrorAction SilentlyContinue)) { throw 'node is unavailable' }
        if (-not (Test-Path (Join-Path $PSScriptRoot '..\playwright.config.ts'))) { throw 'Playwright configuration is unavailable' }
        & npx --no-install playwright --version 2>&1 | Out-Null
        if ($LASTEXITCODE -ne 0) { throw 'Playwright is unavailable' }
        $chromiumPath = (& node -e "process.stdout.write(require('@playwright/test').chromium.executablePath())").Trim()
        if ($LASTEXITCODE -ne 0 -or -not (Test-Path -LiteralPath $chromiumPath)) { throw 'Playwright Chromium is unavailable' }
    }
}
catch {
    Write-Output "Blocked: deployed scenario preflight failed: $($_.Exception.Message)"
    exit 2
}

function Invoke-SandboxExec {
    param([string]$Name, [string]$Executable, [string[]]$Arguments)
    $body = @{ command = @{ executable = $Executable; arguments = $Arguments; directory = '/workspace/repo' } }
    $response = Invoke-SandboxWebRequest "$api/v1/sandboxes/$Name/exec" `
        ($body | ConvertTo-Json -Depth 6 -Compress)
    $output = ''
    foreach ($line in ((Get-WebResponseText $response.Content) -split "`n")) {
        if ([string]::IsNullOrWhiteSpace($line)) { continue }
        $event = $line | ConvertFrom-Json
        if ($event.type -eq 'stdout' -and $event.data) {
            $output += [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($event.data))
        }
        if ($event.type -eq 'error' -or ($event.type -eq 'exit' -and $event.exitCode -ne 0)) {
            throw "sandbox command failed without exposing its arguments"
        }
    }
    $output.Trim()
}

function New-RepositorySandbox {
    param(
        [string]$Repository,
        [string]$Suffix,
        [string]$Template = 'standard',
        [string]$Profile = 'small'
    )
    $encoded = [Uri]::EscapeDataString($Repository)
    $resolved = Invoke-API Get "/v1/repositories/resolve?repository=$encoded" $null
    $name = "repo-e2e-$Suffix-$([Guid]::NewGuid().ToString('N').Substring(0, 8))"
    $sandbox = Invoke-API Post '/v1/sandboxes' @{
        name = $name
        template = $Template
        profile = $Profile
        source = @{
            type = 'git'
            repositoryId = $resolved.repositoryId
            repositoryUrl = $resolved.repositoryUrl
            refName = $resolved.refName
            commitSha = $resolved.commitSha
            lfs = $true
            submodules = $true
            authorName = $resolved.authorName
            authorEmail = $resolved.authorEmail
            primaryOrg = $resolved.primaryOrg
            readOnly = $resolved.readOnly
        }
    }
    $deadline = [DateTimeOffset]::UtcNow.AddMinutes(10)
    while ($sandbox.phase -notin @('Running', 'Failed')) {
        if ([DateTimeOffset]::UtcNow -ge $deadline) { throw "sandbox $name did not become ready" }
        Start-Sleep -Seconds 5
        $sandbox = Invoke-API Get "/v1/sandboxes/$name" $null
    }
    if ($sandbox.phase -ne 'Running') { throw "sandbox $name failed repository initialization" }
    [PSCustomObject]@{ Name = $name; Resolved = $resolved }
}

function Invoke-RepositoryCloneScenario {
    $private = $null
    $public = $null
    $branch = "devsandbox-e2e-$([Guid]::NewGuid().ToString('N').Substring(0, 8))"
    $prCreated = $false
    try {
        $private = New-RepositorySandbox $env:DEVSANDBOX_TEST_PRIVATE_REPO 'private'
        $actual = Invoke-SandboxExec $private.Name git @('rev-parse', 'HEAD')
        if ($actual -ne $private.Resolved.commitSha) { throw 'private fixture was not checked out at its recorded commit' }
        if (-not (Invoke-SandboxExec $private.Name git @('lfs', 'ls-files'))) { throw 'private fixture contains no Git LFS object' }
        Invoke-SandboxExec $private.Name git @('submodule', 'status', '--recursive') | Out-Null
        if (-not (Invoke-SandboxExec $private.Name git @('config', 'user.name')) -or
            -not (Invoke-SandboxExec $private.Name git @('config', 'user.email'))) {
            throw 'Git author identity was not configured'
        }

        Invoke-SandboxExec $private.Name git @('checkout', '-b', $branch) | Out-Null
        Invoke-SandboxExec $private.Name git @('commit', '--allow-empty', '-m', 'DevSandbox repository E2E') | Out-Null
        Invoke-SandboxExec $private.Name git @('push', '--set-upstream', 'origin', $branch) | Out-Null
        Invoke-SandboxExec $private.Name gh @(
            'pr', 'create', '--head', $branch, '--base', $private.Resolved.defaultBranch,
            '--title', 'DevSandbox repository E2E', '--body', 'Temporary automated fixture validation.'
        ) | Out-Null
        $prCreated = $true

        $public = New-RepositorySandbox $env:DEVSANDBOX_TEST_PUBLIC_REPO 'public'
        $expectReadOnly = -not [string]::Equals(
            $env:DEVSANDBOX_EXPECT_PUBLIC_READ_ONLY, 'false', [StringComparison]::OrdinalIgnoreCase
        )
        if ($expectReadOnly -and -not $public.Resolved.readOnly) {
            throw 'external public fixture was not marked read-only'
        }
        if (-not $expectReadOnly -and $public.Resolved.readOnly) {
            throw 'owner public fixture was unexpectedly marked read-only'
        }
        $pushURL = Invoke-SandboxExec $public.Name git @('remote', 'get-url', '--push', 'origin')
        if ($expectReadOnly -and $pushURL -notlike 'devsandbox-read-only://*') {
            throw 'external public fixture has a writable push URL'
        }
    }
    finally {
        if ($private) {
            if ($prCreated) {
                try { Invoke-SandboxExec $private.Name gh @('pr', 'close', $branch, '--delete-branch') | Out-Null } catch {}
            }
            try { Invoke-SandboxExec $private.Name git @('push', 'origin', '--delete', $branch) | Out-Null } catch {}
            try { Invoke-API Delete "/v1/sandboxes/$($private.Name)" $null | Out-Null } catch {}
        }
        if ($public) {
            try { Invoke-API Delete "/v1/sandboxes/$($public.Name)" $null | Out-Null } catch {}
        }
    }
    Write-Output 'Passed: RepositoryClone'
}

function Wait-Sandbox {
    param([string]$Name, [string[]]$Phases, [int]$TimeoutSeconds = 600)
    $deadline = [DateTimeOffset]::UtcNow.AddSeconds($TimeoutSeconds)
    do {
        $sandbox = Invoke-API Get "/v1/sandboxes/$Name" $null
        if ($sandbox.phase -in $Phases) { return $sandbox }
        if ($sandbox.phase -eq 'Failed') { throw "sandbox $Name failed" }
        Start-Sleep -Seconds 5
    } while ([DateTimeOffset]::UtcNow -lt $deadline)
    throw "sandbox $Name did not reach $($Phases -join '/')"
}

function New-EmptySandbox {
    param([string]$Prefix, [int]$IdleSeconds)
    $lastError = $null
    foreach ($attempt in 1..2) {
        $name = "$Prefix-$([Guid]::NewGuid().ToString('N').Substring(0, 8))"
        try {
            Invoke-API Post '/v1/sandboxes' @{
                name = $name
                template = 'standard'
                profile = 'small'
                idleTimeoutSeconds = $IdleSeconds
                source = @{ type = 'empty' }
            } | Out-Null
            Wait-Sandbox $name @('Running') 300 | Out-Null
            return $name
        }
        catch {
            $lastError = $_
            try { Invoke-API Delete "/v1/sandboxes/$name" $null | Out-Null } catch {}
            if ($attempt -lt 2) { Start-Sleep -Seconds 10 }
        }
    }
    throw $lastError
}

function Invoke-BaselineScenario {
    $name = $null
    try {
        $name = New-EmptySandbox 'baseline-e2e' 600
        $marker = "mvp15-$([Guid]::NewGuid().ToString('N'))"
        $result = Invoke-SandboxExecResult $name sh @(
            '-c', "printf '%s' '$marker' > /workspace/mvp15-marker && cat /workspace/mvp15-marker"
        )
        if ($result.ExitCode -ne 0 -or [Text.Encoding]::UTF8.GetString($result.Stdout) -ne $marker) {
            throw 'empty standard sandbox exec marker failed'
        }
        $list = Invoke-API Get '/v1/sandboxes' $null
        if ($name -notin @($list.items.name)) { throw 'created sandbox is absent from list' }
        $status = Invoke-API Get "/v1/sandboxes/$name" $null
        if ($status.phase -ne 'Running' -or $status.template.name -ne 'standard') {
            throw 'sandbox status did not report a running standard template'
        }

        if (-not [string]::IsNullOrWhiteSpace($env:DEVSANDBOX_TEST_SECOND_USER_SESSION)) {
            $otherHeaders = @{ Authorization = "Bearer $env:DEVSANDBOX_TEST_SECOND_USER_SESSION" }
            $isolated = $false
            if (-not [string]::IsNullOrWhiteSpace($env:DEVSANDBOX_API_HOST_HEADER)) {
                $otherHeaders.Host = $env:DEVSANDBOX_API_HOST_HEADER
            }
            try {
                Invoke-RestMethod -Method Get -Uri "$api/v1/sandboxes/$name" -Headers $otherHeaders `
                    -SkipCertificateCheck:$skipTls | Out-Null
            }
            catch {
                $isolated = $_.Exception.Response.StatusCode.value__ -eq 404
            }
            if (-not $isolated) { throw 'a second user could inspect another owner sandbox' }
        }

        Invoke-API Post "/v1/sandboxes/$name/stop" $null | Out-Null
        $stopped = Wait-Sandbox $name @('Stopped') 300
        if (-not $stopped.stoppedAt -or -not $stopped.retentionDeadline -or
            [DateTimeOffset]$stopped.retentionDeadline -le [DateTimeOffset]$stopped.stoppedAt) {
            throw 'stopped sandbox did not publish a future retention deadline'
        }
        Invoke-API Post "/v1/sandboxes/$name/resume" $null | Out-Null
        Wait-Sandbox $name @('Running') 600 | Out-Null
        $result = Invoke-SandboxExecResult $name cat @('/workspace/mvp15-marker')
        if ($result.ExitCode -ne 0 -or [Text.Encoding]::UTF8.GetString($result.Stdout) -ne $marker) {
            throw 'workspace marker did not persist across stop/resume'
        }
    }
    finally {
        if ($name) { try { Invoke-API Delete "/v1/sandboxes/$name" $null | Out-Null } catch {} }
    }
    Write-Output 'Passed: Baseline (/me, templates, empty standard, exec, list/status, stop/resume persistence)'
}

function Invoke-RetentionScenario {
    $name = $null
    try {
        $name = New-EmptySandbox 'retention-e2e' 600
        Invoke-API Post "/v1/sandboxes/$name/stop" $null | Out-Null
        $stopped = Wait-Sandbox $name @('Stopped') 300
        $stoppedAt = [DateTimeOffset]$stopped.stoppedAt
        $deadline = [DateTimeOffset]$stopped.retentionDeadline
        if ($deadline -le $stoppedAt -or $deadline -lt [DateTimeOffset]::UtcNow.AddDays(6)) {
            throw 'default stopped retention was not demonstrated'
        }
    }
    finally {
        if ($name) { try { Invoke-API Delete "/v1/sandboxes/$name" $null | Out-Null } catch {} }
    }
    Write-Output 'Passed: Retention'
}

function Invoke-QuotaScenario {
    $names = 1..4 | ForEach-Object { "quota-e2e-$_-$([Guid]::NewGuid().ToString('N').Substring(0, 8))" }
    $httpHandler = [Net.Http.HttpClientHandler]::new()
    if ($skipTls) {
        $httpHandler.ServerCertificateCustomValidationCallback = [DevSandboxValidationTls]::HttpCallback
    }
    $client = [Net.Http.HttpClient]::new($httpHandler)
    if (-not [string]::IsNullOrWhiteSpace($env:DEVSANDBOX_API_HOST_HEADER)) {
        $client.DefaultRequestHeaders.Host = $env:DEVSANDBOX_API_HOST_HEADER
    }
    $client.DefaultRequestHeaders.Authorization =
        [Net.Http.Headers.AuthenticationHeaderValue]::new('Bearer', $env:DEVSANDBOX_TEST_USER_SESSION)
    try {
        $tasks = @()
        foreach ($name in $names) {
            $body = @{
                name = $name
                template = 'standard'
                profile = 'small'
                idleTimeoutSeconds = 600
                source = @{ type = 'empty' }
            } | ConvertTo-Json -Depth 5 -Compress
            $content = [Net.Http.StringContent]::new($body, [Text.Encoding]::UTF8, 'application/json')
            $tasks += $client.PostAsync("$api/v1/sandboxes", $content)
        }
        [Threading.Tasks.Task]::WaitAll([Threading.Tasks.Task[]]$tasks)
        foreach ($task in $tasks) {
            $response = $task.Result
            try {
                if ([int]$response.StatusCode -ne 201) {
                    throw 'a concurrent quota create request was not accepted for reconciliation'
                }
            }
            finally { $response.Dispose() }
        }

        $deadline = [DateTimeOffset]::UtcNow.AddMinutes(12)
        do {
            $running = 0
            $rejected = 0
            foreach ($name in $names) {
                $sandbox = Invoke-API Get "/v1/sandboxes/$name" $null
                if ($sandbox.phase -eq 'Running') { $running++ }
                if (@($sandbox.conditions | Where-Object {
                    $_.type -eq 'QuotaReady' -and $_.reason -eq 'QuotaExceeded'
                }).Count -gt 0) { $rejected++ }
            }
            if ($running -eq 3 -and $rejected -eq 1) { break }
            Start-Sleep -Seconds 5
        } while ([DateTimeOffset]::UtcNow -lt $deadline)
        if ($running -ne 3 -or $rejected -ne 1) {
            throw "concurrent active quota result was running=$running rejected=$rejected; expected 3/1"
        }
    }
    finally {
        $client.Dispose()
        foreach ($name in $names) {
            try { Invoke-API Delete "/v1/sandboxes/$name" $null | Out-Null } catch {}
        }
    }
    Write-Output 'Passed: Quota (four concurrent creates produced three active reservations and one rejection)'
}

function Invoke-SandboxExecResult {
    param([string]$Name, [string]$Executable, [string[]]$Arguments)
    $body = @{ command = @{ executable = $Executable; arguments = $Arguments; directory = '/workspace' } }
    $response = Invoke-SandboxWebRequest "$api/v1/sandboxes/$Name/exec" `
        ($body | ConvertTo-Json -Depth 6 -Compress)
    $stdout = [IO.MemoryStream]::new()
    $stderr = [IO.MemoryStream]::new()
    $exitCode = $null
    foreach ($line in ((Get-WebResponseText $response.Content) -split "`n")) {
        if ([string]::IsNullOrWhiteSpace($line)) { continue }
        $event = $line | ConvertFrom-Json
        if ($event.data) {
            $data = [Convert]::FromBase64String($event.data)
            if ($event.type -eq 'stdout') { $stdout.Write($data, 0, $data.Length) }
            if ($event.type -eq 'stderr') { $stderr.Write($data, 0, $data.Length) }
        }
        if ($event.type -eq 'error') { throw 'sandbox execution transport failed' }
        if ($event.type -eq 'exit') { $exitCode = [int]$event.exitCode }
    }
    [PSCustomObject]@{ Stdout = $stdout.ToArray(); Stderr = $stderr.ToArray(); ExitCode = $exitCode }
}

function New-AuthenticatedWebSocket {
    param([string]$Path)
    $socket = [Net.WebSockets.ClientWebSocket]::new()
    if (-not [string]::IsNullOrWhiteSpace($env:DEVSANDBOX_WEBSOCKET_PROXY)) {
        $socket.Options.Proxy = [Net.WebProxy]::new($env:DEVSANDBOX_WEBSOCKET_PROXY)
    }
    else {
        $socket.Options.Proxy = $null
    }
    if ($skipTls) {
        $socket.Options.RemoteCertificateValidationCallback = [DevSandboxValidationTls]::WebSocketCallback
    }
    if (-not [string]::IsNullOrWhiteSpace($env:DEVSANDBOX_API_HOST_HEADER)) {
        $socket.Options.SetRequestHeader('Host', $env:DEVSANDBOX_API_HOST_HEADER)
    }
    $socket.Options.SetRequestHeader('Authorization', "Bearer $env:DEVSANDBOX_TEST_USER_SESSION")
    $socket.Options.SetRequestHeader('Authorization', "Bearer $env:DEVSANDBOX_TEST_USER_SESSION")
    $webSocketBase = if ($env:DEVSANDBOX_WEBSOCKET_URL) {
        $env:DEVSANDBOX_WEBSOCKET_URL.TrimEnd('/')
    }
    else {
        $api -replace '^https:', 'wss:'
    }
    $uri = [Uri]::new($webSocketBase + $Path)
    $socket.ConnectAsync($uri, [Threading.CancellationToken]::None).GetAwaiter().GetResult() | Out-Null
    $socket
}

function Send-WebSocket {
    param(
        [Net.WebSockets.ClientWebSocket]$Socket,
        [byte[]]$Data,
        [Net.WebSockets.WebSocketMessageType]$Type
    )
    $segment = [ArraySegment[byte]]::new($Data)
    $Socket.SendAsync($segment, $Type, $true, [Threading.CancellationToken]::None).GetAwaiter().GetResult() | Out-Null
}

function Receive-WebSocketMessage {
    param([Net.WebSockets.ClientWebSocket]$Socket)
    $stream = [IO.MemoryStream]::new()
    $buffer = [byte[]]::new(65536)
    do {
        $segment = [ArraySegment[byte]]::new($buffer)
        $result = $Socket.ReceiveAsync($segment, [Threading.CancellationToken]::None).GetAwaiter().GetResult()
        if ($result.MessageType -eq [Net.WebSockets.WebSocketMessageType]::Close) {
            throw 'WebSocket closed before the expected result'
        }
        $stream.Write($buffer, 0, $result.Count)
    } while (-not $result.EndOfMessage)
    [PSCustomObject]@{ Type = $result.MessageType; Data = $stream.ToArray() }
}

function Invoke-ConnectivityScenario {
    $name = $null
    $job = $null
    try {
        $name = New-EmptySandbox 'connect-e2e' 600
        $result = Invoke-SandboxExecResult $name sh @('-c', "printf 'text'; printf '\000\377raw'; printf 'err' >&2; exit 23")
        if ($result.ExitCode -ne 23) { throw 'remote exit code was not preserved' }
        if ([Text.Encoding]::UTF8.GetString([byte[]]$result.Stdout[0..3]) -ne 'text') { throw 'text stdout was corrupted' }
        if ($result.Stdout.Length -lt 9 -or $result.Stdout[4] -ne 0 -or $result.Stdout[5] -ne 255) {
            throw 'binary stdout was corrupted'
        }
        if ([Text.Encoding]::UTF8.GetString($result.Stderr) -ne 'err') { throw 'stderr was corrupted' }

        $shell = New-AuthenticatedWebSocket "/v1/sandboxes/$name/shell"
        try {
            Send-WebSocket $shell ([Text.Encoding]::UTF8.GetBytes('{"type":"resize","rows":41,"cols":132}')) Text
            Send-WebSocket $shell ([byte[]](3)) Binary
            Send-WebSocket $shell ([Text.Encoding]::UTF8.GetBytes("printf shell-ok`nexit`n")) Binary
            $shellOutput = ''
            do {
                $message = Receive-WebSocketMessage $shell
                $event = [Text.Encoding]::UTF8.GetString($message.Data) | ConvertFrom-Json
                if ($event.type -eq 'stdout' -and $event.data) {
                    $shellOutput += [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($event.data))
                }
            } while ($event.type -ne 'exit')
            if ($shellOutput -notlike '*shell-ok*') { throw 'shell stream or resize failed' }
        }
        finally { $shell.Dispose() }

        $remotePort = Get-Random -Minimum 30000 -Maximum 45000
        $echoScript = 'fifo=/tmp/devsandbox-echo; rm -f "$fifo"; mkfifo "$fifo"; ' +
            'cat "$fifo" | nc -l 127.0.0.1 ' + $remotePort + ' > "$fifo"; rm -f "$fifo"'
        $job = Invoke-API Post "/v1/sandboxes/$name/exec" @{
            detach = $true
            command = @{ executable = 'sh'; arguments = @('-c', $echoScript); directory = '/workspace' }
        }
        Start-Sleep -Seconds 2
        $tunnel = New-AuthenticatedWebSocket "/v1/sandboxes/$name/ports/$remotePort"
        try {
            $payload = [byte[]](0, 255, 1, 13, 10, 78, 79, 84, 32, 72, 84, 84, 80)
            Send-WebSocket $tunnel $payload Binary
            $echoed = Receive-WebSocketMessage $tunnel
            if ($echoed.Type -ne [Net.WebSockets.WebSocketMessageType]::Binary -or
                [Convert]::ToBase64String($echoed.Data) -ne [Convert]::ToBase64String($payload)) {
                throw 'raw tunnel bytes were corrupted'
            }
        }
        finally { $tunnel.Dispose() }
    }
    finally {
        if ($name -and $job) {
            try { Invoke-API Delete "/v1/sandboxes/$name/jobs/$($job.id)" $null | Out-Null } catch {}
        }
        if ($name) { try { Invoke-API Delete "/v1/sandboxes/$name" $null | Out-Null } catch {} }
    }
    Write-Output 'Passed: Connectivity'
}

function Invoke-ManagedJobIdleScenario {
    $name = $null
    $job = $null
    try {
        $name = New-EmptySandbox 'job-idle-e2e' 20
        $job = Invoke-API Post "/v1/sandboxes/$name/exec" @{
            detach = $true
            command = @{ executable = 'sleep'; arguments = @('120'); directory = '/workspace' }
        }
        Start-Sleep -Seconds 35
        $sandbox = Invoke-API Get "/v1/sandboxes/$name" $null
        if ($sandbox.phase -ne 'Running') { throw 'active managed job did not suppress idle stop' }
        Invoke-API Delete "/v1/sandboxes/$name/jobs/$($job.id)" $null | Out-Null
        $job = $null
        Wait-Sandbox $name @('Stopped') 180 | Out-Null
    }
    finally {
        if ($name -and $job) {
            try { Invoke-API Delete "/v1/sandboxes/$name/jobs/$($job.id)" $null | Out-Null } catch {}
        }
        if ($name) { try { Invoke-API Delete "/v1/sandboxes/$name" $null | Out-Null } catch {} }
    }
    Write-Output 'Passed: ManagedJobIdle'
}

function Invoke-VSCodeScenario {
    $sandbox = $null
    $previousURL = [Environment]::GetEnvironmentVariable('DEVSANDBOX_VSCODE_URL', 'Process')
    $previousRepository = [Environment]::GetEnvironmentVariable('DEVSANDBOX_VSCODE_REPOSITORY', 'Process')
    $previousHTMLReport = [Environment]::GetEnvironmentVariable('PLAYWRIGHT_HTML_OPEN', 'Process')
    try {
        $sandbox = New-RepositorySandbox $env:DEVSANDBOX_TEST_PUBLIC_REPO 'vscode' 'vscode' 'medium'
        $issued = Invoke-API Post "/v1/sandboxes/$($sandbox.Name)/vscode-url" @{}
        if ([string]::IsNullOrWhiteSpace($issued.url)) { throw 'VS Code URL issuance returned no URL' }
        try { $parsed = [Uri]$issued.url }
        catch { throw 'VS Code URL issuance returned a malformed URL' }
        if ($parsed.Scheme -ne 'https' -or $parsed.AbsolutePath -ne '/bootstrap' -or
            -not $parsed.Fragment.StartsWith('#credential=') -or $parsed.Query) {
            throw 'VS Code URL did not use the fragment-only bootstrap contract'
        }

        $env:DEVSANDBOX_VSCODE_URL = $issued.url
        $env:DEVSANDBOX_VSCODE_REPOSITORY = 'repo'
        $env:PLAYWRIGHT_HTML_OPEN = 'never'
        $testOutput = @(& npx --no-install playwright test --project=chromium 2>&1)
        $testExitCode = $LASTEXITCODE
        foreach ($line in $testOutput) {
            $sanitized = ([string]$line).Replace($issued.url, '[redacted one-time URL]')
            $sanitized = [regex]::Replace($sanitized, 'credential=[A-Za-z0-9_-]+', 'credential=[redacted]')
            Write-Output $sanitized
        }
        if ($testExitCode -ne 0) { throw "Playwright VS Code smoke test failed with exit code $testExitCode" }
    }
    finally {
        [Environment]::SetEnvironmentVariable('DEVSANDBOX_VSCODE_URL', $previousURL, 'Process')
        [Environment]::SetEnvironmentVariable('DEVSANDBOX_VSCODE_REPOSITORY', $previousRepository, 'Process')
        [Environment]::SetEnvironmentVariable('PLAYWRIGHT_HTML_OPEN', $previousHTMLReport, 'Process')
        if ($sandbox) {
            try { Invoke-API Delete "/v1/sandboxes/$($sandbox.Name)" $null | Out-Null } catch {}
        }
    }
    Write-Output 'Passed: VSCode'
}

try {
    if ($Scenario -in @('All', 'Baseline')) {
        Invoke-BaselineScenario
    }
    if ($Scenario -in @('All', 'RepositoryClone')) {
        Invoke-RepositoryCloneScenario
    }
    if ($Scenario -in @('All', 'Connectivity')) {
        Invoke-ConnectivityScenario
    }
    if ($Scenario -in @('All', 'ManagedJobIdle')) {
        Invoke-ManagedJobIdleScenario
    }
    if ($Scenario -in @('All', 'Retention')) {
        Invoke-RetentionScenario
    }
    if ($Scenario -in @('All', 'VSCode')) {
        Invoke-VSCodeScenario
    }
    if ($Scenario -in @('All', 'CopilotTemplate')) {
        . (Join-Path $PSScriptRoot 'e2e-copilot.ps1')
        Invoke-CopilotTemplateScenario
    }
    if ($Scenario -in @('All', 'Quota')) {
        Invoke-QuotaScenario
    }
}
finally {
    & (Join-Path $PSScriptRoot 'cleanup-e2e.ps1') -Method API
}
