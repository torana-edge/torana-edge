$ErrorActionPreference = "Stop"

$Version = if ($env:INSTALL_VERSION) { $env:INSTALL_VERSION } else { "latest" }
$InstallDir = if ($env:INSTALL_DIR) { $env:INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA "Torana\bin" }
$Repository = "torana-edge/torana-edge"
$Architecture = if ([System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture -eq "Arm64") { "arm64" } else { "amd64" }

if ($Version -eq "latest") {
    $release = Invoke-RestMethod "https://api.github.com/repos/$Repository/releases/latest"
    $Tag = $release.tag_name
    $ReleasePath = "latest/download"
} else {
    $Tag = $Version
    $ReleasePath = "download/$Version"
}

$ArchiveVersion = $Tag.TrimStart("v")
$Archive = "torana_${ArchiveVersion}_windows_${Architecture}.zip"
$Base = "https://github.com/$Repository/releases/$ReleasePath"
$Temporary = Join-Path ([System.IO.Path]::GetTempPath()) ("torana-" + [guid]::NewGuid())

try {
    New-Item -ItemType Directory -Path $Temporary | Out-Null
    Invoke-WebRequest "$Base/$Archive" -OutFile (Join-Path $Temporary $Archive)
    Invoke-WebRequest "$Base/checksums.txt" -OutFile (Join-Path $Temporary "checksums.txt")
    $line = Get-Content (Join-Path $Temporary "checksums.txt") | Where-Object { $_ -match "\s\*?$([regex]::Escape($Archive))$" } | Select-Object -First 1
    if (-not $line) { throw "Release checksum for $Archive was not published." }
    $expected = ($line -split "\s+")[0].ToLowerInvariant()
    $actual = (Get-FileHash (Join-Path $Temporary $Archive) -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($actual -ne $expected) { throw "Checksum verification failed for $Archive." }
    Expand-Archive -Path (Join-Path $Temporary $Archive) -DestinationPath $Temporary
    New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
    Copy-Item (Join-Path $Temporary "torana.exe") (Join-Path $InstallDir "torana.exe") -Force
    Write-Host "Torana installed to $(Join-Path $InstallDir 'torana.exe')"
    Write-Host ""
    Write-Host "Torana is a proxy - on its own it forwards traffic and nothing else."
    Write-Host "The interesting behaviour lives in plugins, which are NOT installed"
    Write-Host "with the gateway. Pick the ones you want:"
    Write-Host ""
    Write-Host "  torana plugin install --official"
    Write-Host "  torana plugin install github.com/you/your-plugins/plugins/foo"
    Write-Host ""
    Write-Host "Plugins are compiled from source on your machine, never downloaded"
    Write-Host "prebuilt, and none run until you approve their capabilities in the"
    Write-Host "control plane. A Go toolchain is required to build them."
    Write-Host ""
    Write-Host "  torana serve    # then open http://127.0.0.1:8080/_torana/"
} finally {
    Remove-Item -Recurse -Force $Temporary -ErrorAction SilentlyContinue
}
