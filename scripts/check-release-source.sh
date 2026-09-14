#!/usr/bin/env bash
# Read-only guard for a future tag release. Never creates a tag or release.
set -euo pipefail

fail() { echo "release source rejected: $*" >&2; exit 1; }
[[ ${GITHUB_REPOSITORY:-} == torana-edge/torana-edge ]] || fail "unexpected repository"
[[ ${GITHUB_EVENT_NAME:-} == push && ${GITHUB_REF_TYPE:-} == tag ]] || fail "expected a tag push"
tag=${GITHUB_REF_NAME:?}
number='(0|[1-9][0-9]*)'
identifier='(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)'
semver="^v${number}\\.${number}\\.${number}(-${identifier}(\\.${identifier})*)?(\\+[0-9A-Za-z-]+(\\.[0-9A-Za-z-]+)*)?$"
[[ $tag =~ $semver ]] || fail "tag must be v-prefixed SemVer"
[[ ${GITHUB_REF:-} == "refs/tags/$tag" ]] || fail "ref does not match tag"
[[ ${GITHUB_SHA:-} =~ ^[0-9a-f]{40}$ ]] || fail "invalid source SHA"
head_sha=$(git rev-parse HEAD)
[[ $head_sha == "$GITHUB_SHA" ]] || fail "checkout is not the event commit"
local_tag_sha=$(git rev-parse "refs/tags/$tag^{commit}")
[[ $local_tag_sha == "$GITHUB_SHA" ]] || fail "local tag does not resolve to the event commit"

repo="repos/$GITHUB_REPOSITORY"
encoded_tag=$(jq -rn --arg value "$tag" '$value | @uri')
remote_sha=$(gh api "$repo/commits/refs/tags/$encoded_tag" --jq .sha)
[[ $remote_sha == "$GITHUB_SHA" ]] || fail "remote tag moved or resolves to another commit"
branch=$(gh api "$repo" --jq .default_branch)
[[ -n $branch && $branch != null ]] || fail "missing default branch"
encoded_branch=$(jq -rn --arg value "$branch" '$value | @uri')
branch_sha=$(gh api "$repo/commits/refs/heads/$encoded_branch" --jq .sha)
[[ $branch_sha =~ ^[0-9a-f]{40}$ ]] || fail "invalid default branch commit"
merge_base=$(gh api "$repo/compare/$GITHUB_SHA...$branch_sha" --jq .merge_base_commit.sha)
[[ $merge_base == "$GITHUB_SHA" ]] || fail "tag commit is not on the default branch"

# Listing (including drafts) must succeed: a 403/timeout is not 'no release'.
existing=$(gh api --paginate "$repo/releases?per_page=100" |
  jq -s --arg tag "$tag" '[.[][] | select(.tag_name == $tag)] | length')
[[ $existing == 0 ]] || fail "a release already exists for $tag; do not overwrite or rerun it"
version=${tag#v}
prerelease=false
[[ ${version%%+*} != *-* ]] || prerelease=true
printf 'version=%s\nsha=%s\nprerelease=%s\n' "$version" "$GITHUB_SHA" "$prerelease" >> "${GITHUB_OUTPUT:?}"
