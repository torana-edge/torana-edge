# Release and installer reference

This is the maintainer procedure for building, verifying and distributing an
Edge release. Users should follow the [quickstart](QUICKSTART.md), which names
the available install channel. An SDK tag is not an Edge binary release.

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
version accepts either `v1.2.3` or `1.2.3`, including prerelease and build
metadata such as `v1.2.3-rc.1+build.1`. The tag must use a leading `v`; the
archive version does not. The full prerelease/build suffix is preserved:

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
signatures. **The installers check checksums, not signer identity.** They do
not install/download a verifier or require GitHub CLI. The operator provenance
gate below is required before website activation; it is not automatic
per-install signature verification.

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

1. Require reviewed source and green release
   dry-run and installer checks. `.github/workflows/installers.yml` exercises
   the actual shell on Linux/macOS and both PowerShell 5.1 and PowerShell 7 on
   Windows, using synthetic downloads and a built native `torana` executable.
   Fixtures read `.goreleaser.yaml`; contract regressions reject incompatible
   asset/checksum/root-binary/version/platform changes. The dry run checks the
   actual six archives, hashes, root members, binary targets, and linker
   versions. A disposable fixture repository also exercises real GoReleaser
   tag parsing with prerelease/build metadata and executes its native binary.
   These checks do not prove a public release or valid attestation exists.
2. In a separately authorized release operation, tag the reviewed Edge commit
   with the chosen `vMAJOR.MINOR.PATCH` version. The tag-only
   `.github/workflows/release.yml` will then build, attest, verify, and publish
   a new release. A branch merge does not run it; there is no manual
   dispatch. It requires the tag's exact commit to be on the default branch
   and refuses any preexisting release, including drafts. The existing CI
   snapshot is **not a release** and skips publishing/announcements/SBOMs.
3. Run the explicit provenance verification below on all six published
   archives and their SBOMs. Verify root binary names and confirm
   `torana version` reports the chosen
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
   UI. Website deployment is a separate operation from an Edge release.
6. After that website deployment, verify both route bodies exactly match the
   tagged scripts and run the public commands in clean test accounts. Only
   then update the source-build quickstarts to advertise binary installation.

## Release workflow and provenance gate

The workflow uses pinned GoReleaser `v2.15.4`, Syft `v1.51.1`, GitHub CLI
`v2.100.0` (with a checked-in download digest), and commit-pinned actions.
It builds without publishing, validates exactly six archive names plus their
configured SBOMs and strict SHA-256 manifest, then uses
[`actions/attest`](https://github.com/actions/attest/tree/v4.2.2) to generate
signed SLSA build provenance. This is the current successor to
`actions/attest-build-provenance`. It uploads the attestation to GitHub and
includes `provenance.sigstore.json` in the release.

Before publication, every archive and SBOM is verified against that bundle,
the exact repository, signer workflow, tag ref, and source commit; self-hosted
runner attestations are rejected. Only then does it recheck the source and
create a new release containing those same verified files, the checksum
manifest, and bundle. Signing uses GitHub's OIDC identity, not a stored private
key. Job permissions are limited to contents, attestations, and OIDC writes;
no website, package registry, or repository settings are changed.

Default-branch ancestry is not review approval. The workflow does not configure a
protected-environment approval gate, branch protection, or tag protection.
Maintainers remain responsible for those policies and deliberate release-tag
sign-off; do not assume repository settings enforce them.

After the separately authorized tag release, an operator must repeat
[GitHub CLI attestation verification](https://cli.github.com/manual/gh_attestation_verify)
on the downloaded assets before website activation. Use a trusted current
GitHub CLI installation, the approved tag and its independently recorded full
commit SHA; do not derive the expected identity from the downloaded manifest:

```bash
set -euo pipefail
tag=v1.2.3 # Replace with the separately approved Edge tag.
commit=REPLACE_WITH_APPROVED_40_CHARACTER_COMMIT_SHA
[[ $commit =~ ^[0-9a-f]{40}$ ]]
version=${tag#v}
verify_dir=$(mktemp -d)
gh release download "$tag" --repo torana-edge/torana-edge \
  --pattern provenance.sigstore.json --pattern checksums.txt --dir "$verify_dir"
for os in darwin linux windows; do
  format=tar.gz
  if [[ $os == windows ]]; then format=zip; fi
  for arch in amd64 arm64; do
    archive="torana_${version}_${os}_${arch}.${format}"
    for asset in "$archive" "$archive.sbom.json"; do
      gh release download "$tag" --repo torana-edge/torana-edge \
        --pattern "$asset" --dir "$verify_dir"
      gh attestation verify "$verify_dir/$asset" \
        --bundle "$verify_dir/provenance.sigstore.json" \
        --repo torana-edge/torana-edge \
        --signer-workflow torana-edge/torana-edge/.github/workflows/release.yml \
        --source-ref "refs/tags/$tag" --source-digest "$commit" \
        --deny-self-hosted-runners
    done
  done
done
```

Record all twelve verification results, workflow run URL, tag/SHA, and native
smoke-test coverage in the activation PR. An attestation establishes the build
workflow/source identity and artifact digest; it does not prove the program is
safe. The checksum manifest alone is never signer proof.

If signing or verification fails, no release-creation step runs. If release
creation/upload fails partway, inspect GitHub's state before any recovery:
the guard deliberately refuses an existing release and a blind rerun will
not repair it. Do not delete/recreate releases, move tags, overwrite assets,
or activate the website. Preserve the workflow evidence and require an
explicitly reviewed recovery decision (which may require a new version).
Local/mock tests cannot exercise GitHub OIDC signing or real release uploads;
those remain part of live release validation.

## Website installer commands

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
shellcheck scripts/check-release-source.sh
go test ./scripts/installertest -count=1 -v
TORANA_INSTALLER_SMOKE=1 go test ./scripts/installertest -run NativeBinary -count=1 -v
GOWORK=off go install github.com/goreleaser/goreleaser/v2@v2.15.4
GOWORK=off goreleaser release --snapshot --clean --skip=sbom,publish,announce
TORANA_RELEASE_DIST=../../dist go test ./scripts/installertest -run GeneratedReleaseArtifacts -count=1 -v
TORANA_TEST_GORELEASER="$(command -v goreleaser)" go test ./scripts/installertest -run GoReleaserBuildMetadata -count=1 -v
```

On Windows, run `go test ./scripts/installertest -count=1 -v` with
`TORANA_TEST_POWERSHELL=powershell` and again with
`TORANA_TEST_POWERSHELL=pwsh`. Set `TORANA_INSTALLER_SMOKE=1` to include building
and executing the native binary. Every installation targets a fresh test
directory; neither local checks nor CI modify the user's actual installation.
