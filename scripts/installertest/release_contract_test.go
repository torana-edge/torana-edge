package installertest

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"text/template"

	"gopkg.in/yaml.v3"
)

// Model the release fields the installers rely on. Unknown build/archive
// options fail closed so a newly introduced override cannot evade this check.
type releaseContract struct {
	Version int `yaml:"version"`
	Builds  []struct {
		ID      string         `yaml:"id"`
		Main    string         `yaml:"main"`
		Binary  string         `yaml:"binary"`
		LDFlags []string       `yaml:"ldflags"`
		GOOS    []string       `yaml:"goos"`
		GOARCH  []string       `yaml:"goarch"`
		Env     []string       `yaml:"env"`
		Other   map[string]any `yaml:",inline"`
	} `yaml:"builds"`
	Archives []struct {
		Formats         []string `yaml:"formats"`
		NameTemplate    string   `yaml:"name_template"`
		WrapInDirectory any      `yaml:"wrap_in_directory"`
		FormatOverrides []struct {
			GOOS    string         `yaml:"goos"`
			Formats []string       `yaml:"formats"`
			Other   map[string]any `yaml:",inline"`
		} `yaml:"format_overrides"`
		Other map[string]any `yaml:",inline"`
	} `yaml:"archives"`
	Checksum struct {
		NameTemplate string         `yaml:"name_template"`
		Algorithm    string         `yaml:"algorithm"`
		Other        map[string]any `yaml:",inline"`
	} `yaml:"checksum"`
	SBOMs []struct {
		Artifacts string         `yaml:"artifacts"`
		Other     map[string]any `yaml:",inline"`
	} `yaml:"sboms"`
}

func readReleaseContract(t *testing.T) releaseContract {
	t.Helper()
	raw, err := os.ReadFile("../../.goreleaser.yaml")
	must(t, err)
	contract, err := parseReleaseContract(raw)
	must(t, err)
	return contract
}

func parseReleaseContract(raw []byte) (releaseContract, error) {
	var c releaseContract
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return c, err
	}
	if c.Version != 2 || len(c.Builds) != 1 || len(c.Archives) != 1 {
		return c, fmt.Errorf("installer contract requires one v2 build/archive group")
	}
	b, a := c.Builds[0], c.Archives[0]
	if len(b.Other) != 0 || len(a.Other) != 0 || len(c.Checksum.Other) != 0 {
		return c, fmt.Errorf("review new release options against the installer contract: build=%v archive=%v checksum=%v", b.Other, a.Other, c.Checksum.Other)
	}
	if b.ID != "torana" || b.Main != "./cmd/torana" || b.Binary != "torana" || !slices.Equal(b.Env, []string{"CGO_ENABLED=0"}) {
		return c, fmt.Errorf("installer requires the standalone CGO-disabled cmd/torana binary named torana")
	}
	osList, archList := slices.Clone(b.GOOS), slices.Clone(b.GOARCH)
	slices.Sort(osList)
	slices.Sort(archList)
	if !slices.Equal(osList, []string{"darwin", "linux", "windows"}) || !slices.Equal(archList, []string{"amd64", "arm64"}) {
		return c, fmt.Errorf("installer promises exactly darwin/linux/windows on amd64/arm64")
	}
	if a.WrapInDirectory != nil && a.WrapInDirectory != false {
		return c, fmt.Errorf("installer requires an unwrapped root binary member")
	}
	if !slices.Equal(a.Formats, []string{"tar.gz"}) || len(a.FormatOverrides) != 1 || a.FormatOverrides[0].GOOS != "windows" || !slices.Equal(a.FormatOverrides[0].Formats, []string{"zip"}) {
		return c, fmt.Errorf("installer requires tar.gz on Unix and zip on Windows")
	}
	if len(a.FormatOverrides[0].Other) != 0 {
		return c, fmt.Errorf("review new archive format override options against installer contract")
	}
	if c.Checksum.NameTemplate != "checksums.txt" || c.Checksum.Algorithm != "" && c.Checksum.Algorithm != "sha256" {
		return c, fmt.Errorf("installer requires one checksums.txt containing SHA-256 digests")
	}
	if len(c.SBOMs) != 1 || c.SBOMs[0].Artifacts != "archive" || len(c.SBOMs[0].Other) != 0 {
		return c, fmt.Errorf("release requires the configured default archive SBOMs")
	}
	for _, version := range []string{"1.2.3", "1.2.3-rc.1", "1.2.3+build.1", "1.2.3-rc.1+build.1"} {
		for _, goos := range b.GOOS {
			for _, arch := range b.GOARCH {
				name, err := c.archiveName(version, goos, arch)
				want := "torana_" + version + "_" + goos + "_" + arch + "." + c.archiveFormat(goos)
				if err != nil || name != want {
					return c, fmt.Errorf("release archive %q does not match installer request %q: %v", name, want, err)
				}
			}
		}
		flags, err := c.ldflags(version)
		if err != nil || !hasVersionFlag(flags, version) {
			return c, fmt.Errorf("release ldflags must inject the unprefixed full version %q: %v", version, err)
		}
	}
	return c, nil
}

