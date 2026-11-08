# ovnfs installer for Windows
# Usage: irm https://stencil.so/ovnfs.ps1 | iex
#        & ([scriptblock]::Create((irm https://stencil.so/ovnfs.ps1))) -Version v0.2.3
#
# Downloads the release binary from Stencil's build service
# ($env:OVNFS_BUILD_URL, default https://build.stencil.so), checks it against
# the service's SHA-256, installs it as $env:OVNFS_INSTALL_DIR\ovnfs.exe
# (default %LOCALAPPDATA%\Programs\ovnfs\ovnfs.exe) and adds that folder to
# the user PATH. `ovnfs update` later replaces it with the latest release.

param(
    [string]$Version
)

$ErrorActionPreference = "Stop"
# Windows PowerShell 5.1 downloads crawl while it draws a progress bar.
$ProgressPreference = "SilentlyContinue"

if ($PSVersionTable.PSVersion -lt [version]"5.1") {
    throw "Windows PowerShell 5.1 or newer is required (found $($PSVersionTable.PSVersion)). Install PowerShell 7 from https://aka.ms/powershell and re-run the installer."
}
# The build service requires TLS 1.2, which Windows PowerShell 5.1 does not always offer by default.
[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

$InstallDir = if ($env:OVNFS_INSTALL_DIR) { $env:OVNFS_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA "Programs\ovnfs" }
$BuildUrl = if ($env:OVNFS_BUILD_URL) { $env:OVNFS_BUILD_URL.TrimEnd("/") } else { "https://build.stencil.so" }

# PROCESSOR_ARCHITEW6432 holds the OS architecture when 32-bit PowerShell runs
# on 64-bit Windows.
$RawArchitecture = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
$Target = switch ($RawArchitecture) {
    "AMD64" { "windows-x86_64" }
    "ARM64" { "windows-arm64" }
    default { throw "Unsupported Windows architecture: $RawArchitecture" }
}

if ($Version) {
    if ($Version -notmatch '^v?(\d+\.\d+\.\d+)$') {
        throw "Not a release tag: $Version"
    }
    Write-Host "Fetching ovnfs v$($Matches[1])..."
    $ApiUrl = "$BuildUrl/api/products/ovnfs/versions/$($Matches[1])/$Target"
} else {
    Write-Host "Fetching the latest release..."
    $ApiUrl = "$BuildUrl/api/products/ovnfs/latest/${Target}?channel=stable"
}
try {
    $Answer = Invoke-RestMethod -Uri $ApiUrl -TimeoutSec 60
} catch {
    throw "Failed to find an ovnfs build for $Target at ${ApiUrl}: $_"
}
$Version = $Answer.build.version
$ExpectedSha256 = "$($Answer.file.sha256)".ToLowerInvariant()
$DownloadUrl = $Answer.download
if (-not $Version -or -not $DownloadUrl -or $ExpectedSha256 -notmatch '^[0-9a-f]{64}$') {
    throw "Unexpected answer from $ApiUrl"
}

$Exe = Join-Path $InstallDir "ovnfs.exe"
$Old = "$Exe.old"
# Downloaded beside the destination and renamed into place only once it checks
# out, so a bad download never replaces a working ovnfs.
$Staged = Join-Path $InstallDir ".ovnfs-download-$PID.exe"
$Upgrade = Test-Path -LiteralPath $Exe
New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
try {
    Write-Host "Downloading ovnfs v$Version ($Target)..."
    Invoke-WebRequest -UseBasicParsing -Uri $DownloadUrl -OutFile $Staged -TimeoutSec 900
    $ActualSha256 = (Get-FileHash -Algorithm SHA256 -LiteralPath $Staged).Hash.ToLowerInvariant()
    if ($ActualSha256 -ne $ExpectedSha256) {
        throw "Checksum mismatch for ovnfs v${Version}: expected $ExpectedSha256, got $ActualSha256"
    }
    & $Staged version | Out-Null
    if ($LASTEXITCODE -ne 0) {
        throw "ovnfs v$Version does not run here"
    }
    # Windows refuses to overwrite a running executable, such as the daemon's,
    # but lets it be renamed; the old one is removed once nothing runs it.
    Remove-Item -Force -LiteralPath $Old -ErrorAction SilentlyContinue
    if ($Upgrade) {
        Move-Item -Force -LiteralPath $Exe -Destination $Old
    }
    try {
        Move-Item -LiteralPath $Staged -Destination $Exe
    } catch {
        if ($Upgrade) {
            Move-Item -LiteralPath $Old -Destination $Exe
        }
        throw
    }
    Remove-Item -Force -LiteralPath $Old -ErrorAction SilentlyContinue
} finally {
    Remove-Item -Force -LiteralPath $Staged -ErrorAction SilentlyContinue
}

Write-Host ""
Write-Host "[OK] Installed ovnfs v$Version to $Exe" -ForegroundColor Green

$UserPath = [Environment]::GetEnvironmentVariable("Path", "User")
if (($UserPath -split ";") -notcontains $InstallDir) {
    $NewPath = if ($UserPath) { "$UserPath;$InstallDir" } else { $InstallDir }
    [Environment]::SetEnvironmentVariable("Path", $NewPath, "User")
    $env:Path = "$env:Path;$InstallDir"
    Write-Host "Added $InstallDir to your user PATH; terminals opened before this one need a restart to see it"
}
if ($Upgrade) {
    Write-Host "A running ovnfs daemon keeps the old version until 'ovnfs restart'"
}
if (-not (Get-Command git -ErrorAction SilentlyContinue)) {
    Write-Host "ovnfs runs git, which is not on your PATH" -ForegroundColor Yellow
}
