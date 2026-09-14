#Requires -Version 5.1
<#
.SYNOPSIS
Install an official Torana Edge release on Windows.
.DESCRIPTION
Defaults to the latest published release and $env:LOCALAPPDATA\Torana\bin.
TORANA_VERSION and TORANA_INSTALL_DIR provide defaults. No administrator access,
profile/PATH edits, service startup, or plugin installation. Requires curl.exe
(included with supported current Windows versions). Website activation waits
for the first published Edge tag; see docs/RELEASE_INSTALLERS.md.
#>
[CmdletBinding()]
param(
    [string]$Version = $env:TORANA_VERSION,
    [string]$InstallDir = $env:TORANA_INSTALL_DIR
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$tempDir = $null
$stageDir = $null

function Invoke-ToranaDownload {
    param([string[]]$CurlArguments)
    # Use curl.exe explicitly: Windows PowerShell aliases curl to
    # Invoke-WebRequest, which does not support these HTTPS redirect controls.
    $result = & curl.exe --proto '=https' --proto-redir '=https' --tlsv1.2 `
        --fail --silent --show-error --location --retry 2 `
        --connect-timeout 15 --max-time 300 @CurlArguments
    if ($LASTEXITCODE -ne 0) {
        throw "Download failed (curl exit $LASTEXITCODE). The release or asset may not be published yet; check GitHub releases. Before the first Edge release, build from source."
    }
    return $result
}

try {
    if ([Environment]::OSVersion.Platform -ne [PlatformID]::Win32NT) {
        throw 'This installer supports Windows; use install.sh on macOS or Linux.'
    }
    if (-not (Get-Command curl.exe -CommandType Application -ErrorAction SilentlyContinue)) {
        throw 'curl.exe is required (included with current Windows). Install curl or download the release manually.'
    }
    # PROCESSOR_ARCHITEW6432 identifies the native OS when a 32-bit PowerShell
    # process is running under WOW64. Otherwise use the process architecture.
    $architecture = $env:PROCESSOR_ARCHITEW6432
    if (-not $architecture) { $architecture = $env:PROCESSOR_ARCHITECTURE }
    switch ($architecture) {
        'AMD64' { $arch = 'amd64' }
        'ARM64' { $arch = 'arm64' }
        default { throw "Unsupported architecture '$architecture'; Windows amd64 and arm64 are supported." }
    }
    if (-not $InstallDir) {
        if (-not $env:LOCALAPPDATA) { throw 'LOCALAPPDATA is unset; use -InstallDir.' }
        $InstallDir = Join-Path $env:LOCALAPPDATA 'Torana\bin'
    }
    # Drive-relative paths such as C:bin depend on unrelated shell state.
    if ($InstallDir -notmatch '^(?:[A-Za-z]:[\\/]|\\\\[^\\]+\\[^\\]+)') {
        throw 'InstallDir must be an absolute Windows path.'
    }
    $InstallDir = [IO.Path]::GetFullPath($InstallDir)
    $releaseUrl = 'https://github.com/torana-edge/torana-edge/releases'
    if (-not $Version) {
        $resolved = Invoke-ToranaDownload -CurlArguments @('--output', 'NUL', '--write-out', '%{url_effective}', "$releaseUrl/latest")
        if (-not $resolved -or -not $resolved.StartsWith("$releaseUrl/tag/", [StringComparison]::Ordinal)) {
            throw 'No published release could be resolved to an official release tag.'
        }
        $Version = $resolved.Substring("$releaseUrl/tag/".Length)
    }
    $Version = $Version -creplace '^v', ''
    if ($Version -cnotmatch '^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z]+([.-][0-9A-Za-z]+)*)?(\+[0-9A-Za-z]+([.-][0-9A-Za-z]+)*)?$') {
        throw 'Version must be a release version such as v1.2.3 or v1.2.3-rc.1.'
    }

    $archive = "torana_${Version}_windows_${arch}.zip"
    $tempDir = Join-Path ([IO.Path]::GetTempPath()) ("torana-install-" + [Guid]::NewGuid().ToString('N'))
    $null = New-Item -ItemType Directory -Path $tempDir
    Write-Host "Downloading Torana $Version for windows/$arch..."
    foreach ($asset in @($archive, 'checksums.txt')) {
        $null = Invoke-ToranaDownload -CurlArguments @('--output', (Join-Path $tempDir $asset), "$releaseUrl/download/v$Version/$asset")
    }
    $checksumLines = @(Get-Content -LiteralPath (Join-Path $tempDir 'checksums.txt') | Where-Object {
        # Identify all entries for the exact filename before checking syntax,
        # so malformed or duplicate entries cannot be silently skipped.
        $_ -cmatch ('^\s*\S+\s+\*?' + [regex]::Escape($archive) + '(?:\s|$)')
    })
    if ($checksumLines.Count -ne 1 -or $checksumLines[0] -cnotmatch ('^\s*([0-9a-fA-F]{64})\s+\*?' + [regex]::Escape($archive) + '\s*$')) {
        throw "checksums.txt must contain exactly one valid SHA-256 entry for $archive."
    }
    $expected = $Matches[1]
    $actual = (Get-FileHash -Algorithm SHA256 -LiteralPath (Join-Path $tempDir $archive)).Hash
    if ($actual -ine $expected) { throw 'SHA-256 checksum mismatch; nothing was installed.' }

    # Extract only the exact root entry, streaming into a file we create. ZIP
    # paths cannot escape the staging directory or create archive-supplied links.
    Add-Type -AssemblyName System.IO.Compression.FileSystem
    try {
        $zip = [IO.Compression.ZipFile]::OpenRead((Join-Path $tempDir $archive))
    } catch { throw "Invalid release archive: $($_.Exception.Message)" }
    try {
        $entries = @($zip.Entries | Where-Object { $_.FullName -ceq 'torana.exe' })
        if ($entries.Count -ne 1 -or $entries[0].Length -eq 0) {
            throw 'Release archive must contain exactly one non-empty root torana.exe binary.'
        }
        $binary = Join-Path $tempDir 'torana.exe'
        [IO.Compression.ZipFileExtensions]::ExtractToFile($entries[0], $binary, $false)
    } finally { $zip.Dispose() }

    $destination = Join-Path $InstallDir 'torana.exe'
    if (Test-Path -LiteralPath $destination) {
        $existing = Get-Item -LiteralPath $destination -Force
        if ($existing.PSIsContainer -or ($existing.Attributes -band [IO.FileAttributes]::ReparsePoint)) {
            throw "Refusing to replace a directory or link: $destination"
        }
    }
    $null = [IO.Directory]::CreateDirectory($InstallDir)
    $stageDir = Join-Path $InstallDir ('.torana-install-' + [Guid]::NewGuid().ToString('N'))
    $null = New-Item -ItemType Directory -Path $stageDir
    $staged = Join-Path $stageDir 'torana.exe'
    Copy-Item -LiteralPath $binary -Destination $staged
    # Same-volume replacement preserves the previous executable if replacement
    # fails (e.g. an instance is running); never delete the old file first.
    if (Test-Path -LiteralPath $destination) {
        [IO.File]::Replace($staged, $destination, $null)
    } else {
        [IO.File]::Move($staged, $destination)
    }
    Write-Host "Installed Torana $Version to $destination"
    if (($env:PATH -split ';') -icontains $InstallDir) {
        Write-Host 'Run: torana version'
    } else {
        Write-Host "Add this directory to your user PATH in Environment Variables, then open a new terminal and run torana version:`n  $InstallDir"
    }
} catch {
    Write-Error "torana installer: $($_.Exception.Message)"
    exit 1
} finally {
    if ($stageDir) { Remove-Item -LiteralPath $stageDir -Recurse -Force -ErrorAction SilentlyContinue }
    if ($tempDir) { Remove-Item -LiteralPath $tempDir -Recurse -Force -ErrorAction SilentlyContinue }
}