func renderReleaseTemplate(value, version, goos, arch string) (string, error) {
	tmpl, err := template.New("release").Option("missingkey=error").Parse(value)
	if err != nil {
		return "", err
	}
	var out bytes.Buffer
	err = tmpl.Execute(&out, map[string]string{"Version": version, "Tag": "v" + version, "Os": goos, "Arch": arch})
	return out.String(), err
}

func (c releaseContract) archiveFormat(goos string) string {
	for _, override := range c.Archives[0].FormatOverrides {
		if override.GOOS == goos {
			return override.Formats[0]
		}
	}
	return c.Archives[0].Formats[0]
}

func (c releaseContract) archiveName(version, goos, arch string) (string, error) {
	name, err := renderReleaseTemplate(c.Archives[0].NameTemplate, version, goos, arch)
	return name + "." + c.archiveFormat(goos), err
}

func (c releaseContract) binaryName(goos string) string {
	name := c.Builds[0].Binary
	if goos == "windows" {
		name += ".exe"
	}
	return name
}

func (c releaseContract) ldflags(version string) (string, error) {
	return renderReleaseTemplate(strings.Join(c.Builds[0].LDFlags, " "), version, "", "")
}

func hasVersionFlag(flags, version string) bool {
	fields := strings.Fields(flags)
	for i, field := range fields {
		if field == "-X" && i+1 < len(fields) && fields[i+1] == "main.version="+version {
			return true
		}
	}
	return false
}

func TestReleaseConfigInstallerContract(t *testing.T) {
	readReleaseContract(t)
}

func TestReleaseConfigRejectsInstallerBreakage(t *testing.T) {
	raw, err := os.ReadFile("../../.goreleaser.yaml")
	must(t, err)
	for _, change := range []struct{ name, from, to string }{
		{"archive-name", "torana_{{.Version}}_{{.Os}}_{{.Arch}}", "edge_{{.Version}}_{{.Os}}_{{.Arch}}"},
		{"prefixed-version", "torana_{{.Version}}_", "torana_{{.Tag}}_"},
		{"binary-name", "binary: torana", "binary: edge"},
		{"wrapped-member", "  - formats:", "  - wrap_in_directory: true\n    formats:"},
		{"checksum-name", "name_template: \"checksums.txt\"", "name_template: \"SHA256SUMS\""},
		{"checksum-algorithm", "checksum:", "checksum:\n  algorithm: sha512"},
		{"windows-format", "formats: [zip]", "formats: [tar.gz]"},
		{"missing-platform", "      - windows\n", ""},
		{"missing-architecture", "      - arm64\n", ""},
		{"build-exclusion", "    goos:", "    ignore:\n      - goos: windows\n    goos:"},
		{"binary-version", "main.version={{.Version}}", "main.version={{.Tag}}"},
		{"disabled-sboms", "  - artifacts: archive", "  - artifacts: archive\n    disable: true"},
	} {
		t.Run(change.name, func(t *testing.T) {
			modified := strings.Replace(string(raw), change.from, change.to, 1)
			if modified == string(raw) {
				t.Fatal("mutation did not change release config")
			}
			if _, err := parseReleaseContract([]byte(modified)); err == nil {
				t.Fatal("installer-breaking release configuration passed")
			}
		})
	}
}

// The existing GoReleaser dry run supplies this directory. Validate its actual
// six archives as well as the synthetic fixtures, without publishing anything.
func TestGeneratedReleaseArtifacts(t *testing.T) {
	dist := os.Getenv("TORANA_RELEASE_DIST")
	if dist == "" {
		t.Skip("set TORANA_RELEASE_DIST to inspect generated GoReleaser artifacts")
	}
	c := readReleaseContract(t)
	validateGeneratedReleaseArtifacts(t, dist, c, os.Getenv("TORANA_RELEASE_VERSION"))
}

