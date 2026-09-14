package installertest

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestReleaseSourceGuard(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("release guard runs only on the Ubuntu release runner")
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Fatal("jq is required for the release guard test")
	}
	const sha = "1111111111111111111111111111111111111111"
	const other = "2222222222222222222222222222222222222222"
	for _, tc := range []struct {
		name       string
		env        map[string]string
		want       bool
		prerelease bool
	}{
		{"stable", nil, true, false},
		{"build-metadata", map[string]string{"GITHUB_REF_NAME": "v1.2.3+build.1-linux"}, true, false},
		{"prerelease-build", map[string]string{"GITHUB_REF_NAME": "v1.2.3-rc.1+build.1"}, true, true},
		{"branch-tag-name-collision", map[string]string{"TEST_BRANCH": "v1.2.3"}, true, false},
		{"slash-default-branch", map[string]string{"TEST_BRANCH": "release/main"}, true, false},
		{"unprefixed-tag", map[string]string{"GITHUB_REF_NAME": "1.2.3"}, false, false},
		{"invalid-semver", map[string]string{"GITHUB_REF_NAME": "v01.2.3"}, false, false},
		{"numeric-prerelease-leading-zero", map[string]string{"GITHUB_REF_NAME": "v1.2.3-01"}, false, false},
		{"invalid-tag-suffix", map[string]string{"GITHUB_REF_NAME": "v1.2.3+"}, false, false},
		{"other-repository", map[string]string{"GITHUB_REPOSITORY": "example/torana-edge"}, false, false},
		{"manual-dispatch", map[string]string{"GITHUB_EVENT_NAME": "workflow_dispatch"}, false, false},
		{"branch-event", map[string]string{"GITHUB_REF_TYPE": "branch"}, false, false},
		{"ref-mismatch", map[string]string{"GITHUB_REF": "refs/heads/v1.2.3"}, false, false},
		{"malformed-sha", map[string]string{"GITHUB_SHA": "main"}, false, false},
		{"wrong-checkout", map[string]string{"TEST_HEAD_SHA": other}, false, false},
		{"git-failure-with-valid-output", map[string]string{"TEST_GIT_FAIL": "1"}, false, false},
		{"wrong-local-tag", map[string]string{"TEST_LOCAL_TAG_SHA": other}, false, false},
		{"moved-remote-tag", map[string]string{"TEST_REMOTE_SHA": other}, false, false},
		{"remote-api-failure", map[string]string{"TEST_API_FAIL": "tag"}, false, false},
		{"branch-api-failure", map[string]string{"TEST_API_FAIL": "branch"}, false, false},
		{"invalid-default-sha", map[string]string{"TEST_BRANCH_SHA": "null"}, false, false},
		{"missing-default-branch", map[string]string{"TEST_BRANCH": "null"}, false, false},
		{"unmerged-tag", map[string]string{"TEST_MERGE_BASE": other}, false, false},
		{"compare-api-failure", map[string]string{"TEST_API_FAIL": "compare"}, false, false},
		{"existing-release", map[string]string{"TEST_RELEASES": `[{"tag_name":"v1.2.3"}]`}, false, false},
		{"existing-draft-on-later-page", map[string]string{"TEST_RELEASES": "[]\n[{\"tag_name\":\"v1.2.3\",\"draft\":true}]"}, false, false},
		{"release-api-partial-failure", map[string]string{"TEST_API_FAIL": "releases"}, false, false},
		{"malformed-release-response", map[string]string{"TEST_RELEASES": "not JSON"}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			must(t, os.WriteFile(filepath.Join(root, "git"), []byte(`#!/usr/bin/env bash
set -euo pipefail
case "$*" in
  'rev-parse HEAD') echo "$TEST_HEAD_SHA" ;;
  "rev-parse refs/tags/$GITHUB_REF_NAME^{commit}") echo "$TEST_LOCAL_TAG_SHA" ;;
  *) echo "unexpected git call: $*" >&2; exit 99 ;;
esac
[[ $TEST_GIT_FAIL != 1 ]]
`), 0o700))
			must(t, os.WriteFile(filepath.Join(root, "gh"), []byte(`#!/usr/bin/env bash
set -euo pipefail
repo=repos/torana-edge/torana-edge
tag=$(jq -rn --arg x "$GITHUB_REF_NAME" '$x|@uri')
branch=$(jq -rn --arg x "$TEST_BRANCH" '$x|@uri')
case "$*" in
  "api $repo --jq .default_branch") echo "$TEST_BRANCH"; kind=repo ;;
  "api $repo/commits/refs/tags/$tag --jq .sha") echo "$TEST_REMOTE_SHA"; kind=tag ;;
  "api $repo/commits/refs/heads/$branch --jq .sha") echo "$TEST_BRANCH_SHA"; kind=branch ;;
  "api $repo/compare/$GITHUB_SHA...$TEST_BRANCH_SHA --jq .merge_base_commit.sha") echo "$TEST_MERGE_BASE"; kind=compare ;;
  "api --paginate $repo/releases?per_page=100") echo "$TEST_RELEASES"; kind=releases ;;
  *) echo "unexpected/unqualified API call: $*" >&2; exit 99 ;;
esac
[[ $TEST_API_FAIL != "$kind" ]]
`), 0o700))
			env := map[string]string{
				"PATH":     root + string(os.PathListSeparator) + os.Getenv("PATH"),
				"GH_TOKEN": "", "GITHUB_TOKEN": "", "GITHUB_REPOSITORY": "torana-edge/torana-edge",
				"GITHUB_EVENT_NAME": "push", "GITHUB_REF_TYPE": "tag", "GITHUB_REF_NAME": "v1.2.3", "GITHUB_SHA": sha,
				"GITHUB_OUTPUT": filepath.Join(root, "output"), "TEST_HEAD_SHA": sha, "TEST_LOCAL_TAG_SHA": sha,
				"TEST_REMOTE_SHA": sha, "TEST_BRANCH": "main", "TEST_BRANCH_SHA": other, "TEST_MERGE_BASE": sha,
				"TEST_RELEASES": "[]", "TEST_API_FAIL": "", "TEST_GIT_FAIL": "",
			}
			for key, value := range tc.env {
				env[key] = value
			}
			if _, ok := env["GITHUB_REF"]; !ok {
				env["GITHUB_REF"] = "refs/tags/" + env["GITHUB_REF_NAME"]
			}
			cmd := exec.Command("bash", "../check-release-source.sh")
			cmd.Env = overrideEnv(os.Environ(), env)
			output, err := cmd.CombinedOutput()
			if (err == nil) != tc.want {
				t.Fatalf("guard success=%v, want %v: %s", err == nil, tc.want, output)
			}
			result, _ := os.ReadFile(env["GITHUB_OUTPUT"])
			if !tc.want && len(result) != 0 {
				t.Fatal("rejected source emitted release outputs")
			}
			if tc.want {
				pre := "false"
				if tc.prerelease {
					pre = "true"
				}
				want := "version=" + strings.TrimPrefix(env["GITHUB_REF_NAME"], "v") + "\nsha=" + sha + "\nprerelease=" + pre + "\n"
				if string(result) != want {
					t.Fatalf("outputs=%q, want %q", result, want)
				}
			}
		})
	}
}

