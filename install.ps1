[CmdletBinding()]
param(
    [string]$Version = "latest",
    [string]$InstallDir = "$env:LOCALAPPDATA\Programs\spm",
    [switch]$NoCompletion,
    [switch]$NoPath,
    [switch]$NoRestart,
    [switch]$RestartServices,
    [switch]$Uninstall,
    [switch]$Force
)

$ErrorActionPreference = "Stop"
$repository = "eyvant/spm"

if ($Force -and -not $Uninstall) {
    throw "-Force is only valid with -Uninstall"
}
if ($NoRestart -and $RestartServices) {
    throw "-NoRestart and -RestartServices cannot be combined"
}

# spm.exe is the CLI. spm-supervisor.exe runs user services and is copied into
# each system service; spm-scheduler.exe runs scheduled jobs. The runtimes
# carry their own version and are replaced only when it changes, so upgrading
# the CLI alone never restarts anything.
$runtimes = [ordered]@{
    "spm-supervisor.exe" = "--supervisor"
    "spm-scheduler.exe"  = "--scheduler"
}

# A running executable cannot be overwritten, but it can be renamed. Earlier
# upgrades leave previous binaries as *.exe.old*; delete the ones that no
# process uses any more.
function Remove-StaleExecutables([string]$Directory) {
    if (-not (Test-Path -LiteralPath $Directory)) { return }
    Get-ChildItem -Force -LiteralPath $Directory -Filter "spm*.exe.old*" | ForEach-Object {
        try { Remove-Item -Force -LiteralPath $_.FullName -ErrorAction Stop } catch { }
    }
}

# Install-Executable moves an existing file aside and copies the new one in.
function Install-Executable([string]$Source, [string]$Target) {
    $previous = $null
    if (Test-Path -LiteralPath $Target) {
        $previous = "$Target.old"
        if (Test-Path -LiteralPath $previous) {
            $previous = "$Target.old-" + [guid]::NewGuid().ToString("N")
        }
        Move-Item -LiteralPath $Target -Destination $previous
    }
    try {
        Copy-Item -LiteralPath $Source -Destination $Target
    } catch {
        if ($previous) { Move-Item -Force -LiteralPath $previous -Destination $Target }
        throw
    }
}

function Get-RuntimeVersion([string]$Executable) {
    if (-not (Test-Path -LiteralPath $Executable)) { return $null }
    try { return ((& $Executable version) | Select-Object -First 1).Trim() } catch { return $null }
}

function Test-SystemServices {
    if ($env:SPM_WINDOWS_DATA_DIR) { return $false }
    $directory = Join-Path $env:ProgramData "spm\services"
    try {
        return [bool]((Test-Path -LiteralPath $directory) -and
            (Get-ChildItem -Force -LiteralPath $directory -ErrorAction Stop | Select-Object -First 1))
    } catch {
        # Unreadable without elevation: system data exists, so let the
        # elevated step decide.
        return $true
    }
}

