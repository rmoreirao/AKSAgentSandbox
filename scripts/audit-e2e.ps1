[CmdletBinding()]
param(
    [Parameter(Mandatory)]
    [string]$WorkspaceId,
    [ValidateRange(5, 1440)]
    [int]$LookbackMinutes = 30,

    [switch]$RequireManualEvents
)

$ErrorActionPreference = 'Stop'

if (-not (Get-Command az -ErrorAction SilentlyContinue)) {
    throw 'Azure CLI is required.'
}

function Invoke-AnalyticsQuery {
    param([Parameter(Mandatory)][string]$Query)
    $singleLine = (($Query -split "`r?`n") | ForEach-Object { $_.Trim() }) -join ' '
    $json = (& az monitor log-analytics query --workspace $WorkspaceId `
        --analytics-query $singleLine --output json --only-show-errors) -join "`n"
    if ($LASTEXITCODE -ne 0) {
        throw 'Log Analytics query failed.'
    }
    return @($json | ConvertFrom-Json)
}

$required = @(
    'sandbox.create',
    'sandbox.ready',
    'sandbox.stop',
    'sandbox.resume',
    'sandbox.delete',
    'template.selected',
    'profile.selected',
    'repository.selected',
    'commit.selected',
    'session.start',
    'session.end',
    'quota.rejection',
    'credential.refresh'
)
if ($RequireManualEvents) {
    $required += 'login'
}
$eventList = ($required | ForEach-Object { "'$_'" }) -join ','
$query = @"
ContainerLogV2
| where TimeGenerated > ago(${LookbackMinutes}m)
| extend audit=parse_json(tostring(LogMessage))
| where tostring(audit.msg) == 'audit'
| summarize count() by event=tostring(audit.event)
| where event in ($eventList)
"@

$rows = Invoke-AnalyticsQuery $query
$found = @($rows | ForEach-Object { $_.event })
$missing = @($required | Where-Object { $_ -notin $found })
if ($missing.Count -gt 0) {
    throw "Expected audit events were not found: $($missing -join ', ')"
}

$sessionQuery = @"
ContainerLogV2
| where TimeGenerated > ago(${LookbackMinutes}m)
| extend audit=parse_json(tostring(LogMessage))
| where tostring(audit.msg) == 'audit' and tostring(audit.event) in ('session.start', 'session.end')
| summarize starts=countif(tostring(audit.event) == 'session.start'),
            ends=countif(tostring(audit.event) == 'session.end') by session=tostring(audit.session)
| where starts > 0 and ends > 0
"@
$sessionRows = Invoke-AnalyticsQuery $sessionQuery
$sessionKinds = @($sessionRows | ForEach-Object { $_.session })
$missingSessions = @('shell', 'vscode', 'exec', 'tunnel', 'job') | Where-Object { $_ -notin $sessionKinds }
if ($missingSessions.Count -gt 0) {
    throw "Expected session start/end pairs were not found: $($missingSessions -join ', ')"
}

$unsafeQuery = @"
ContainerLogV2
| where TimeGenerated > ago(${LookbackMinutes}m)
| extend audit=parse_json(tostring(LogMessage))
| where tostring(audit.msg) == 'audit'
| where bag_has_key(audit, 'token') or bag_has_key(audit, 'refresh_token')
    or bag_has_key(audit, 'command') or bag_has_key(audit, 'stdout')
    or bag_has_key(audit, 'stderr') or bag_has_key(audit, 'output')
    or bag_has_key(audit, 'file_content')
| count
"@
$unsafe = Invoke-AnalyticsQuery $unsafeQuery
if (@($unsafe).Count -gt 0 -and [int]$unsafe[0].Count -ne 0) {
    throw 'Audit records contain a forbidden field.'
}

Write-Host 'Audit E2E passed: lifecycle events are queryable and forbidden fields are absent.'