func validateGeneratedReleaseArtifacts(t *testing.T, dist string, c releaseContract, version string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dist, "metadata.json"))
	must(t, err)
	var metadata struct {
		Version string `json:"version"`
	}
	must(t, json.Unmarshal(raw, &metadata))
	if metadata.Version == "" {
		t.Fatal("GoReleaser metadata has no version")
	}
	if version != "" && metadata.Version != version {
		t.Fatalf("generated version %q does not match required version %q", metadata.Version, version)
	}
	checksums, err := os.ReadFile(filepath.Join(dist, c.Checksum.NameTemplate))
	must(t, err)
	expected := map[string]bool{}
	for _, goos := range c.Builds[0].GOOS {
		for _, arch := range c.Builds[0].GOARCH {
			name, err := c.archiveName(metadata.Version, goos, arch)
			must(t, err)
			expected[name] = true
			expected[name+".sbom.json"] = os.Getenv("TORANA_RELEASE_REQUIRE_SBOM") == "1"
		}
	}
	manifest, err := parseReleaseChecksums(string(checksums), expected)
	must(t, err)
	for name, digest := range manifest {
		path := filepath.Join(dist, name)
		info, err := os.Lstat(path)
		must(t, err)
		if !info.Mode().IsRegular() || info.Size() == 0 {
			t.Fatalf("release asset %s must be a nonempty regular file", name)
		}
		data, err := os.ReadFile(path)
		must(t, err)
		if fmt.Sprintf("%x", sha256.Sum256(data)) != digest {
			t.Fatalf("checksum mismatch for %s", name)
		}
		if strings.HasSuffix(name, ".sbom.json") && !json.Valid(data) {
			t.Fatalf("invalid SBOM JSON: %s", name)
		}
	}
	for _, goos := range c.Builds[0].GOOS {
		for _, arch := range c.Builds[0].GOARCH {
			t.Run(goos+"/"+arch, func(t *testing.T) {
				name, err := c.archiveName(metadata.Version, goos, arch)
				must(t, err)
				archive, err := os.ReadFile(filepath.Join(dist, name))
				must(t, err)
				binary := rootReleaseBinary(t, archive, c.archiveFormat(goos), c.binaryName(goos))
				info, err := buildinfo.Read(bytes.NewReader(binary))
				must(t, err)
				settings := map[string]string{}
				for _, setting := range info.Settings {
					settings[setting.Key] = setting.Value
				}
				if !slices.Equal([]string{settings["GOOS"], settings["GOARCH"], settings["CGO_ENABLED"]}, []string{goos, arch, "0"}) {
					t.Fatalf("archive has wrong binary target: %v", settings)
				}
				if !hasVersionFlag(settings["-ldflags"], metadata.Version) {
					t.Fatalf("binary was not built with version %q", metadata.Version)
				}
			})
		}
	}
}

func parseReleaseChecksums(raw string, expected map[string]bool) (map[string]string, error) {
	// The workflow's read loops require a complete final line. Fail closed
	// rather than silently omit a last asset from verification/publication.
	if !strings.HasSuffix(raw, "\n") {
		return nil, fmt.Errorf("checksum manifest must end with a newline")
	}
	manifest := map[string]string{}
	linePattern := regexp.MustCompile(`^([0-9a-f]{64})  ([A-Za-z0-9_.+-]+)$`)
	for _, line := range strings.Split(strings.TrimSuffix(raw, "\n"), "\n") {
		match := linePattern.FindStringSubmatch(line)
		if match == nil {
			return nil, fmt.Errorf("malformed checksum entry %q", line)
		}
		digest, name := match[1], match[2]
		if _, ok := expected[name]; !ok {
			return nil, fmt.Errorf("unexpected release asset %q", name)
		}
		if _, duplicate := manifest[name]; duplicate {
			return nil, fmt.Errorf("duplicate checksum for %q", name)
		}
		manifest[name] = digest
	}
	for name, required := range expected {
		if required && manifest[name] == "" {
			return nil, fmt.Errorf("missing release asset %q", name)
		}
	}
	return manifest, nil
}

func TestReleaseManifestFailsClosed(t *testing.T) {
	hash := strings.Repeat("a", 64)
	valid := hash + "  torana_1.2.3_linux_amd64.tar.gz\n"
	expected := map[string]bool{"torana_1.2.3_linux_amd64.tar.gz": true}
	_, err := parseReleaseChecksums(valid, expected)
	must(t, err)
	for _, raw := range []string{"", strings.TrimSuffix(valid, "\n"), valid + valid, valid + "\n", strings.Replace(valid, hash, "bad", 1), strings.Replace(valid, "  torana", " *torana", 1), strings.Replace(valid, "  torana", "  ../torana", 1), strings.Replace(valid, "linux", "darwin", 1), strings.TrimSpace(valid) + " extra\n"} {
		if _, err := parseReleaseChecksums(raw, expected); err == nil {
			t.Fatalf("invalid manifest accepted: %q", raw)
		}
	}
	expected["torana_1.2.3_linux_amd64.tar.gz.sbom.json"] = true
	if _, err := parseReleaseChecksums(valid, expected); err == nil {
		t.Fatal("missing required SBOM accepted")
	}
}

