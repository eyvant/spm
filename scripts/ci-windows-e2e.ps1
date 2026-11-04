[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$Spm,
    [Parameter(Mandatory)][ValidateSet('User', 'System')][string]$Scope,
    [string]$Prefix = "ci-$PID"
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$Spm = [IO.Path]::GetFullPath($Spm)
$repo = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
$system = $Scope -eq 'System'
$scopeArgs = if ($system) { @('--system') } else { @() }
$prefix = ($Prefix.ToLowerInvariant() -replace '[^a-z0-9-]', '-')
$main = "$prefix-$($Scope.ToLowerInvariant())-main"
$dependency = "$prefix-$($Scope.ToLowerInvariant())-dependency"
$dependent = "$prefix-$($Scope.ToLowerInvariant())-dependent"
$declared = "$prefix-$($Scope.ToLowerInvariant())-declared"
$scheduled = "$prefix-$($Scope.ToLowerInvariant())-scheduled"
$scheduledDeclared = "$prefix-$($Scope.ToLowerInvariant())-scheduled-declared"
$crashing = "$prefix-user-crashing"
$names = @($main, $dependency, $dependent, $declared, $scheduled, $scheduledDeclared, $crashing)
$oldData = [Environment]::GetEnvironmentVariable('SPM_WINDOWS_DATA_DIR', 'Process')
$tempRoot = if ($env:RUNNER_TEMP) { $env:RUNNER_TEMP } else { Join-Path $repo '.ci-tmp' }
New-Item -ItemType Directory -Path $tempRoot -Force | Out-Null
$data = if ($system) { Join-Path $env:ProgramData "$prefix-spm-data" } else { Join-Path $tempRoot "$prefix-user-data" }
$toml = Join-Path $tempRoot "$prefix-$($Scope.ToLowerInvariant()).toml"
$scheduleToml = Join-Path $tempRoot "$prefix-$($Scope.ToLowerInvariant())-schedule.toml"
$workload = Join-Path $repo 'scripts\spm-test-service.ps1'
$workloadDir = $null
$scheduleMarker = $null

if ($system) {
    $principal = [Security.Principal.WindowsPrincipal]::new([Security.Principal.WindowsIdentity]::GetCurrent())
    if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
        throw 'System integration requires an elevated runner.'
    }
    $workloadDir = Join-Path $env:ProgramData "$prefix-spm-ci"
    New-Item -ItemType Directory -Path $workloadDir -Force | Out-Null
    & icacls.exe $workloadDir /inheritance:r /grant:r '*S-1-5-18:(OI)(CI)F' '*S-1-5-19:(OI)(CI)M' '*S-1-5-20:(OI)(CI)M' '*S-1-5-32-544:(OI)(CI)F' /Q | Out-Null
    if ($LASTEXITCODE -ne 0) { throw 'Failed to set workload directory ACLs.' }
    $workload = Join-Path $workloadDir 'spm-test-service.ps1'
    Copy-Item -LiteralPath (Join-Path $repo 'scripts\spm-test-service.ps1') -Destination $workload -Force
    $systemSpm = Join-Path $workloadDir 'spm-ci.exe'
    Copy-Item -LiteralPath $Spm -Destination $systemSpm -Force
    $Spm = $systemSpm
    $scheduleMarker = Join-Path $workloadDir 'schedule-marker.txt'
} else {
    $scheduleMarker = Join-Path $tempRoot "$prefix-schedule-marker.txt"
}

function Invoke-Spm {
    param([Parameter(Mandatory)][string[]]$Arguments, [switch]$Capture)
    # Keep the global scope prefix and command arguments in one native argv.
    $commandArguments = @($script:scopeArgs) + @($Arguments)
    $output = & $script:Spm @commandArguments 2>&1
    $code = $LASTEXITCODE
    if ($code -ne 0) {
        throw "spm failed ($code): $($commandArguments -join ' ')`n$($output -join [Environment]::NewLine)"
    }
    if (-not $Capture) { $output | ForEach-Object { Write-Host $_ } }
    return $output
}

function Remove-ServiceSafe([string]$Name) {
    $stopArguments = @($script:scopeArgs) + @('stop', $Name)
    $deleteArguments = @($script:scopeArgs) + @('delete', '--yes', $Name)
    & $script:Spm @stopArguments 2>$null | Out-Null
    & $script:Spm @deleteArguments 2>$null | Out-Null
    if ($script:system) { & sc.exe delete "spm-$Name" 2>$null | Out-Null }
}

function Get-SpmRow([string]$Name) {
    $json = (Invoke-Spm @('list', '--json') -Capture) -join [Environment]::NewLine
    return @($json | ConvertFrom-Json) | Where-Object name -eq $Name
}