func TestReleaseWorkflowSafetyContract(t *testing.T) {
	raw, err := os.ReadFile("../../.github/workflows/release.yml")
	must(t, err)
	var workflow struct {
		On          map[string]any    `yaml:"on"`
		Permissions map[string]string `yaml:"permissions"`
		Env         map[string]string `yaml:"env"`
		Jobs        map[string]struct {
			Permissions map[string]string `yaml:"permissions"`
			Env         map[string]string `yaml:"env"`
			Steps       []struct {
				Name, Uses, Run string
				With, Env       map[string]string
			}
		} `yaml:"jobs"`
	}
	must(t, yaml.Unmarshal(raw, &workflow))
	if len(workflow.On) != 1 || workflow.On["push"] == nil || len(workflow.Permissions) != 0 || len(workflow.Jobs) != 1 {
		t.Fatal("release must be tag-push-only with job-scoped permissions")
	}
	push, ok := workflow.On["push"].(map[string]any)
	if !ok || len(push) != 1 {
		t.Fatal("unexpected push trigger")
	}
	tags, ok := push["tags"].([]any)
	if !ok || len(tags) != 1 || tags[0] != "v*" {
		t.Fatal("release trigger must match v-prefixed tags only")
	}
	job := workflow.Jobs["release"]
	if len(job.Permissions) != 3 || job.Permissions["contents"] != "write" || job.Permissions["id-token"] != "write" || job.Permissions["attestations"] != "write" {
		t.Fatal("unexpected release permissions")
	}
	steps := map[string]int{}
	for i, step := range job.Steps {
		steps[step.Name] = i
	}
	ordered := []string{"Require new tag on default branch", "Build release assets without publishing", "Validate all six generated installer assets", "Attest release artifact provenance", "Verify signed artifacts before publication", "Recheck source and create a new release"}
	previous := -1
	for _, name := range ordered {
		i, ok := steps[name]
		if !ok || i <= previous {
			t.Fatalf("missing/out-of-order gate: %s", name)
		}
		previous = i
	}
	build := job.Steps[steps[ordered[1]]]
	must(t, validateReleaseBuildEnv(workflow.Env, job.Env, build.Env))
	if !strings.Contains(build.Run, "@v2.15.4 release --clean --skip=publish,announce") || strings.Contains(build.Run, "--snapshot") {
		t.Fatal("real release must build tagged assets without early publishing or skipping SBOMs")
	}
	validate := job.Steps[steps[ordered[2]]]
	if validate.Env["TORANA_RELEASE_REQUIRE_SBOM"] != "1" || validate.Env["TORANA_RELEASE_VERSION"] != "${{ steps.source.outputs.version }}" || !strings.Contains(validate.Run, "TestGeneratedReleaseArtifacts") {
		t.Fatal("release asset validation must be mandatory and tag-bound")
	}
	attest := job.Steps[steps[ordered[3]]]
	if !strings.HasPrefix(attest.Uses, "actions/attest@") || attest.With["subject-checksums"] != "dist/checksums.txt" || attest.With["create-storage-record"] != "false" {
		t.Fatal("unexpected provenance configuration")
	}
	verify := job.Steps[steps[ordered[4]]].Run
	for _, required := range []string{"set -euo pipefail", "gh attestation verify", "--bundle dist/provenance.sigstore.json", "--repo torana-edge/torana-edge", "--signer-workflow torana-edge/torana-edge/.github/workflows/release.yml", `--source-ref "$GITHUB_REF" --source-digest "$GITHUB_SHA"`, "--deny-self-hosted-runners", "done < dist/checksums.txt"} {
		if !strings.Contains(verify, required) {
			t.Fatalf("missing verification policy: %s", required)
		}
	}
	publish := job.Steps[steps[ordered[5]]].Run
	for _, required := range []string{"set -euo pipefail", "bash scripts/check-release-source.sh", "gh release create", "--verify-tag", "dist/provenance.sigstore.json", "done < dist/checksums.txt"} {
		if !strings.Contains(publish, required) {
			t.Fatalf("missing publish safeguard: %s", required)
		}
	}
	if strings.Contains(publish, "--clobber") || strings.Contains(publish, "gh release delete") {
		t.Fatal("release must never overwrite existing artifacts")
	}
}

