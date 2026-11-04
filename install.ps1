[CmdletBinding()]
param(
    [string]$Version = "latest",
    [string]$InstallDir = "$env:LOCALAPPDATA\Programs\spm",
    [switch]$NoCompletion,
    [switch]$NoPath,
    [switch]$Uninstall,
    [switch]$Force
)

$ErrorActionPreference = "Stop"
$repository = "eyvant/spm"

if ($Force -and -not $Uninstall) {
    throw "-Force is only valid with -Uninstall"
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

    foreach ($path in @((Join-Path $InstallDir "spm.exe"), $completion)) {
        if (Test-Path -LiteralPath $path) {
            Remove-Item -Force -LiteralPath $path
            Write-Host "Removed $path"
        }
    }

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
    $executable = Join-Path $expanded "spm.exe"
    if (-not (Test-Path -LiteralPath $executable)) { throw "Release archive does not contain spm.exe" }
    New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
    Copy-Item -Force -LiteralPath $executable -Destination (Join-Path $InstallDir "spm.exe")

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
} finally {
    if (Test-Path -LiteralPath $temporaryDir) {
        Remove-Item -Recurse -Force -LiteralPath $temporaryDir
    }
}