# Move the processes of the changed runtimes onto the new binaries.
function Update-SpmRuntime([string]$Executable, [string[]]$Components) {
    Write-Host "Moving running spm processes onto the new runtime ($($Components -join ' '))..."
    & $Executable __upgrade-runtime @Components
    if ($LASTEXITCODE -ne 0) {
        Write-Warning "Some user services were not moved to the new supervisor; the messages above say why and what to do. They keep running."
    }

    if (-not (Test-SystemServices)) { return }
    $principal = [Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()
    if ($principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
        & $Executable --system __upgrade-runtime @Components
        if ($LASTEXITCODE -ne 0) {
            Write-Warning "Some system services were not moved to the new supervisor; the messages above say why and what to do. They keep running."
        }
        return
    }
    Write-Host "System services need Administrator rights to upgrade; approve the Windows prompt to continue."
    $report = Join-Path ([IO.Path]::GetTempPath()) ("spm-upgrade-" + [guid]::NewGuid() + ".log")
    New-Item -ItemType File -Path $report | Out-Null
    try {
        try {
            $process = Start-Process -FilePath $Executable -ArgumentList (@("--system", "__upgrade-runtime") + $Components + @("--report", "`"$report`"")) `
                -Verb RunAs -WindowStyle Hidden -Wait -PassThru
        } catch {
            Write-Warning "System services still run the previous spm version because elevation was declined. Run this installer again and approve the prompt."
            return
        }
        Get-Content -LiteralPath $report | ForEach-Object { Write-Host $_ }
        if ($process.ExitCode -ne 0) {
            Write-Warning "Some system services were not moved to the new supervisor; the messages above say why and what to do. They keep running."
        }
    } finally {
        Remove-Item -Force -LiteralPath $report -ErrorAction SilentlyContinue
    }
}

if ($Uninstall) {
    $dataRoots = if ($env:SPM_WINDOWS_DATA_DIR) {
        @($env:SPM_WINDOWS_DATA_DIR)
    } else {
        @(
            (Join-Path $env:APPDATA "spm")
            (Join-Path $env:ProgramData "spm")
        )
    }
    $hasManagedServices = $false
    $serviceInspectionFailed = $false
    foreach ($dataRoot in $dataRoots) {
        $servicesDirectory = Join-Path $dataRoot "services"
        try {
            if ((Test-Path -LiteralPath $servicesDirectory) -and
                (Get-ChildItem -Force -LiteralPath $servicesDirectory | Select-Object -First 1)) {
                $hasManagedServices = $true
                break
            }
        } catch {
            $serviceInspectionFailed = $true
            Write-Warning "Could not inspect $servicesDirectory; run the uninstaller as Administrator to check system services."
        }
    }
    if ($serviceInspectionFailed -and -not $Force) {
        throw "Could not verify that all managed services are removed. Run as Administrator, or pass -Force to remove the CLI without deleting services."
    }
    if ($hasManagedServices -and -not $Force) {
        throw "Managed services still exist. Run 'spm delete all --yes' and 'spm --system delete all --yes' from an Administrator terminal as applicable, or pass -Force to remove the CLI without deleting services."
    }

    $completion = Join-Path $InstallDir "spm-completion.ps1"
    $profilePath = $PROFILE.CurrentUserAllHosts
    $profileLine = ". '$($completion.Replace("'", "''"))'"
    if (Test-Path -LiteralPath $profilePath) {
        $profileLines = @(Get-Content -LiteralPath $profilePath)
        $remainingLines = @($profileLines | Where-Object { $_ -ne $profileLine })
        if ($remainingLines.Count -ne $profileLines.Count) {
            Set-Content -Encoding UTF8 -LiteralPath $profilePath -Value $remainingLines
            Write-Host "Removed spm completion from $profilePath"
        }
    }

    foreach ($path in @((Join-Path $InstallDir "spm.exe"), $completion) + @($runtimes.Keys | ForEach-Object { Join-Path $InstallDir $_ })) {
        if (Test-Path -LiteralPath $path) {
            Remove-Item -Force -LiteralPath $path
            Write-Host "Removed $path"
        }
    }
    Remove-StaleExecutables $InstallDir

    $userPath = [Environment]::GetEnvironmentVariable("Path", "User")
    $normalizedInstallDir = $InstallDir.TrimEnd('\', '/')
    $pathParts = @($userPath -split ';' | Where-Object {
        $_ -and $_.TrimEnd('\', '/') -ine $normalizedInstallDir
    })
    $updatedPath = $pathParts -join ';'
    if ($updatedPath -ne $userPath) {
        [Environment]::SetEnvironmentVariable("Path", $updatedPath, "User")
        Write-Host "Removed $InstallDir from the user PATH. Open a new terminal to refresh it."
    }

    if ((Test-Path -LiteralPath $InstallDir) -and -not (Get-ChildItem -Force -LiteralPath $InstallDir | Select-Object -First 1)) {
        Remove-Item -Force -LiteralPath $InstallDir
    }
    Write-Host "Uninstalled spm. Managed service data was not removed."
    return
}

if (-not [Environment]::Is64BitOperatingSystem) {
    throw "spm requires 64-bit Windows"
}
$architecture = if ($env:PROCESSOR_ARCHITECTURE -eq "ARM64") { "arm64" } else { "amd64" }

if ($Version -eq "latest") {
    $release = Invoke-RestMethod -Headers @{ "User-Agent" = "spm-installer" } `
        -Uri "https://api.github.com/repos/$repository/releases/latest"
    $Version = $release.tag_name
} elseif (-not $Version.StartsWith("v")) {
    $Version = "v$Version"
}
if ($Version -notmatch '^v\d+\.\d+\.\d+([.-][0-9A-Za-z.-]+)?$') {
    throw "Invalid release version: $Version"
}

$archive = "spm-$Version-windows-$architecture.zip"
$baseUrl = "https://github.com/$repository/releases/download/$Version"
$temporaryDir = Join-Path ([IO.Path]::GetTempPath()) ("spm-install-" + [guid]::NewGuid())
New-Item -ItemType Directory -Path $temporaryDir | Out-Null
try {
    $archivePath = Join-Path $temporaryDir $archive
    $checksumsPath = Join-Path $temporaryDir "checksums.txt"
    Write-Host "Downloading spm $Version for windows/$architecture..."
    Invoke-WebRequest -UseBasicParsing -Uri "$baseUrl/$archive" -OutFile $archivePath
    Invoke-WebRequest -UseBasicParsing -Uri "$baseUrl/checksums.txt" -OutFile $checksumsPath

    $checksumLine = Get-Content -LiteralPath $checksumsPath | Where-Object { $_ -match "\s+$([regex]::Escape($archive))$" } | Select-Object -First 1
    if (-not $checksumLine) { throw "checksums.txt does not contain $archive" }
    $expected = ($checksumLine -split '\s+')[0].ToLowerInvariant()
    $actual = (Get-FileHash -Algorithm SHA256 -LiteralPath $archivePath).Hash.ToLowerInvariant()
    if ($actual -ne $expected) { throw "Checksum verification failed for $archive" }

    $expanded = Join-Path $temporaryDir "expanded"
    Expand-Archive -LiteralPath $archivePath -DestinationPath $expanded
    foreach ($file in @("spm.exe") + @($runtimes.Keys)) {
        if (-not (Test-Path -LiteralPath (Join-Path $expanded $file))) { throw "Release archive does not contain $file" }
    }
    New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
    Remove-StaleExecutables $InstallDir
    $target = Join-Path $InstallDir "spm.exe"
    $upgrading = Test-Path -LiteralPath $target
    Install-Executable (Join-Path $expanded "spm.exe") $target
    $changed = @()
    foreach ($runtime in $runtimes.Keys) {
        $source = Join-Path $expanded $runtime
        $installed = Join-Path $InstallDir $runtime
        $newVersion = Get-RuntimeVersion $source
        if ($newVersion -and $newVersion -eq (Get-RuntimeVersion $installed)) {
            Write-Host "$newVersion is already installed"
            continue
        }
        Install-Executable $source $installed
        $changed += $runtimes[$runtime]
        Write-Host "Installed $newVersion"
    }

    if (-not $NoCompletion) {
        $completion = Join-Path $InstallDir "spm-completion.ps1"
        & (Join-Path $InstallDir "spm.exe") completion powershell | Set-Content -Encoding UTF8 -LiteralPath $completion
        $profileDirectory = Split-Path -Parent $PROFILE.CurrentUserAllHosts
        New-Item -ItemType Directory -Force -Path $profileDirectory | Out-Null
        $line = ". '$($completion.Replace("'", "''"))'"
        $profileText = if (Test-Path -LiteralPath $PROFILE.CurrentUserAllHosts) { Get-Content -Raw -LiteralPath $PROFILE.CurrentUserAllHosts } else { "" }
        if ($profileText -notlike "*$line*") { Add-Content -Encoding UTF8 -LiteralPath $PROFILE.CurrentUserAllHosts -Value $line }
        Write-Host "Installed PowerShell completion. It loads in new PowerShell sessions."
    }

    if (-not $NoPath) {
        $userPath = [Environment]::GetEnvironmentVariable("Path", "User")
        $parts = @($userPath -split ';' | Where-Object { $_ })
        if ($parts -notcontains $InstallDir) {
            [Environment]::SetEnvironmentVariable("Path", (($parts + $InstallDir) -join ';'), "User")
            Write-Host "Added $InstallDir to the user PATH. Open a new terminal to use it."
        }
    }
    Write-Host "Installed spm $Version to $InstallDir\spm.exe"
    & (Join-Path $InstallDir "spm.exe") version
    # A first install has nothing running. After an upgrade, only the
    # runtimes whose version changed need their processes moved; that includes
    # installs from before the split, whose services still run spm.exe.
    if ($upgrading -and $changed.Count -gt 0) {
        if ($NoRestart) {
            Write-Host "Skipped moving spm processes onto the new runtime (-NoRestart); they keep the previous version until restarted."
        } else {
            # Services are handed to a new supervisor without stopping them.
            # One that cannot be handed over is left running on its current
            # supervisor and reported, unless -RestartServices allows a restart.
            if ($RestartServices -and $changed -contains "--supervisor") { $changed += "--allow-restart" }
            Update-SpmRuntime $target $changed
        }
    }
    Remove-StaleExecutables $InstallDir
} finally {
    if (Test-Path -LiteralPath $temporaryDir) {
        Remove-Item -Recurse -Force -LiteralPath $temporaryDir
    }
}