// A release job needs a write-capable token later, but the downloaded build
// tool and any future build hooks must not inherit it through env scopes.
func validateReleaseBuildEnv(workflowEnv, jobEnv, buildEnv map[string]string) error {
	if len(workflowEnv) != 0 || len(buildEnv) != 0 || len(jobEnv) != 1 || jobEnv["GOWORK"] != "off" {
		return fmt.Errorf("release builder environment must contain only GOWORK=off; scope write tokens to API/publication steps")
	}
	return nil
}

func TestReleaseBuilderRejectsInheritedCredentials(t *testing.T) {
	must(t, validateReleaseBuildEnv(nil, map[string]string{"GOWORK": "off"}, nil))
	for _, scope := range []string{"workflow", "job", "build"} {
		for _, name := range []string{"GITHUB_TOKEN", "GH_TOKEN", "CUSTOM_WRITE_CREDENTIAL"} {
			t.Run(scope+"/"+name, func(t *testing.T) {
				envs := map[string]map[string]string{"workflow": {}, "job": {"GOWORK": "off"}, "build": {}}
				envs[scope][name] = "${{ github.token }}"
				if err := validateReleaseBuildEnv(envs["workflow"], envs["job"], envs["build"]); err == nil {
					t.Fatal("builder inherited a write credential")
				}
			})
		}
	}
}