function Wait-Running([string]$Name) {
    for ($attempt = 0; $attempt -lt 40; $attempt++) {
        $row = Get-SpmRow $Name
        if ($null -ne $row -and [int]$row.pid -gt 0) { return }
        Start-Sleep -Milliseconds 250
    }
    throw "$Name did not reach running state."
}

function Wait-Log([string]$Name, [string]$Pattern) {
    for ($attempt = 0; $attempt -lt 40; $attempt++) {
        try {
            $log = (Invoke-Spm @('logs', '--lines', '100', '--grep', $Pattern, $Name) -Capture) -join "`n"
            if ($log -match [regex]::Escape($Pattern)) { return }
        } catch {}
        Start-Sleep -Milliseconds 250
    }
    throw "$Name did not log $Pattern."
}

function Wait-File([string]$Path, [int]$TimeoutSeconds = 90) {
    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    while ([DateTime]::UtcNow -lt $deadline) {
        if (Test-Path -LiteralPath $Path) { return }
        Start-Sleep -Seconds 1
    }
    throw "Timed out waiting for $Path"
}

function Wait-ScheduleSuccess([string]$Name, [int]$TimeoutSeconds = 20) {
    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    $lastStatus = ''
    while ([DateTime]::UtcNow -lt $deadline) {
        $lastStatus = (Invoke-Spm @('status', $Name) -Capture) -join "`n"
        if ($lastStatus -match 'status\s+waiting' -and $lastStatus -match 'last exit code\s+0') {
            return $lastStatus
        }
        Start-Sleep -Milliseconds 250
    }
    throw "Scheduled task state did not settle after $TimeoutSeconds seconds: $lastStatus"
}

function Write-ScheduleDiagnostics([string]$Name) {
    $taskName = "spm-$Name-schedule"
    Write-Warning "Scheduled task diagnostics for $taskName"
    $taskText = try { Get-ScheduledTask -TaskName $taskName | Select-Object TaskName,State | Format-List | Out-String } catch { $_ | Out-String }
    $infoText = try { Get-ScheduledTaskInfo -TaskName $taskName | Select-Object LastRunTime,LastTaskResult,NextRunTime,NumberOfMissedRuns | Format-List | Out-String } catch { $_ | Out-String }
    $statusText = try { (Invoke-Spm @('status', $Name) -Capture) -join [Environment]::NewLine } catch { $_ | Out-String }
    $logText = try { (Invoke-Spm @('logs', '--lines', '200', $Name) -Capture) -join [Environment]::NewLine } catch { $_ | Out-String }
    $diagnostics = "TASK`n$taskText`nINFO`n$infoText`nSTATUS`n$statusText`nLOG`n$logText"
    Write-Host $diagnostics
    if ($env:GITHUB_ACTIONS -eq 'true') {
        $encoded = $diagnostics.Replace('%', '%25').Replace("`r", '%0D').Replace("`n", '%0A')
        Write-Host "::error title=Scheduled task diagnostics::$encoded"
    }
}

function Start-Workload([string]$Name, [string]$Account = '') {
    $arguments = @('start', $script:workload, '--name', $Name, '--no-enable', '--restart', 'no', '--cwd', (Split-Path $script:workload))
    if ($script:system -and $Account) { $arguments += @('--user', $Account) }
    Invoke-Spm $arguments | Out-Null
    Wait-Running $Name
}

