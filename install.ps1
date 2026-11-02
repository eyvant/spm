[CmdletBinding()]
param(
    [string]$Version = "latest",
    [string]$InstallDir = "$env:LOCALAPPDATA\Programs\spm",
    [switch]$NoCompletion,
    [switch]$NoPath
)

$ErrorActionPreference = "Stop"
$repository = "eyvant/spm"

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
