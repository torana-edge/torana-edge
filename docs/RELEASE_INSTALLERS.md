# Release installers: preparation and activation

The installer implementation is ready for review in this repository. **Do not
serve it from torana.sh or advertise binary installation until the first Torana
Edge tag has a published GitHub release with verified assets.** This change
does not create a tag, publish a release, deploy the website, or change the
current source-build quickstart. An SDK tag is not an Edge release.

Both scripts install the existing `torana` binary built from `cmd/torana` in
this repository. They do not introduce a separate CLI distribution, start a
server, install plugins, write configuration, edit shell profiles, or use sudo.

## Installer contract

| Platform | Architectures | Script | Default directory |
| --- | --- | --- | --- |
| macOS | Intel amd64, Apple Silicon arm64 | `scripts/install.sh` | `$HOME/.local/bin` |
| Linux | amd64, arm64 | `scripts/install.sh` | `$HOME/.local/bin` |
| Windows | amd64, arm64 | `scripts/install.ps1` | `$env:LOCALAPPDATA\Torana\bin` |

macOS/Linux require a POSIX shell, curl, tar, standard filesystem utilities,
and either `sha256sum` or `shasum`. Windows requires PowerShell 5.1 or newer and
`curl.exe` (included in current Windows versions). The Windows installer names
`curl.exe` explicitly so PowerShell's `curl` alias cannot change its behavior.
Architecture detection also handles a 32-bit Windows process under WOW64;
unsupported 32-bit operating systems fail with an explanation.

The default version resolves GitHub's latest published release. A pinned
version accepts either `v1.2.3` or `1.2.3`, including a prerelease such as
`v1.2.3-rc.1`. The tag must use a leading `v`; the archive version does not:

```text
https://github.com/torana-edge/torana-edge/releases/download/v1.2.3/
  torana_1.2.3_darwin_amd64.tar.gz
  torana_1.2.3_darwin_arm64.tar.gz
  torana_1.2.3_linux_amd64.tar.gz
  torana_1.2.3_linux_arm64.tar.gz
  torana_1.2.3_windows_amd64.zip
  torana_1.2.3_windows_arm64.zip
  checksums.txt
```

These names match `.goreleaser.yaml`. Versions here are examples, not claims
that those releases exist. Missing releases/assets, invalid versions, missing
or duplicate checksum entries, mismatched hashes, and missing/duplicate/empty
binary entries fail before the installed executable changes. Downloads and
redirects require HTTPS. SHA-256 checks validate bytes against the checksum
file from the same official release; they are not independent publisher
signatures.

The scripts stream only the exact root binary entry from an archive into a
file they create. Other archive paths cannot write files. Verified bytes are
staged in the destination directory and replace an existing regular binary;
temporary files are cleaned on success or failure. Windows refuses replacement
if the running executable is locked, preserving the previous binary. Stop that
Torana instance and retry. The scripts print PATH guidance without changing
the current process, user PATH, or shell profiles.

## Run from a reviewed checkout after a release exists

```sh
# Latest stable release; no administrator access needed.
sh scripts/install.sh

# Explicit version and location; command-line arguments override environment.
sh scripts/install.sh --version v1.2.3 --install-dir "$HOME/.local/bin"

# The same defaults are available as environment variables.
TORANA_VERSION=v1.2.3 TORANA_INSTALL_DIR="$HOME/.local/bin" sh scripts/install.sh

# Add the default location to this shell only, if needed.
export PATH="$HOME/.local/bin:$PATH"
torana version
```

```powershell
& .\scripts\install.ps1
& .\scripts\install.ps1 -Version v1.2.3 -InstallDir "$env:LOCALAPPDATA\Torana\bin"

# Add the default location to this terminal only, if needed.
$env:PATH = "$env:LOCALAPPDATA\Torana\bin;$env:PATH"
torana version
```