try {
    [Environment]::SetEnvironmentVariable('SPM_WINDOWS_DATA_DIR', $data, 'Process')
    foreach ($name in $names) { Remove-ServiceSafe $name }

    Write-Host "==> $Scope lifecycle, PowerShell inference, logs, and native inspection"
    Start-Workload $main $(if ($system) { 'LocalService' } else { '' })
    Wait-Log $main 'READY'
    Invoke-Spm @('status', $main) | Out-Null
    Invoke-Spm @('native', 'windows', 'inspect', $main) | Out-Null
    Invoke-Spm @('restart', $main) | Out-Null
    Wait-Running $main
    if (-not $system -and $env:SPM_CI_SKIP_STARTUP -eq '1') {
        Write-Host 'Skipping Startup-folder assertion in the local sandbox.'
    } else {
        Invoke-Spm @('enable', $main) | Out-Null
    }
    if ($system) {
        $native = Get-CimInstance Win32_Service -Filter "Name='spm-$main'"
        if ($native.State -ne 'Running' -or $native.StartMode -ne 'Auto' -or $native.StartName -notmatch 'LocalService') {
            throw "Unexpected SCM state: $($native | Out-String)"
        }
    } elseif ($env:SPM_CI_SKIP_STARTUP -ne '1') {
        $shortcut = Join-Path $env:APPDATA "Microsoft\Windows\Start Menu\Programs\Startup\spm-$main.lnk"
        if (-not (Test-Path -LiteralPath $shortcut)) { throw 'Startup shortcut was not created.' }
    }
    Invoke-Spm @('disable', $main) | Out-Null

    Write-Host "==> $Scope dependency orchestration"
    Start-Workload $dependency $(if ($system) { 'LocalService' } else { '' })
    Invoke-Spm @('stop', $dependency) | Out-Null
    $arguments = @('start', $workload, '--name', $dependent, '--no-enable', '--restart', 'no', '--cwd', (Split-Path $workload), '--requires', $dependency, '--after', $dependency)
    if ($system) { $arguments += @('--user', 'LocalService') }
    Invoke-Spm $arguments | Out-Null
    Wait-Running $dependency
    Wait-Running $dependent
    if ($system) {
        $qc = & sc.exe qc "spm-$dependent" 2>&1 | Out-String
        if ($LASTEXITCODE -ne 0 -or $qc -notmatch [regex]::Escape("spm-$dependency")) { throw "SCM dependency missing: $qc" }
        Invoke-Spm @('stop', $dependent) | Out-Null
        Invoke-Spm @('stop', $dependency) | Out-Null
        Start-Service -Name "spm-$dependent"
        (Get-Service -Name "spm-$dependent").WaitForStatus('Running', [TimeSpan]::FromSeconds(20))
        (Get-Service -Name "spm-$dependency").WaitForStatus('Running', [TimeSpan]::FromSeconds(20))
    }

    Write-Host "==> $Scope declarative validate/apply/diff"
    $ps = (Get-Command powershell.exe).Source.Replace('\', '\\')
    $cwd = (Split-Path $workload).Replace('\', '\\')
    $scriptPath = $workload.Replace('\', '\\')
    $accountLine = if ($system) { 'user = "LocalService"' } else { '' }
    $document = @"
schema = 1
[[services]]
name = "$declared"
cwd = "$cwd"
command = ["$ps", "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", "$scriptPath", "-IntervalSeconds", "1"]
restart = "no"
enable = false
[services.windows]
$accountLine
"@
    [IO.File]::WriteAllText($toml, $document, [Text.UTF8Encoding]::new($false))
    Invoke-Spm @('validate', $toml) | Out-Null
    $plan = (Invoke-Spm @('apply', '--dry-run', $toml) -Capture) -join "`n"
    if ($plan -notmatch "create`t$([regex]::Escape($declared))") { throw "Unexpected plan: $plan" }
    Invoke-Spm @('apply', $toml) | Out-Null
    Wait-Running $declared
    $diff = (Invoke-Spm @('diff', $toml) -Capture) -join "`n"
    if ($diff -notmatch "unchanged`t$([regex]::Escape($declared))") { throw "Unexpected diff: $diff" }

    Write-Host "==> $Scope native scheduling, portable cron, state, and lifecycle"
    Remove-Item -LiteralPath $scheduleMarker -Force -ErrorAction SilentlyContinue
    $powershell = (Get-Command powershell.exe).Source
    $scheduleArguments = @(
        'schedule', '--name', $scheduled, '--cron', '* * * * *', '--timezone', 'UTC',
        '--persistent', '--randomized-delay', '1s', '--cwd', (Split-Path $scheduleMarker)
    )
    # SYSTEM is the canonical non-interactive Task Scheduler principal on
    # hosted Windows runners. LocalService and NetworkService account mapping
    # remain covered below by the SCM account matrix.
    if ($system) { $scheduleArguments += @('--user', 'SYSTEM') }
    $scheduleArguments += @(
        '--',
        $powershell, '-NoProfile', '-NonInteractive', '-Command',
        "Add-Content -LiteralPath '$($scheduleMarker.Replace("'", "''"))' -Value ([DateTime]::UtcNow.ToString('O'))"
    )
    Invoke-Spm $scheduleArguments | Out-Null
    $taskName = "spm-$scheduled-schedule"
    $task = Get-ScheduledTask -TaskName $taskName -ErrorAction Stop
    if ($task.State -eq 'Disabled') { throw "$taskName was unexpectedly disabled" }
    $taskXml = Export-ScheduledTask -TaskName $taskName
    if ($taskXml -notmatch '__scheduled-run' -or $taskXml -notmatch '<Interval>PT1M</Interval>') {
        throw "Unexpected schedule task XML: $taskXml"
    }
    try {
        Wait-File $scheduleMarker 100
    } catch {
        Write-ScheduleDiagnostics $scheduled
        throw
    }
    $scheduleStatus = Wait-ScheduleSuccess $scheduled
    $beforeManual = @(Get-Content -LiteralPath $scheduleMarker).Count
    Invoke-Spm @('run', $scheduled) | Out-Null
    $afterManual = @(Get-Content -LiteralPath $scheduleMarker).Count
    if ($afterManual -le $beforeManual) { throw 'Manual scheduled run did not execute.' }
    Invoke-Spm @('disable', $scheduled) | Out-Null
    if ((Get-ScheduledTask -TaskName $taskName).State -ne 'Disabled') { throw 'Scheduled task was not disabled.' }
    Invoke-Spm @('enable', $scheduled) | Out-Null
    if ((Get-ScheduledTask -TaskName $taskName).State -eq 'Disabled') { throw 'Scheduled task was not re-enabled.' }

    Write-Host "==> $Scope declarative scheduled task"
    $escapedPowerShell = $powershell.Replace('\', '\\')
    $escapedMarker = $scheduleMarker.Replace('\', '\\').Replace('"', '\"')
    $scheduleAccountLine = if ($system) { 'user = "SYSTEM"' } else { '' }
    $scheduleDocument = @"
schema = 1
[[services]]
name = "$scheduledDeclared"
command = ["$escapedPowerShell", "-NoProfile", "-NonInteractive", "-Command", "Add-Content -LiteralPath '$escapedMarker' -Value declared"]
schedule = "*/5 9-17/2 1 * 1-5/2"
timezone = "Asia/Singapore"
persistent = true
timer_randomized_delay = "1s"
enable = false
[services.windows]
$scheduleAccountLine
"@
    [IO.File]::WriteAllText($scheduleToml, $scheduleDocument, [Text.UTF8Encoding]::new($false))
    Invoke-Spm @('validate', $scheduleToml) | Out-Null
    Invoke-Spm @('apply', $scheduleToml) | Out-Null
    $declaredTask = Get-ScheduledTask -TaskName "spm-$scheduledDeclared-schedule" -ErrorAction Stop
    if ($declaredTask.State -ne 'Disabled') { throw 'Declarative scheduled task should be disabled.' }
    $scheduleDiff = (Invoke-Spm @('diff', $scheduleToml) -Capture) -join "`n"
    if ($scheduleDiff -notmatch "unchanged`t$([regex]::Escape($scheduledDeclared))") { throw "Unexpected schedule diff: $scheduleDiff" }
    Remove-Item -LiteralPath $scheduleToml -Force -ErrorAction SilentlyContinue

    if (-not $system) {
        Write-Host '==> User crash/restart policy'
        $powershell = (Get-Command powershell.exe).Source
        Invoke-Spm @('start', '--name', $crashing, '--no-enable', '--restart', 'on-failure', '--restart-sec', '200ms', '--', $powershell, '-NoProfile', '-NonInteractive', '-ExecutionPolicy', 'Bypass', '-File', $workload, '-IntervalSeconds', '1', '-ExitAfter', '1', '-ExitCode', '23') | Out-Null
        for ($attempt = 0; $attempt -lt 60; $attempt++) {
            $row = Get-SpmRow $crashing
            if ($null -ne $row -and [int]$row.restarts -ge 2) { break }
            Start-Sleep -Milliseconds 250
        }
        $row = Get-SpmRow $crashing
        if ($null -eq $row -or [int]$row.restarts -lt 2) { throw 'Restart policy was not exercised.' }
    } else {
        Write-Host '==> Built-in SCM accounts'
        foreach ($case in @(@('SYSTEM', 'LocalSystem'), @('LocalService', 'LocalService'), @('NetworkService', 'NetworkService'))) {
            $accountName = $case[0]
            $expected = $case[1]
            $name = "$prefix-account-$($accountName.ToLowerInvariant())"
            $names += $name
            Start-Workload $name $accountName
            $native = Get-CimInstance Win32_Service -Filter "Name='spm-$name'"
            if ($native.StartName -notmatch $expected) { throw "$accountName mapped to $($native.StartName)" }
            Remove-ServiceSafe $name
        }
    }
    Write-Host "spm Windows $Scope integration passed" -ForegroundColor Green
} finally {
    foreach ($name in $names) { try { Remove-ServiceSafe $name } catch { Write-Warning $_ } }
    [Environment]::SetEnvironmentVariable('SPM_WINDOWS_DATA_DIR', $oldData, 'Process')
    Remove-Item -LiteralPath $toml -Force -ErrorAction SilentlyContinue
    Remove-Item -LiteralPath $scheduleToml -Force -ErrorAction SilentlyContinue
    Remove-Item -LiteralPath $scheduleMarker -Force -ErrorAction SilentlyContinue
    if ($system) { Remove-Item -LiteralPath $workloadDir -Recurse -Force -ErrorAction SilentlyContinue }
    Remove-Item -LiteralPath $data -Recurse -Force -ErrorAction SilentlyContinue
}

# GitHub's pwsh wrapper exits with the last native command's exit code.
# Best-effort cleanup can leave a nonzero code for an already-deleted service.
# Only successful completion reaches here; exceptions still fail the step.
$global:LASTEXITCODE = 0
