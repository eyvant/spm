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
    $fixtureExecutable = Join-Path $payloadDir "spm.exe"
    if ($Spm) {
        Copy-Item -LiteralPath ([IO.Path]::GetFullPath($Spm)) -Destination $fixtureExecutable
    } else {
        & go build -buildvcs=false -o $fixtureExecutable (Join-Path $repository "cmd\spm")
        if ($LASTEXITCODE -ne 0) { throw "Failed to build the installer test fixture" }
    }
    Compress-Archive -LiteralPath $fixtureExecutable -DestinationPath $archivePath
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
