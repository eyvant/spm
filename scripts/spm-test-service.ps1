<#
.SYNOPSIS
A small long-running Windows workload for manually testing spm.

.DESCRIPTION
Prints startup information and periodic heartbeat lines to stdout. Optionally
opens a loopback TCP port for readiness testing or exits after a number of
heartbeats so spm's restart policy can be observed.

.EXAMPLE
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\scripts\spm-test-service.ps1

.EXAMPLE
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\scripts\spm-test-service.ps1 -Port 18080

.EXAMPLE
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\scripts\spm-test-service.ps1 -ExitAfter 5 -ExitCode 23
#>
[CmdletBinding()]
param(
    [ValidateRange(1, 3600)]
    [int]$IntervalSeconds = 2,

    [ValidateRange(0, 65535)]
    [int]$Port = 0,

    [ValidateRange(0, 1000000)]
    [int]$ExitAfter = 0,

    [ValidateRange(0, 255)]
    [int]$ExitCode = 23
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$listener = $null
$startedAt = [DateTimeOffset]::Now
$heartbeat = 0
$testValue = [Environment]::GetEnvironmentVariable('SPM_TEST_VALUE')
if ([string]::IsNullOrEmpty($testValue)) {
    $testValue = '<not set>'
}

try {
    if ($Port -gt 0) {
        $listener = [Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback, $Port)
        $listener.Start()
    }

    Write-Output ('READY pid={0} started={1:o} cwd="{2}" tcp={3} SPM_TEST_VALUE="{4}"' -f `
        $PID, $startedAt, (Get-Location).Path, $Port, $testValue)

    while ($true) {
        $heartbeat++
        $uptime = [DateTimeOffset]::Now - $startedAt
        Write-Output ('HEARTBEAT sequence={0} pid={1} uptime={2:c} time={3:o}' -f `
            $heartbeat, $PID, $uptime, [DateTimeOffset]::Now)

        if ($null -ne $listener) {
            while ($listener.Pending()) {
                $client = $listener.AcceptTcpClient()
                try {
                    $payload = [Text.Encoding]::UTF8.GetBytes("spm test service pid=$PID heartbeat=$heartbeat`r`n")
                    $stream = $client.GetStream()
                    $stream.Write($payload, 0, $payload.Length)
                    $stream.Flush()
                } finally {
                    $client.Dispose()
                }
            }
        }

        if ($ExitAfter -gt 0 -and $heartbeat -ge $ExitAfter) {
            Write-Output "EXITING intentionally after $heartbeat heartbeats with code $ExitCode"
            exit $ExitCode
        }

        Start-Sleep -Seconds $IntervalSeconds
    }
} catch {
    Write-Error "TEST SERVICE ERROR: $($_.Exception.Message)"
    exit 1
} finally {
    if ($null -ne $listener) {
        $listener.Stop()
    }
    Write-Output "STOPPED pid=$PID time=$([DateTimeOffset]::Now.ToString('o'))"
}
