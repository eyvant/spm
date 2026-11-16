[CmdletBinding()]
param(
    [string]$Spm
)

$ErrorActionPreference = "Stop"
$repository = Split-Path -Parent $PSScriptRoot
$installer = Join-Path $repository "install.ps1"
$temporaryParent = [IO.Path]::GetFullPath([IO.Path]::GetTempPath()).TrimEnd('\', '/')
$temporaryRoot = Join-Path $temporaryParent ("spm-installer-test-" + [guid]::NewGuid())
$installDir = Join-Path $temporaryRoot "install"
$noExtrasInstallDir = Join-Path $temporaryRoot "no-extras-install"
$badChecksumInstallDir = Join-Path $temporaryRoot "bad-checksum-install"
$dataRoot = Join-Path $temporaryRoot "data"
$fixtureDir = Join-Path $temporaryRoot "release"
$payloadDir = Join-Path $temporaryRoot "payload"
$releaseVersion = "v9.8.7"
$architecture = if ($env:PROCESSOR_ARCHITECTURE -eq "ARM64") { "arm64" } else { "amd64" }
$archive = "spm-$releaseVersion-windows-$architecture.zip"
$archivePath = Join-Path $fixtureDir $archive
$checksumsPath = Join-Path $fixtureDir "checksums.txt"
$profilePath = $PROFILE.CurrentUserAllHosts
$profileExisted = Test-Path -LiteralPath $profilePath
$profileBytes = if ($profileExisted) { [IO.File]::ReadAllBytes($profilePath) } else { $null }
$originalUserPath = [Environment]::GetEnvironmentVariable("Path", "User")
$originalDataRoot = $env:SPM_WINDOWS_DATA_DIR

function Assert-True([bool]$Condition, [string]$Message) {
    if (-not $Condition) { throw $Message }
}

function Invoke-RestMethod {
    [CmdletBinding()]
    param(
        [hashtable]$Headers,
        [string]$Uri
    )
    return [pscustomobject]@{ tag_name = $releaseVersion }
}

function Invoke-WebRequest {
    [CmdletBinding()]
    param(
        [switch]$UseBasicParsing,
        [string]$Uri,
        [string]$OutFile
    )
    $fileName = ([Uri]$Uri).Segments[-1]
    Copy-Item -LiteralPath (Join-Path $fixtureDir $fileName) -Destination $OutFile
}

try {
    New-Item -ItemType Directory -Path $fixtureDir, $payloadDir | Out-Null
    $fixtureFiles = @("spm.exe", "spm-supervisor.exe", "spm-scheduler.exe")
    if ($Spm) {
        # The runtimes are built next to spm.exe.
        $spmDirectory = Split-Path -Parent ([IO.Path]::GetFullPath($Spm))
        Copy-Item -LiteralPath ([IO.Path]::GetFullPath($Spm)) -Destination (Join-Path $payloadDir "spm.exe")
        foreach ($file in $fixtureFiles[1..2]) { Copy-Item -LiteralPath (Join-Path $spmDirectory $file) -Destination $payloadDir }
    } else {
        Push-Location $repository
        try {
            & go build -buildvcs=false -o "$payloadDir\" ./cmd/spm ./cmd/spm-supervisor ./cmd/spm-scheduler
            if ($LASTEXITCODE -ne 0) { throw "Failed to build the installer test fixture" }
        } finally {
            Pop-Location
        }
    }
    Compress-Archive -LiteralPath @($fixtureFiles | ForEach-Object { Join-Path $payloadDir $_ }) -DestinationPath $archivePath
    $checksum = (Get-FileHash -Algorithm SHA256 -LiteralPath $archivePath).Hash.ToLowerInvariant()
    Set-Content -Encoding ASCII -LiteralPath $checksumsPath -Value "$checksum  $archive"
    $env:SPM_WINDOWS_DATA_DIR = $dataRoot

    & $installer -InstallDir $installDir

    $installedExecutable = Join-Path $installDir "spm.exe"
    $installedCompletion = Join-Path $installDir "spm-completion.ps1"
    Assert-True (Test-Path -LiteralPath $installedExecutable) "Installer did not install spm.exe"
    Assert-True (Test-Path -LiteralPath $installedCompletion) "Installer did not install PowerShell completion"
    $versionOutput = & $installedExecutable version
    Assert-True ($LASTEXITCODE -eq 0 -and $versionOutput -like "spm *") "Installed executable did not run"

    $profileLine = ". '$($installedCompletion.Replace("'", "''"))'"
    Assert-True (@(Get-Content -LiteralPath $profilePath) -contains $profileLine) "Installer did not add the PowerShell profile entry"
    $installedUserPath = [Environment]::GetEnvironmentVariable("Path", "User")
    Assert-True (@($installedUserPath -split ';') -contains $installDir) "Installer did not add the user PATH entry"

    $installedSupervisor = Join-Path $installDir "spm-supervisor.exe"
    $installedScheduler = Join-Path $installDir "spm-scheduler.exe"
    Assert-True ((Test-Path -LiteralPath $installedSupervisor) -and (Test-Path -LiteralPath $installedScheduler)) "Installer did not install the runtimes"

    # Upgrade while services and the scheduler run from the installed runtimes.
    & $installedExecutable start --name upgrade-probe -- powershell.exe -NoProfile -Command "Start-Sleep 600"
    Assert-True ($LASTEXITCODE -eq 0) "Could not start the upgrade probe service"
    & $installedExecutable schedule --name upgrade-cron --cron "0 4 * * *" -- cmd.exe /c exit 0
    Assert-True ($LASTEXITCODE -eq 0) "Could not schedule the upgrade probe job"
    $probeState = Join-Path $dataRoot "services\upgrade-probe\state.json"
    $daemonPidFile = Join-Path $dataRoot "scheduler-state\run.lock\pid"
    $deadline = (Get-Date).AddSeconds(15)
    while (-not (Test-Path -LiteralPath $daemonPidFile) -and (Get-Date) -lt $deadline) { Start-Sleep -Milliseconds 200 }
    $oldSupervisor = (Get-Content -Raw -LiteralPath $probeState | ConvertFrom-Json).supervisor_pid
    $servicePid = (Get-Content -Raw -LiteralPath $probeState | ConvertFrom-Json).pid
    $oldDaemon = [int](Get-Content -LiteralPath $daemonPidFile)
    Assert-True ($null -ne (Get-Process -Id $oldSupervisor -ErrorAction SilentlyContinue)) "Upgrade probe supervisor is not running"
    Assert-True ($null -ne (Get-Process -Id $oldDaemon -ErrorAction SilentlyContinue)) "Scheduler daemon is not running"
    try {
        # Same runtime files: only the CLI is replaced, nothing restarts.
        & $installer -InstallDir $installDir -NoCompletion -NoPath
        Assert-True ((Get-Content -Raw -LiteralPath $probeState | ConvertFrom-Json).supervisor_pid -eq $oldSupervisor) "A CLI-only upgrade restarted the service"
        Assert-True ([int](Get-Content -LiteralPath $daemonPidFile) -eq $oldDaemon) "A CLI-only upgrade restarted the scheduler"

        # Missing runtimes count as changed: the scheduler daemon is replaced
        # and the service is handed to a new supervisor without stopping it.
        # The running binaries can be renamed but not deleted.
        Move-Item -LiteralPath $installedSupervisor -Destination "$installedSupervisor.old-test"
        Move-Item -LiteralPath $installedScheduler -Destination "$installedScheduler.old-test"
        & $installer -InstallDir $installDir -NoCompletion -NoPath

        $newSupervisor = (Get-Content -Raw -LiteralPath $probeState | ConvertFrom-Json).supervisor_pid
        # The new daemon replaces the stale lock left by the terminated one.
        $newDaemon = $oldDaemon
        $deadline = (Get-Date).AddSeconds(15)
        while ($newDaemon -eq $oldDaemon -and (Get-Date) -lt $deadline) {
            Start-Sleep -Milliseconds 200
            try { $newDaemon = [int](Get-Content -LiteralPath $daemonPidFile -ErrorAction Stop) } catch { }
        }
        Assert-True ($null -eq (Get-Process -Id $oldSupervisor -ErrorAction SilentlyContinue)) "Upgrade left the old supervisor running"
        Assert-True ($null -eq (Get-Process -Id $oldDaemon -ErrorAction SilentlyContinue)) "Upgrade left the old scheduler daemon running"
        Assert-True ($newSupervisor -ne $oldSupervisor -and $null -ne (Get-Process -Id $newSupervisor -ErrorAction SilentlyContinue)) "Upgrade did not replace the service supervisor"
        $probe = Get-Content -Raw -LiteralPath $probeState | ConvertFrom-Json
        Assert-True ($probe.pid -eq $servicePid -and $null -ne (Get-Process -Id $servicePid -ErrorAction SilentlyContinue)) "The supervisor upgrade restarted the service (pid $servicePid -> $($probe.pid))"
        Assert-True ($newDaemon -ne $oldDaemon -and $null -ne (Get-Process -Id $newDaemon -ErrorAction SilentlyContinue)) "Upgrade did not restart the scheduler daemon"
        $image = (Get-Process -Id $newSupervisor).Path
        Assert-True ($image -ieq $installedSupervisor) "The service supervisor runs $image instead of the installed spm-supervisor"
        $image = (Get-Process -Id $newDaemon).Path
        Assert-True ($image -ieq $installedScheduler) "The scheduler daemon runs $image instead of the installed spm-scheduler"
        Assert-True (-not (Get-ChildItem -LiteralPath $installDir -Filter "spm*.exe.old*")) "Upgrade left previous binaries behind"
    } finally {
        & $installedExecutable delete all --yes | Out-Null
    }

    & $installer -Version "9.8.7" -InstallDir $noExtrasInstallDir -NoCompletion -NoPath
    Assert-True (Test-Path -LiteralPath (Join-Path $noExtrasInstallDir "spm.exe")) "Installer did not normalize an explicit version"
    Assert-True (-not (Test-Path -LiteralPath (Join-Path $noExtrasInstallDir "spm-completion.ps1"))) "-NoCompletion installed completion"
    Assert-True (-not (@([Environment]::GetEnvironmentVariable("Path", "User") -split ';') -contains $noExtrasInstallDir)) "-NoPath added a PATH entry"

    Set-Content -Encoding ASCII -LiteralPath $checksumsPath -Value "$('0' * 64)  $archive"
    $checksumFailed = $false
    try {
        & $installer -Version $releaseVersion -InstallDir $badChecksumInstallDir -NoCompletion -NoPath
    } catch {
        if ($_.Exception.Message -like "Checksum verification failed*") {
            $checksumFailed = $true
        } else {
            throw
        }
    }
    Assert-True $checksumFailed "Installation succeeded with an invalid checksum"
    Assert-True (-not (Test-Path -LiteralPath (Join-Path $badChecksumInstallDir "spm.exe"))) "Checksum failure installed spm.exe"

    & $installer -Uninstall -InstallDir $installDir

    Assert-True (-not (Test-Path -LiteralPath $installedExecutable)) "Uninstall left spm.exe behind"
    Assert-True (-not (Test-Path -LiteralPath $installedCompletion)) "Uninstall left PowerShell completion behind"
    Assert-True (-not (@(Get-Content -LiteralPath $profilePath) -contains $profileLine)) "Uninstall left the PowerShell profile entry behind"
    $remainingPath = [Environment]::GetEnvironmentVariable("Path", "User")
    Assert-True (-not (@($remainingPath -split ';') -contains $installDir)) "Uninstall left the user PATH entry behind"

    New-Item -ItemType Directory -Path $installDir -Force | Out-Null
    New-Item -ItemType File -Path (Join-Path $installDir "spm.exe") | Out-Null
    New-Item -ItemType Directory -Path (Join-Path $dataRoot "services\api") -Force | Out-Null
    $blocked = $false
    try {
        & $installer -Uninstall -InstallDir $installDir
    } catch {
        if ($_.Exception.Message -like "Managed services still exist*") {
            $blocked = $true
        } else {
            throw
        }
    }
    Assert-True $blocked "Uninstall was not blocked while a managed service existed"
    Assert-True (Test-Path -LiteralPath (Join-Path $installDir "spm.exe")) "Blocked uninstall removed spm.exe"

    & $installer -Uninstall -Force -InstallDir $installDir
    Assert-True (-not (Test-Path -LiteralPath (Join-Path $installDir "spm.exe"))) "Forced uninstall left spm.exe behind"

    $forceWithoutUninstallFailed = $false
    try {
        & $installer -Force -InstallDir $installDir
    } catch {
        if ($_.Exception.Message -eq "-Force is only valid with -Uninstall") {
            $forceWithoutUninstallFailed = $true
        } else {
            throw
        }
    }
    Assert-True $forceWithoutUninstallFailed "-Force succeeded without -Uninstall"

    Write-Host "install.ps1 install and uninstall tests passed"
} finally {
    [Environment]::SetEnvironmentVariable("Path", $originalUserPath, "User")
    $env:SPM_WINDOWS_DATA_DIR = $originalDataRoot
    if ($profileExisted) {
        [IO.File]::WriteAllBytes($profilePath, $profileBytes)
    } elseif (Test-Path -LiteralPath $profilePath) {
        Remove-Item -Force -LiteralPath $profilePath
    }

    $resolvedTemporaryRoot = [IO.Path]::GetFullPath($temporaryRoot)
    if ((Split-Path -Parent $resolvedTemporaryRoot).TrimEnd('\', '/') -ne $temporaryParent -or
        (Split-Path -Leaf $resolvedTemporaryRoot) -notlike "spm-installer-test-*") {
        throw "Refusing to remove unexpected test path: $resolvedTemporaryRoot"
    }
    if (Test-Path -LiteralPath $resolvedTemporaryRoot) {
        Remove-Item -Recurse -Force -LiteralPath $resolvedTemporaryRoot
    }
}
