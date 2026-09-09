function Invoke-CopilotTemplateScenario {
    $name = $null
    $metadata = Get-Content -Raw -LiteralPath (Join-Path $PSScriptRoot '..\images\versions.json') |
        ConvertFrom-Json
    $identity = Invoke-API Get '/v1/me' $null
    $identityLogin = if ($identity.login) { $identity.login } else { $identity.githubLogin }
    if ([string]::IsNullOrWhiteSpace($identityLogin)) {
        throw 'CopilotTemplate preflight failed: platform session has no GitHub login'
    }

    function Invoke-CopilotExec {
        param([string]$Executable, [string[]]$Arguments, [switch]$AllowFailure)
        $result = Invoke-SandboxExecResult $name $Executable $Arguments
        $stdout = [Text.Encoding]::UTF8.GetString($result.Stdout).Trim()
        $stderr = [Text.Encoding]::UTF8.GetString($result.Stderr).Trim()
        if (-not $AllowFailure -and $result.ExitCode -ne 0) {
            if ($stderr -like 'GitHub Copilot entitlement required:*') {
                throw $stderr
            }
            throw "$Executable failed without exposing command arguments or credentials"
        }
        [PSCustomObject]@{ Stdout = $stdout; Stderr = $stderr; ExitCode = $result.ExitCode }
    }

    try {
        $name = "copilot-e2e-$([Guid]::NewGuid().ToString('N').Substring(0, 8))"
        Invoke-API Post '/v1/sandboxes' @{
            name = $name
            template = 'copilot'
            profile = 'medium'
            idleTimeoutSeconds = 600
            source = @{ type = 'empty' }
        } | Out-Null
        Wait-Sandbox $name @('Running') | Out-Null

        $gitVersion = (Invoke-CopilotExec git @('--version')).Stdout
        if ($gitVersion -notmatch '^git version \d+\.\d+') { throw 'unexpected Git version output' }
        $ghVersion = (Invoke-CopilotExec gh @('--version')).Stdout
        if ($ghVersion -notmatch "^gh version $([regex]::Escape($metadata.tools.githubCli.version))(?:\s|$)") {
            throw 'GitHub CLI does not match images/versions.json'
        }
        $copilotVersion = (Invoke-CopilotExec copilot @('--version')).Stdout
        if ($copilotVersion -notmatch [regex]::Escape($metadata.tools.copilotCli.version)) {
            throw 'Copilot CLI does not match images/versions.json'
        }
        $playwrightHelp = (Invoke-CopilotExec playwright-cli @('--help')).Stdout
        if ($playwrightHelp -notmatch 'playwright-cli') { throw 'Playwright CLI is unavailable' }
        $playwrightVersion = (Invoke-CopilotExec node @(
            '-p', "require('/usr/local/lib/node_modules/@playwright/cli/package.json').version"
        )).Stdout
        if ($playwrightVersion -ne $metadata.tools.playwrightCli.version) {
            throw 'Playwright CLI does not match images/versions.json'
        }

        $actualLogin = (Invoke-CopilotExec gh @('api', 'user', '--jq', '.login')).Stdout
        if (-not [string]::Equals($actualLogin, $identityLogin, [StringComparison]::OrdinalIgnoreCase)) {
            throw 'GitHub CLI authenticated as a different platform user'
        }

        $auth = Invoke-CopilotExec copilot @('-p', 'Reply with exactly: DEVSANDBOX_COPILOT_AUTH_OK')
        if ($auth.ExitCode -ne 0) {
            throw 'Copilot noninteractive authentication failed without starting an interactive login'
        }
        if ($auth.Stdout -notmatch 'DEVSANDBOX_COPILOT_AUTH_OK') {
            throw 'Copilot authenticated but returned an unexpected noninteractive response'
        }
        if (($auth.Stdout + $auth.Stderr) -match 'github\.com/login/device|one-time code') {
            throw 'Copilot attempted an interactive login'
        }

        $browserScript = @'
const { chromium } = require('/usr/local/lib/node_modules/@playwright/cli/node_modules/playwright');
(async () => {
  const browser = await chromium.launch({ headless: true });
  const page = await browser.newPage();
  await page.setContent('<title>DevSandbox Copilot E2E</title>');
  console.log((await page.title()) + '|' + browser.version());
  await browser.close();
})().catch(() => process.exit(1));
'@
        $browser = (Invoke-CopilotExec node @('-e', $browserScript)).Stdout
        $expectedBrowser = "DevSandbox Copilot E2E|$($metadata.tools.playwrightCli.chromiumVersion)"
        if ($browser -ne $expectedBrowser) { throw 'Chromium simple-page validation failed' }

        $runtimeType = (Invoke-CopilotExec stat @('-f', '-c', '%T', '/run/devsandbox')).Stdout
        if ($runtimeType -ne 'tmpfs') { throw 'runtime credentials are not on a memory-backed volume' }
        $scan = Invoke-CopilotExec sh @(
            '-c',
            "if grep -IRIlE 'gh[uops]_[A-Za-z0-9_]+|github_pat_[A-Za-z0-9_]+' /workspace 2>/dev/null | grep -q .; then exit 9; fi"
        ) -AllowFailure
        if ($scan.ExitCode -ne 0) { throw 'a GitHub credential-like value was found on the workspace PVC' }

        Invoke-API Post "/v1/sandboxes/$name/stop" $null | Out-Null
        Wait-Sandbox $name @('Stopped') 300 | Out-Null
        Invoke-API Post "/v1/sandboxes/$name/resume" $null | Out-Null
        Wait-Sandbox $name @('Running') 600 | Out-Null
        $scan = Invoke-CopilotExec sh @(
            '-c',
            "if grep -IRIlE 'gh[uops]_[A-Za-z0-9_]+|github_pat_[A-Za-z0-9_]+' /workspace 2>/dev/null | grep -q .; then exit 9; fi"
        ) -AllowFailure
        if ($scan.ExitCode -ne 0) { throw 'a credential remained on the workspace PVC after stop' }
    }
    finally {
        if ($name) {
            try { Invoke-API Delete "/v1/sandboxes/$name" $null | Out-Null } catch {}
        }
    }
    Write-Output 'Passed: Copilot template tools, Chromium, tmpfs credentials, and stop/resume cleanup'
    Write-Output 'Passed: Copilot noninteractive inference'
}