`TORANA_VERSION` and `TORANA_INSTALL_DIR` also work in PowerShell. For a
persistent PATH change, explicitly update your shell configuration on Unix or
your user Environment Variables on Windows, then open a new terminal.

## Checks before website activation

1. Merge the reviewed installer and CLI changes and require green release
   dry-run and installer checks. `.github/workflows/installers.yml` exercises
   the actual shell on Linux/macOS and both PowerShell 5.1 and PowerShell 7 on
   Windows, using synthetic downloads and a built native `torana` executable.
   Architecture-selection tests cover every advertised asset name. These
   checks do not contact GitHub releases or prove a public asset exists.
2. In a separately authorized release operation, tag the reviewed Edge commit
   with the chosen `vMAJOR.MINOR.PATCH` version and publish the artifacts from
   `.goreleaser.yaml`. The existing CI dry run uses GoReleaser `v2.15.4` with
   `--snapshot --skip=sbom,publish,announce`; that dry run is **not a release**.
   A real release must use the tag version, retain the configured SBOMs (with
   Syft available), and upload all six archives plus `checksums.txt`. This PR
   adds no automatic publishing workflow.
3. Download the actual published assets and checksum file. Verify all six
   hashes and root binary names. Confirm `torana version` reports the chosen
   tag version, using each native OS/architecture available. Record remaining
   hardware coverage honestly; selecting an arm64 archive in a mocked test
   does not execute an arm64 binary.
4. Run the reviewed scripts against the actual release, first with its pinned
   version and then with the default latest lookup, using a fresh temporary
   install directory on macOS, Linux, and Windows. Confirm the GitHub release
   is published and eligible for `/releases/latest` before advertising the
   default command. A prerelease alone does not satisfy this latest-release
   check. Check missing-release and checksum failures preserve a previous
   install. Do not continue website activation if any of these checks fails.
5. **Only after steps 1–4 pass**, make a separate website PR copying the
   reviewed, tagged `scripts/install.sh` and `scripts/install.ps1` bytes to the
   static routes `/install.sh` and `/install.ps1` on torana.sh. Record the Edge
   tag/commit in that PR. Serve script content directly over HTTPS (not the
   site's HTML fallback), and add the commands below to the site's install
   UI. Website deployment is a separate action from this preparation PR.
6. After that website deployment, verify both route bodies exactly match the
   tagged scripts and run the public commands in clean test accounts. Only
   then update the source-build quickstarts to advertise binary installation.

## Future website commands — not live until activation

macOS/Linux (download the complete script, then execute it):

```sh
curl --proto '=https' --proto-redir '=https' --tlsv1.2 -fsSL https://torana.sh/install.sh -o install-torana.sh && sh install-torana.sh
```

Windows PowerShell (check the download result before execution):

```powershell
curl.exe --proto '=https' --proto-redir '=https' --tlsv1.2 -fsSL https://torana.sh/install.ps1 -o install-torana.ps1; if ($LASTEXITCODE -eq 0) { & .\install-torana.ps1 }
```

These save a reviewable script in the current directory. Pick another output
filename if that file already exists. Add `--version v1.2.3` to the `sh`
invocation or `-Version v1.2.3` to the PowerShell invocation to pin a release.
Local PowerShell execution policy may require an explicit user decision;
the public command does not weaken machine policy.

## Local verification

```sh
sh -n scripts/install.sh
shellcheck scripts/install.sh
go test ./scripts/installertest -count=1 -v
TORANA_INSTALLER_SMOKE=1 go test ./scripts/installertest -run NativeBinary -count=1 -v
```

On Windows, run `go test ./scripts/installertest -count=1 -v` with
`TORANA_TEST_POWERSHELL=powershell` and again with
`TORANA_TEST_POWERSHELL=pwsh`. Set `TORANA_INSTALLER_SMOKE=1` to include building
and executing the native binary. Every installation targets a fresh test
directory; neither local checks nor CI modify the user's actual installation.