// Exercise GoReleaser's real tag parser, templates, packaging and linker flags.
// The only tag created is in a disposable fixture repository. The synthetic
// remote URL is configuration for GoReleaser, never fetched from or pushed to.
// Publishing, announcements and SBOM scanning are explicitly disabled.
func TestGoReleaserBuildMetadata(t *testing.T) {
	tool := os.Getenv("TORANA_TEST_GORELEASER")
	if tool == "" {
		t.Skip("set TORANA_TEST_GORELEASER to the pinned GoReleaser binary")
	}
	c := readReleaseContract(t)
	root := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(root, "cmd", "torana"), 0o700))
	raw, err := os.ReadFile("../../.goreleaser.yaml")
	must(t, err)
	must(t, os.WriteFile(filepath.Join(root, ".goreleaser.yaml"), raw, 0o600))
	must(t, os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/release-contract\n\ngo 1.26.6\n"), 0o600))
	must(t, os.WriteFile(filepath.Join(root, "cmd", "torana", "main.go"), []byte("package main\nimport \"fmt\"\nvar version string\nfunc main() { fmt.Println(version) }\n"), 0o600))
	run := func(name string, args ...string) string {
		t.Helper()
		cmd := exec.Command(name, args...)
		cmd.Dir = root
		cmd.Env = overrideEnv(os.Environ(), map[string]string{
			"GOWORK": "off", "GITHUB_TOKEN": "", "GH_TOKEN": "", "GITHUB_REF": "", "GITHUB_SHA": "",
			"GIT_AUTHOR_NAME": "Release contract", "GIT_AUTHOR_EMAIL": "test@example.invalid",
			"GIT_COMMITTER_NAME": "Release contract", "GIT_COMMITTER_EMAIL": "test@example.invalid",
		})
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s %v: %v\n%s", name, args, err, output)
		}
		return string(output)
	}
	run("git", "init", "--initial-branch=main")
	run("git", "remote", "add", "origin", "https://github.com/example/release-contract-fixture.git")
	run("git", "add", ".")
	run("git", "-c", "commit.gpgsign=false", "commit", "-m", "isolated release contract fixture")
	version := "1.2.3-rc.1+build.1"
	run("git", "-c", "tag.gpgsign=false", "tag", "v"+version)
	run(tool, "release", "--clean", "--skip=sbom,publish,announce")
	dist := filepath.Join(root, "dist")
	validateGeneratedReleaseArtifacts(t, dist, c, version)
	name, err := c.archiveName(version, runtime.GOOS, runtime.GOARCH)
	must(t, err)
	archive, err := os.ReadFile(filepath.Join(dist, name))
	must(t, err)
	binary := rootReleaseBinary(t, archive, c.archiveFormat(runtime.GOOS), c.binaryName(runtime.GOOS))
	path := filepath.Join(root, c.binaryName(runtime.GOOS))
	must(t, os.WriteFile(path, binary, 0o700))
	if got := strings.TrimSpace(run(path)); got != version {
		t.Fatalf("built version = %q, want %q", got, version)
	}
}

func rootReleaseBinary(t *testing.T, archive []byte, format, name string) []byte {
	t.Helper()
	var binary []byte
	matches := 0
	if format == "zip" {
		reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
		must(t, err)
		for _, member := range reader.File {
			if member.Name != name {
				continue
			}
			matches++
			if !member.Mode().IsRegular() {
				t.Fatal("root binary is not a regular file")
			}
			r, err := member.Open()
			must(t, err)
			binary, err = io.ReadAll(r)
			must(t, err)
			must(t, r.Close())
		}
	} else {
		compressed, err := gzip.NewReader(bytes.NewReader(archive))
		must(t, err)
		defer compressed.Close()
		reader := tar.NewReader(compressed)
		for {
			member, err := reader.Next()
			if err == io.EOF {
				break
			}
			must(t, err)
			if member.Name != name {
				continue
			}
			matches++
			if member.Typeflag != tar.TypeReg {
				t.Fatal("root binary is not a regular file")
			}
			binary, err = io.ReadAll(reader)
			must(t, err)
		}
	}
	if matches != 1 || len(binary) == 0 {
		t.Fatalf("expected one nonempty root %s binary, got %d", name, matches)
	}
	return binary
}
